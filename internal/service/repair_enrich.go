package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// The enrichment stage of a repair.
//
// This is the half that makes a degraded write non-destructive. A vectorless
// write silently skips every write-time job that needs a query vector — the
// dedup gate, corroboration routing, contradiction routing, and consolidation —
// and the old backfill deliberately did not re-run them, so that enrichment was
// lost permanently rather than deferred. The result was that an embedder blip
// left duplicate and contradictory memories that nothing ever reconciled.
//
// Here the same jobs run against the now-vectored memory, in the same order
// Remember runs them, with one unavoidable difference: the memory is already
// stored. Where a healthy write's dedup can fold itself into an existing memory
// before anything persists, a repair has to retire a row the caller was already
// told about. It does that by superseding rather than deleting, so the ID the
// caller holds still resolves to the surviving fact (see repairDedup).
//
// The one thing that cannot be recovered is the merge hint: it is a field on a
// synchronous response that was sent before this ran. The activity log records
// the merge instead.

// repairEnrich replays a degraded write's missing write-time enrichment and
// clears the repair state.
func (s *Service) repairEnrich(ctx context.Context, rs store.RepairStore, row store.RepairRow) error {
	m, err := s.store.Get(ctx, row.Namespace, row.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errRepairMoot
		}
		return err
	}
	if m.SupersededBy != nil {
		// Something already replaced it; enriching a tombstone would only
		// disturb rows this repair has no business touching.
		return errRepairMoot
	}
	// Get omits the vector from every read path, so load it explicitly. Doing
	// this through GetEmbedding rather than off the loaded memory is the same
	// rule that keeps a Get-then-Upsert round trip from silently dropping it.
	vec, err := s.store.GetEmbedding(ctx, row.Namespace, row.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errRepairMoot
		}
		return err
	}
	if len(vec) == 0 {
		// The vector went away between stages (a content edit that met a dead
		// embedder). Send it back to the embed stage rather than enriching
		// something with no query vector.
		if ok, serr := rs.SetRepairState(ctx, row.Namespace, row.ID, row.Fingerprint, store.RepairPending); serr != nil {
			return serr
		} else if !ok {
			return errRepairMoot
		}
		return nil
	}
	m.Embedding = vec

	// Same order as Remember's write-time tail, so a repaired write converges on
	// the same corpus state a healthy one would have produced.
	superseded := s.repairDedup(ctx, m)
	if superseded {
		// This memory folded into an existing one. It is retired, so the
		// remaining jobs would be operating on a tombstone.
		return s.clearRepair(ctx, rs, row)
	}
	s.repairCorroborate(ctx, m)
	s.repairContradict(ctx, m)
	if s.consolidator != nil && s.consolidateMode != ConsolidateOff && m.Tier.Term() == memory.LongTerm {
		s.enqueueConsolidate(m.Namespace, m.ID)
	}
	return s.clearRepair(ctx, rs, row)
}

// clearRepair marks a row as owing nothing further.
func (s *Service) clearRepair(ctx context.Context, rs store.RepairStore, row store.RepairRow) error {
	ok, err := rs.SetRepairState(ctx, row.Namespace, row.ID, row.Fingerprint, store.RepairNone)
	if err != nil {
		return err
	}
	if !ok {
		// The content moved under us; whoever moved it owns the state now.
		return errRepairMoot
	}
	return nil
}

// repairDedup replays the write-time dedup gate against a now-vectored memory,
// reporting whether m was retired into an existing near-duplicate.
//
// The coalesce branch is the one place a repair cannot be literally identical
// to a healthy write. A healthy write coalesces *before* storing, so nothing new
// is ever created; here the row exists and the caller has its ID. Deleting it
// would turn a reported success into a 404, so it is superseded instead: memini
// already resolves supersession chains, so the caller's ID still leads to the
// surviving fact, and the activity log records the transition.
func (s *Service) repairDedup(ctx context.Context, m *memory.Memory) bool {
	if s.writeDedupScore <= 0 || s.writeDedupAction == WriteDedupOff || s.writeDedupAction == "" {
		return false
	}
	hit, _, supersedeID := s.dedupCheck(ctx, m)
	switch {
	case hit != nil:
		// Coalesce: fold this memory into the one it restates.
		s.reinforce(ctx, []store.Scored{{Memory: hit}})
		s.corroborate(ctx, hit)
		if err := s.store.SetSuperseded(ctx, m.Namespace, m.ID, hit.ID); err != nil {
			slog.WarnContext(ctx, "repair: superseding a coalesced duplicate failed, leaving both live",
				"namespace", m.Namespace, "id", m.ID, "into", hit.ID, "err", err)
			return false
		}
		slog.InfoContext(ctx, "repair: folded a degraded write into the memory it restates",
			"namespace", m.Namespace, "id", m.ID, "into", hit.ID)
		s.metrics.RepairResult(string(store.RepairEnrich), "coalesced")
		return true
	case supersedeID != "":
		// Supersede: this memory replaces an older near-duplicate. Same
		// direction as a healthy write's auto-supersede.
		done := false
		s.autoSupersede(m.Namespace, supersedeID, m.ID, &done)
		s.metrics.RepairResult(string(store.RepairEnrich), "superseded")
		return false
	}
	return false
}

// repairCorroborate replays corroboration routing. corroborateNearestAsync
// already detaches and already no-ops on anything but a fresh short-term write
// with a vector, so this is the same call the write path makes — the only
// reason it was skipped originally is that the memory had no vector.
func (s *Service) repairCorroborate(ctx context.Context, m *memory.Memory) {
	s.corroborateNearestAsync(ctx, m, true)
}

// repairContradict replays contradiction routing, for the same reason.
func (s *Service) repairContradict(ctx context.Context, m *memory.Memory) {
	s.contradictNearestAsync(ctx, m, true)
}
