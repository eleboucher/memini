package service

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// The deferred-repair worker.
//
// A write that could not reach the embedder is stored anyway, vectorless, and
// carries store.RepairPending in a column on its own row. This worker drains
// that state: it re-embeds, then re-runs the enrichment a healthy write would
// have run, then clears the state.
//
// Two properties are worth stating because they are what the design is for.
// First, a degraded write loses nothing but time — not just the vector but the
// dedup, corroboration and contradiction routing that need one are all replayed
// (see repair_enrich.go), where the old backfill deliberately skipped them and
// so lost them permanently. Second, nothing about the repair is in-memory: the
// state is a committed column, so it survives a restart, and the worker can be
// killed mid-repair without losing the work.

const (
	// repairBatch bounds one claim. Small on purpose: the lease protecting a
	// claimed batch must outlive the whole batch (see repairLeaseTTL), so a
	// bigger batch buys throughput at the price of a longer stale-lease window
	// after a crash.
	repairBatch = 8

	// repairTimeout bounds one repair end to end. The embed inside it carries
	// its own, tighter budget; this is the ceiling that stops a wedged store
	// write from parking the worker.
	repairTimeout = 45 * time.Second

	// repairLeaseTTL is how long a claim holds a row before another worker may
	// take it. It MUST exceed repairBatch*repairTimeout or a slow batch can be
	// double-claimed (pinned by TestRepairLeaseCoversBatch). It is also the
	// crash-recovery delay: after a hard kill, a claimed row re-enters the
	// queue this long later.
	repairLeaseTTL = 10 * time.Minute

	// repairMaxAttempts is the parking ceiling. Parking is self-healing (see
	// repairRearmAfter), so there is nothing here an operator needs to tune.
	repairMaxAttempts = 12

	// repairRearmAfter is how long a parked row rests before the sweeper
	// returns it to the queue. It turns the attempt ceiling into a circuit
	// breaker with an auto-close: during a multi-hour outage each row costs a
	// couple of embedder probes an hour instead of continuous hammering, and
	// recovery still needs no human.
	repairRearmAfter = time.Hour

	// repairBackoffBase and repairBackoffCap bound the retry schedule.
	// Deliberately gentler than the attempt^4 schedules general-purpose job
	// queues use: those reach days by the twentieth attempt, which for an
	// embedding backlog would leave a memory unsearchable for a week after a
	// transient blip. Past the cap, further retries say nothing new — the
	// provider is either down (the sweeper catches the recovery) or the row is
	// genuinely poison (the ceiling parks it).
	repairBackoffBase = 5 * time.Second
	repairBackoffCap  = 30 * time.Minute

	// repairEmbedTimeout bounds the embed inside a background repair,
	// deliberately decoupled from writeEmbedTimeout. The write budget is a
	// latency budget for a caller who is waiting; a background retry has no
	// caller, and holding it to the write budget would make repairs fail
	// forever against an embedder that is merely slow — the exact failure this
	// machinery exists to recover from.
	repairEmbedTimeout = 30 * time.Second
)

// errRepairMoot means the row no longer needs this stage — it was deleted,
// superseded, or already repaired by a concurrent write. The state is cleared
// and no failure is recorded.
var errRepairMoot = errors.New("repair no longer needed")

// repairStore returns the durable repair queue when the backing store provides
// one. Following eventLog's lazy-accessor pattern rather than a field set in
// New: the capability is a property of the store, and asserting at the point of
// use keeps New free of type switches.
func (s *Service) repairStore() (store.RepairStore, bool) {
	rs, ok := s.store.(store.RepairStore)
	return rs, ok
}

// kickRepair wakes RunRepairWorker so a freshly degraded write is repaired in
// milliseconds rather than on the next poll tick. It never blocks the writer.
//
// The channel has capacity 1 and a dropped send is correct, which is the whole
// contrast with enqueueConsolidate: this signal is edge-triggered ("there is
// work"), not per-row. A full channel already means a wake is pending, and the
// work itself is a committed column on the memory — so unlike a dropped
// consolidation job, a dropped kick loses nothing at all.
func (s *Service) kickRepair() {
	if s.repairKick == nil {
		return
	}
	select {
	case s.repairKick <- struct{}{}:
	default:
	}
}

// RunRepairWorker drains the deferred-repair queue until ctx is cancelled. It
// is a no-op without a positive interval or a store that provides the queue.
// Call once, typically in its own goroutine.
func (s *Service) RunRepairWorker(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	if _, ok := s.repairStore(); !ok {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.drainRepairs(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.repairKick:
		}
	}
}

