package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// consolidateCandidates is how many near-neighbours are offered to the LLM when
// deciding whether a new memory is novel, a refinement, or a contradiction.
const consolidateCandidates = 5

// Async-consolidation tuning.
const (
	consolidateQueueCap     = 1024
	consolidateDrainTimeout = 30 * time.Second
)

// ConsolidateMode selects how the opt-in LLM consolidation pipeline runs.
type ConsolidateMode string

const (
	// ConsolidateAsync stores writes immediately and consolidates in the
	// background — writes never block on the LLM. The default.
	ConsolidateAsync ConsolidateMode = "async"
	// ConsolidateSync consolidates before returning, so a write reflects its
	// dedup/supersede outcome immediately (read-your-consolidated-writes).
	ConsolidateSync ConsolidateMode = "sync"
	// ConsolidateOff disables consolidation even when a consolidator is set.
	ConsolidateOff ConsolidateMode = "off"
)

// consolidateJob identifies an already-stored memory awaiting background
// consolidation.
type consolidateJob struct {
	namespace string
	id        string
}

// StartConsolidator runs the background consolidation worker until ctx is
// cancelled, then drains queued jobs within a bounded timeout. It is a no-op
// unless the service was built with a consolidator in async mode. Call once,
// typically in its own goroutine.
func (s *Service) StartConsolidator(ctx context.Context) {
	if s.consolidateQueue == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			s.drainConsolidate()
			return
		case job := <-s.consolidateQueue:
			s.metrics.ConsolidateQueueDepth(len(s.consolidateQueue))
			s.consolidateOne(context.WithoutCancel(ctx), job)
		}
	}
}

// drainConsolidate processes any remaining queued jobs after shutdown, bounded
// by consolidateDrainTimeout.
func (s *Service) drainConsolidate() {
	ctx, cancel := context.WithTimeout(context.Background(), consolidateDrainTimeout)
	defer cancel()
	for {
		select {
		case job := <-s.consolidateQueue:
			s.consolidateOne(ctx, job)
			if ctx.Err() != nil {
				return
			}
		default:
			return
		}
	}
}

// enqueueConsolidate queues a stored memory for background consolidation,
// dropping the job (best-effort) if the queue is saturated.
func (s *Service) enqueueConsolidate(namespace, id string) {
	if s.consolidateQueue == nil {
		return
	}
	select {
	case s.consolidateQueue <- consolidateJob{namespace: namespace, id: id}:
		s.metrics.ConsolidateQueueDepth(len(s.consolidateQueue))
	default:
		slog.Warn("consolidation queue full, dropping job", "namespace", namespace, "id", id)
		s.metrics.ConsolidateResult("dropped")
	}
}