// DrainRepairs runs repair batches until nothing is due, returning how many
// rows were processed. Exported for tests and for `doctor --fix`: it is the
// durable analogue of FlushConsolidation.
func (s *Service) DrainRepairs(ctx context.Context) (int, error) {
	rs, ok := s.repairStore()
	if !ok {
		return 0, nil
	}
	total := 0
	for _, state := range []store.RepairState{store.RepairPending, store.RepairEnrich} {
		for {
			n, err := s.runRepairBatch(ctx, rs, state)
			if err != nil {
				return total, err
			}
			total += n
			if n < repairBatch || ctx.Err() != nil {
				break
			}
		}
	}
	return total, nil
}

// drainRepairs is the worker loop's batch driver: same walk as DrainRepairs but
// logging instead of returning errors, since a poll tick has nobody to report
// to.
func (s *Service) drainRepairs(ctx context.Context) {
	rs, ok := s.repairStore()
	if !ok {
		return
	}
	for _, state := range []store.RepairState{store.RepairPending, store.RepairEnrich} {
		for {
			n, err := s.runRepairBatch(ctx, rs, state)
			if err != nil {
				slog.WarnContext(ctx, "repair: claiming work failed, retrying next tick",
					"stage", string(state), "err", err)
				break
			}
			if n < repairBatch || ctx.Err() != nil {
				break
			}
		}
	}
}

// runRepairBatch claims and runs one batch, returning how many rows it claimed.
func (s *Service) runRepairBatch(ctx context.Context, rs store.RepairStore, state store.RepairState) (int, error) {
	rows, err := rs.ClaimRepairs(ctx, state, s.now(), repairLeaseTTL, repairBatch)
	if err != nil {
		return 0, err
	}
	for i, row := range rows {
		if ctx.Err() != nil {
			// Shutdown: release the rest of the batch so a restart picks them
			// up immediately instead of waiting out the whole lease.
			s.releaseUnrun(rs, rows[i:])
			return len(rows), nil
		}
		s.runOneRepair(ctx, rs, row)
	}
	return len(rows), nil
}

// releaseUnrun returns rows claimed but never started to the queue, due now.
// Uses a detached context because the reason we are here is that ctx is done.
func (s *Service) releaseUnrun(rs store.RepairStore, rows []store.RepairRow) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	for _, row := range rows {
		if err := rs.FailRepair(ctx, row.Namespace, row.ID, "worker shutdown", s.now()); err != nil {
			slog.WarnContext(ctx, "repair: releasing an unrun row failed",
				"namespace", row.Namespace, "id", row.ID, "err", err)
		}
	}
}

// runOneRepair executes a single claimed row and records the outcome.
//
// The whole repair runs on a context detached from the worker loop's, so a
// shutdown landing mid-repair cannot roll back a store write that already
// decided to happen — and, critically, the outcome-recording writes below run
// on that same detached context. Recording an outcome under a cancelled context
// would leave a successfully repaired row still marked as owing work, to be
// re-claimed and re-embedded after the lease expires.
func (s *Service) runOneRepair(ctx context.Context, rs store.RepairStore, row store.RepairRow) {
	bg := context.WithoutCancel(ctx)
	rctx, cancel := context.WithTimeout(bg, repairTimeout)
	defer cancel()

	start := time.Now()
	err := s.dispatchRepair(rctx, rs, row)
	s.metrics.RepairDuration(string(row.State), time.Since(start))

	switch {
	case err == nil:
		s.metrics.RepairResult(string(row.State), "ok")
	case errors.Is(err, errRepairMoot):
		// Nothing to do and nothing wrong: clear the state so the row stops
		// being claimed. A failure to clear is itself only a latency event —
		// the next claim re-runs the same moot check.
		if ok, cerr := rs.SetRepairState(bg, row.Namespace, row.ID, row.Fingerprint, store.RepairNone); cerr != nil {
			slog.WarnContext(bg, "repair: clearing a moot row failed",
				"namespace", row.Namespace, "id", row.ID, "err", cerr)
		} else if !ok {
			// The row moved under us; the writer that moved it owns its state.
			slog.DebugContext(bg, "repair: moot row changed concurrently, leaving its state alone",
				"namespace", row.Namespace, "id", row.ID)
		}
		s.metrics.RepairResult(string(row.State), "moot")
	case row.Attempts >= repairMaxAttempts:
		if perr := rs.ParkRepair(bg, row.Namespace, row.ID, err.Error(), s.now()); perr != nil {
			slog.WarnContext(bg, "repair: parking a wedged row failed",
				"namespace", row.Namespace, "id", row.ID, "err", perr)
		}
		slog.ErrorContext(bg, "repair: giving up after the attempt ceiling; the memory stays degraded",
			"namespace", row.Namespace, "id", row.ID, "stage", string(row.State),
			"attempts", row.Attempts, "err", err)
		s.metrics.RepairResult(string(row.State), "parked")
	default:
		next := s.now().Add(repairBackoff(row.Attempts))
		if ferr := rs.FailRepair(bg, row.Namespace, row.ID, err.Error(), next); ferr != nil {
			slog.WarnContext(bg, "repair: recording a failure failed",
				"namespace", row.Namespace, "id", row.ID, "err", ferr)
		}
		slog.WarnContext(bg, "repair: attempt failed, backing off",
			"namespace", row.Namespace, "id", row.ID, "stage", string(row.State),
			"attempts", row.Attempts, "retry_in", repairBackoff(row.Attempts), "err", err)
		s.metrics.RepairResult(string(row.State), "retry")
	}
}

// dispatchRepair routes a claimed row to its stage handler.
func (s *Service) dispatchRepair(ctx context.Context, rs store.RepairStore, row store.RepairRow) error {
	switch row.State {
	case store.RepairPending:
		return s.repairEmbed(ctx, rs, row)
	case store.RepairEnrich:
		return s.repairEnrich(ctx, rs, row)
	default:
		return errRepairMoot
	}
}

// repairEmbed fills in a vectorless memory's embedding and advances it to the
// enrichment stage.
func (s *Service) repairEmbed(ctx context.Context, rs store.RepairStore, row store.RepairRow) error {
	// Check before spending an embedder call, not after. A row whose vector
	// already landed — a concurrent update that met a healthy embedder, or a
	// previous repair whose state write failed after the vector was stored —
	// must complete immediately. Doing this check after the embed instead is
	// what turns a single failed state write into an infinite re-embed loop.
	vec, err := s.store.GetEmbedding(ctx, row.Namespace, row.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errRepairMoot
		}
		return err
	}
	if len(vec) > 0 {
		// Healthy despite the marker. This is also the guard that absorbs the
		// stale pending flags older import/reembed/dedup/promote paths could
		// leave behind: they hand a row a perfectly good vector and do not
		// clear the marker, and without this check every one of them would
		// cost a pointless embedder round trip.
		return s.advanceToEnrich(ctx, rs, row)
	}

	fresh, err := s.embedForRepair(ctx, row.Content)
	if err != nil {
		return err
	}
	ok, err := rs.SetEmbeddingIfUnchanged(ctx, row.Namespace, row.ID, row.Fingerprint, fresh, store.RepairEnrich)
	if err != nil {
		return err
	}
	if !ok {
		// The content changed while we were embedding, so this vector is for
		// text the memory no longer holds. The writer that changed it set its
		// own state; discard ours.
		return errRepairMoot
	}
	s.metrics.RepairResult(string(store.RepairPending), "embedded")
	// The vector is in place, so the enrichment stage is now runnable. Wake the
	// worker rather than waiting a poll interval for it.
	s.kickRepair()
	return nil
}

// advanceToEnrich moves a row that already has a vector on to enrichment.
func (s *Service) advanceToEnrich(ctx context.Context, rs store.RepairStore, row store.RepairRow) error {
	ok, err := rs.SetRepairState(ctx, row.Namespace, row.ID, row.Fingerprint, store.RepairEnrich)
	if err != nil {
		return err
	}
	if !ok {
		return errRepairMoot
	}
	s.kickRepair()
	return nil
}

// embedForRepair embeds one row's content under the background budget. Being
// its own function scopes the cancel to one row rather than deferring it in a
// loop.
func (s *Service) embedForRepair(ctx context.Context, content string) ([]float32, error) {
	budget := s.backgroundEmbedTimeout
	if budget <= 0 {
		budget = repairEmbedTimeout
	}
	ectx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return embed.EmbedOne(ectx, s.embedder, content)
}

// repairBackoff returns the delay before a failed repair's next attempt:
// exponential from repairBackoffBase, doubling per attempt, capped at
// repairBackoffCap, then full-jittered into [0, d].
//
// The jitter is not decoration. Every row degraded during one embedder outage
// was marked within the same few seconds, so an unjittered schedule retries
// them all in the same instant — a thundering herd aimed at the backend that
// just failed. Full jitter is the variant that does the least redundant work.
func repairBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := repairBackoffBase
	for range attempts - 1 {
		d *= 2
		if d >= repairBackoffCap {
			d = repairBackoffCap
			break
		}
	}
	return time.Duration(rand.Int64N(int64(d)) + 1)
}

// RunRepairSweeper is the safety net behind the repair worker: it re-arms rows
// parked by a long outage, and adopts rows that owe a vector but carry no
// repair state.
//
// Unlike the loop it replaces, it never touches the embedder. Because repair
// state is committed with the write that needs it, this sweeper is an
// optimization rather than a correctness requirement — but it still earns its
// place, because rows written by a release that predates the repair columns, or
// by a path that bypasses Remember (import, restore), carry no state and would
// otherwise never be repaired.
//
// It is a no-op without a positive interval. Falls back to the pre-repair
// backfill loop when the store provides no repair queue.
func (s *Service) RunRepairSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	if _, ok := s.repairStore(); !ok {
		s.RunEmbedBackfill(ctx, interval)
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.SweepRepairs(ctx); err != nil {
				slog.WarnContext(ctx, "repair sweep failed", "err", err)
			}
		}
	}
}