// candidates returns the near-neighbour durable memories offered to the LLM for
// consolidation, optionally excluding excludeID (the memory itself, once stored).
func (s *Service) candidates(ctx context.Context, m *memory.Memory, excludeID string) ([]store.Scored, error) {
	limit := consolidateCandidates
	if excludeID != "" {
		limit++ // room to drop the self-match
	}
	cands, err := s.store.VectorSearch(ctx, m.Namespace, m.Embedding,
		store.Filter{Tiers: []memory.Tier{memory.TierSemantic, memory.TierProcedural}, Now: s.now()}, limit)
	if err != nil {
		return nil, err
	}
	out := make([]store.Scored, 0, len(cands))
	for _, c := range cands {
		if c.Memory.ID == excludeID {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// gated reports whether the candidate set is too dissimilar to be worth an LLM
// call. cands must be best-first.
func (s *Service) gated(cands []store.Scored) bool {
	return len(cands) == 0 || cands[0].Score < s.consolidateMinScore
}

// askConsolidator builds the LLM input from cands and returns its decision. A
// transient LLM error yields ok=false so the caller can fall back gracefully.
func (s *Service) askConsolidator(ctx context.Context, m *memory.Memory, cands []store.Scored) (llm.Decision, bool) {
	in := llm.Input{New: m.Content, Tier: string(m.Tier)}
	for _, c := range cands {
		in.Candidates = append(in.Candidates, llm.Candidate{ID: c.Memory.ID, Content: c.Memory.Content})
	}
	dec, err := s.consolidator.Consolidate(ctx, in)
	if err != nil {
		// LLM is best-effort; a transient failure must not lose the write.
		slog.WarnContext(ctx, "consolidation failed, storing raw", "err", err)
		s.metrics.ConsolidateResult("error")
		return llm.Decision{}, false
	}
	return dec, true
}

// consolidateSync runs the LLM pipeline for a new durable memory m that has not
// yet been stored. It returns (result, true, nil) when it fully handles the
// write (update/supersede), or (nil, false, nil) to fall through to a normal
// insert.
func (s *Service) consolidateSync(ctx context.Context, m *memory.Memory) (*memory.Memory, bool, error) {
	cands, err := s.candidates(ctx, m, "")
	if err != nil {
		return nil, false, fmt.Errorf("remember: consolidate search: %w", err)
	}
	if s.gated(cands) {
		s.metrics.ConsolidateResult("gated")
		return nil, false, nil
	}
	dec, ok := s.askConsolidator(ctx, m, cands)
	if !ok {
		return nil, false, nil
	}

	switch dec.Action {
	case llm.ActionUpdate:
		return s.applyUpdate(ctx, m, dec)
	case llm.ActionSupersede:
		return s.applySupersede(ctx, m, dec)
	default: // ActionNew
		s.metrics.ConsolidateResult("new")
		return nil, false, nil
	}
}

// applyUpdate merges the new memory into an existing target, re-embedding the
// merged content. Falls through to a normal insert if the target is gone.
func (s *Service) applyUpdate(ctx context.Context, m *memory.Memory, dec llm.Decision) (*memory.Memory, bool, error) {
	if dec.Target == "" {
		s.metrics.ConsolidateResult("noop")
		return nil, false, nil
	}
	target, err := s.store.Get(ctx, m.Namespace, dec.Target)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	content := dec.Content
	if content == "" {
		content = m.Content
	}
	vec, err := embed.EmbedOne(ctx, s.embedder, content)
	if err != nil {
		return nil, false, fmt.Errorf("remember: re-embed merged memory: %w", err)
	}
	target.Content = content
	if dec.Summary != "" {
		target.Summary = dec.Summary
	}
	target.UpdatedAt = s.now()
	target.Embedding = vec
	if err := s.store.Upsert(ctx, target); err != nil {
		return nil, false, err
	}
	s.metrics.ConsolidateResult("update")
	return target, true, nil
}

// applySupersede stores the new memory and tombstones the contradicted target.
func (s *Service) applySupersede(ctx context.Context, m *memory.Memory, dec llm.Decision) (*memory.Memory, bool, error) {
	if err := s.store.Upsert(ctx, m); err != nil {
		return nil, false, err
	}
	if dec.Target != "" {
		if err := s.store.SetSuperseded(ctx, m.Namespace, dec.Target, m.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			// Best-effort rollback: remove the new memory so we don't leave both
			// live. A contradiction has different content, so fsck's duplicate
			// detection won't flag the pair — a failed rollback is the one
			// moment we know contradictory state was left behind.
			if delErr := s.store.Delete(ctx, m.Namespace, m.ID); delErr != nil && !errors.Is(delErr, store.ErrNotFound) {
				slog.WarnContext(ctx, "consolidate: supersede rollback failed; contradictory memories left live",
					"namespace", m.Namespace, "new_id", m.ID, "target_id", dec.Target, "err", delErr)
			}
			return nil, false, err
		}
	}
	s.metrics.ConsolidateResult("supersede")
	return m, true, nil
}

// consolidateOne runs the consolidation pipeline for an already-stored memory
// (the async path). Because the memory exists, update/supersede operate
// relative to it: an update merges into the target and deletes this record;
// a supersede tombstones the target pointing at this record.
func (s *Service) consolidateOne(ctx context.Context, job consolidateJob) {
	m, err := s.store.Get(ctx, job.namespace, job.id)
	if errors.Is(err, store.ErrNotFound) {
		return // deleted before we got to it
	}
	if err != nil {
		slog.WarnContext(ctx, "consolidate: get memory", "err", err)
		s.metrics.ConsolidateResult("error")
		return
	}
	if m.SupersededBy != nil {
		return // already tombstoned by another path
	}
	// Get omits the embedding; re-embed to search for candidates. The embedder
	// cache makes this near-free (the vector was just computed on the write).
	if m.Embedding, err = embed.EmbedOne(ctx, s.embedder, m.Content); err != nil {
		slog.WarnContext(ctx, "consolidate: re-embed", "err", err)
		s.metrics.ConsolidateResult("error")
		return
	}

	cands, err := s.candidates(ctx, m, m.ID)
	if err != nil {
		slog.WarnContext(ctx, "consolidate: search", "err", err)
		s.metrics.ConsolidateResult("error")
		return
	}
	if s.gated(cands) {
		s.metrics.ConsolidateResult("gated")
		return
	}
	dec, ok := s.askConsolidator(ctx, m, cands)
	if !ok {
		return
	}

	switch dec.Action {
	case llm.ActionUpdate:
		s.asyncUpdate(ctx, m, dec)
	case llm.ActionSupersede:
		s.asyncSupersede(ctx, m, dec)
	default: // ActionNew
		s.metrics.ConsolidateResult("new")
	}
}

// asyncUpdate merges the stored memory m into target, re-embeds, persists the
// target, and deletes m (the merged result now lives in target).
func (s *Service) asyncUpdate(ctx context.Context, m *memory.Memory, dec llm.Decision) {
	if dec.Target == "" || dec.Target == m.ID {
		s.metrics.ConsolidateResult("noop")
		return
	}
	target, err := s.store.Get(ctx, m.Namespace, dec.Target)
	if errors.Is(err, store.ErrNotFound) {
		s.metrics.ConsolidateResult("noop")
		return
	}
	if err != nil {
		slog.WarnContext(ctx, "consolidate: get target", "err", err)
		s.metrics.ConsolidateResult("error")
		return
	}

	content := dec.Content
	if content == "" {
		content = m.Content
	}
	vec, err := embed.EmbedOne(ctx, s.embedder, content)
	if err != nil {
		slog.WarnContext(ctx, "consolidate: re-embed", "err", err)
		s.metrics.ConsolidateResult("error")
		return
	}
	target.Content = content
	if dec.Summary != "" {
		target.Summary = dec.Summary
	}
	target.UpdatedAt = s.now()
	target.Embedding = vec
	if err := s.store.Upsert(ctx, target); err != nil {
		slog.WarnContext(ctx, "consolidate: upsert target", "err", err)
		s.metrics.ConsolidateResult("error")
		return
	}
	if err := s.store.Delete(ctx, m.Namespace, m.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "consolidate: delete merged source", "err", err)
	}
	s.metrics.ConsolidateResult("update")
}

// asyncSupersede tombstones target, pointing it at the already-stored memory m.
func (s *Service) asyncSupersede(ctx context.Context, m *memory.Memory, dec llm.Decision) {
	if dec.Target == "" || dec.Target == m.ID {
		s.metrics.ConsolidateResult("noop")
		return
	}
	if err := s.store.SetSuperseded(ctx, m.Namespace, dec.Target, m.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "consolidate: supersede target", "err", err)
		s.metrics.ConsolidateResult("error")
		return
	}
	s.metrics.ConsolidateResult("supersede")
}