// SweepRepairs runs one sweep: re-arm parked rows, adopt unmarked ones, and
// refresh the backlog gauges.
func (s *Service) SweepRepairs(ctx context.Context) error {
	rs, ok := s.repairStore()
	if !ok {
		return nil
	}
	now := s.now()
	if n, err := rs.RearmRepairs(ctx, now.Add(-repairRearmAfter), now); err != nil {
		slog.WarnContext(ctx, "repair sweep: re-arming parked rows failed", "err", err)
	} else if n > 0 {
		slog.InfoContext(ctx, "repair sweep: re-armed parked rows after their rest period", "count", n)
		s.kickRepair()
	}

	if n, err := s.adoptUnmarked(ctx, rs); err != nil {
		return err
	} else if n > 0 {
		slog.InfoContext(ctx, "repair sweep: adopted memories that owed a vector but carried no repair state",
			"count", n)
		s.kickRepair()
	}
	return s.reportRepairBacklog(ctx, rs)
}

// adoptUnmarked finds memories still carrying the legacy pending-embed metadata
// marker and moves them onto the repair queue.
//
// This is the upgrade path: a database written by a release before the repair
// columns existed has the marker and no state. It is bounded per tick so a
// large legacy backlog does not turn one sweep into a single enormous
// transaction, and it costs nothing on a store that has already been swept.
func (s *Service) adoptUnmarked(ctx context.Context, rs store.RepairStore) (int64, error) {
	namespaces, err := s.store.ListNamespaces(ctx)
	if err != nil {
		return 0, err
	}
	filter := store.Filter{
		Metadata: map[string]string{memory.PendingEmbedKey: memory.PendingEmbedValue},
		Now:      s.now(),
	}
	var adopted int64
	for _, ns := range namespaces {
		mems, err := s.store.List(ctx, ns, filter, adoptBatch)
		if err != nil {
			return adopted, err
		}
		if len(mems) == 0 {
			continue
		}
		ids := make([]string, 0, len(mems))
		for _, m := range mems {
			ids = append(ids, m.ID)
		}
		n, err := rs.MarkRepairNeeded(ctx, ns, ids, store.RepairPending)
		if err != nil {
			return adopted, err
		}
		adopted += n
	}
	return adopted, nil
}

// adoptBatch bounds one namespace's legacy-marker scan per sweep.
const adoptBatch = 200

// reportRepairBacklog refreshes the queue-depth and oldest-age gauges. These
// run on the sweep tick rather than the (much faster) poll tick because the
// stats query is a GROUP BY over the memories table — the one query in this
// subsystem that could actually cost something on a large backlog.
func (s *Service) reportRepairBacklog(ctx context.Context, rs store.RepairStore) error {
	stats, err := rs.RepairStats(ctx)
	if err != nil {
		return err
	}
	seen := map[store.RepairState]bool{}
	now := s.now()
	for _, st := range stats {
		seen[st.State] = true
		s.metrics.RepairDepth(string(st.State), st.Count)
		if st.OldestAt.IsZero() {
			s.metrics.RepairOldestAge(string(st.State), 0)
			continue
		}
		s.metrics.RepairOldestAge(string(st.State), now.Sub(st.OldestAt).Seconds())
	}
	// Zero the states with no rows, so a drained queue reads as 0 rather than
	// leaving the last non-zero value pinned on the dashboard forever.
	for _, st := range []store.RepairState{store.RepairPending, store.RepairEnrich, store.RepairFailed} {
		if !seen[st] {
			s.metrics.RepairDepth(string(st), 0)
			s.metrics.RepairOldestAge(string(st), 0)
		}
	}
	return nil
}

// RepairBacklog returns the current repair queue by state, for /healthz, the
// UI and doctor. Empty when the store provides no repair queue.
func (s *Service) RepairBacklog(ctx context.Context) ([]store.RepairStat, error) {
	rs, ok := s.repairStore()
	if !ok {
		return nil, nil
	}
	return rs.RepairStats(ctx)
}

// kickRepairIfDegraded wakes the repair worker for a write that stored without
// a vector. The guard lives here rather than at the call site because Remember
// is at its cyclomatic ceiling: one more branch there fails lint.
func (s *Service) kickRepairIfDegraded(m *memory.Memory) {
	if m == nil || m.EmbedState == "" {
		return
	}
	s.kickRepair()
}
