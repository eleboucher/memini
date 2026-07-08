package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// backfillBatch bounds how many pending-embed memories are re-embedded per
// tick, so a large backlog (e.g. after an extended embedder outage) can't turn
// one tick into an unbounded, slow-draining scan.
const backfillBatch = 100

// RunEmbedBackfill periodically re-embeds memories that were stored vectorless
// (metadata pending_embed="true", stamped by embedForRemember when the
// write-time embed budget was exceeded or the embedder errored) until ctx is
// cancelled. It is a no-op without a positive interval. Call once, typically
// in its own goroutine.
func (s *Service) RunEmbedBackfill(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.BackfillEmbeddings(ctx); err != nil {
				slog.WarnContext(ctx, "embed backfill failed", "err", err)
			} else if n > 0 {
				slog.InfoContext(ctx, "backfilled pending embeddings", "count", n)
			}
		}
	}
}

// BackfillEmbeddings re-embeds memories left vectorless by a degraded write
// (metadata pending_embed="true") directly against the store, bypassing
// Remember: this is a repair of an existing row's vector, not a fresh write,
// so re-running scrubbing, tier classification, or the episodic value gate
// could mutate or drop a row that already passed those gates once. Deferred
// similarity jobs (dedup/corroborate/contradict) are deliberately NOT re-run
// here -- re-entering them outside Remember's write ordering risks touching
// rows this pass has no business touching. A backfilled fact simply rejoins
// those jobs the next time it (or something similar) is naturally restated;
// an acceptable v1 tradeoff over the complexity of re-triggering them safely.
//
// Processes at most backfillBatch rows per tick across all namespaces. If the
// very first row in a tick fails to embed, the embedder is almost certainly
// still down: the whole tick aborts right there with a single Warn instead of
// probing every remaining pending row against a dead backend. A later row
// failing on its own (e.g. bad content) is logged and skipped so one bad row
// can't wedge the rest of the tick's progress. Returns the number of rows
// successfully backfilled.
func (s *Service) BackfillEmbeddings(ctx context.Context) (int, error) {
	namespaces, err := s.store.ListNamespaces(ctx)
	if err != nil {
		return 0, err
	}

	filter := store.Filter{Metadata: map[string]string{"pending_embed": "true"}, Now: s.now()}
	var pending []*memory.Memory
	for _, ns := range namespaces {
		mems, err := s.store.List(ctx, ns, filter, 0)
		if err != nil {
			return 0, err
		}
		pending = append(pending, mems...)
	}
	found := len(pending)
	if len(pending) > backfillBatch {
		pending = pending[:backfillBatch]
	}

	backfilled := 0
	for i, m := range pending {
		vec, err := s.embedForBackfill(ctx, m.Content)
		if err != nil {
			if i == 0 {
				slog.WarnContext(ctx, "embed backfill: embedder unavailable, deferring tick",
					"pending", found, "err", err)
				s.metrics.EmbedBackfillPending(found)
				return backfilled, nil
			}
			slog.WarnContext(ctx, "embed backfill: embed row", "namespace", m.Namespace, "id", m.ID, "err", err)
			continue
		}
		delete(m.Metadata, "pending_embed")
		m.Embedding = vec
		m.UpdatedAt = s.now()
		if err := s.store.Upsert(ctx, m); err != nil {
			slog.WarnContext(ctx, "embed backfill: upsert row", "namespace", m.Namespace, "id", m.ID, "err", err)
			continue
		}
		backfilled++
	}
	s.metrics.EmbedBackfillPending(found - backfilled)
	return backfilled, nil
}

// embedForBackfill embeds one pending row's content under the same budget the
// write path uses (writeEmbedTimeout, see embedForRemember): a
// slow-but-not-erroring embedder — a network stall, exactly the degraded
// scenario backfill exists to recover from — must surface as a per-row error
// (aborting or skipping per BackfillEmbeddings' rules) instead of hanging the
// tick and every tick after it. writeEmbedTimeout <= 0 keeps the embed
// unbounded, matching the write path's fail-fast default. Being its own
// function scopes the cancel to one row rather than deferring it in a loop.
func (s *Service) embedForBackfill(ctx context.Context, content string) ([]float32, error) {
	if s.writeEmbedTimeout <= 0 {
		return embed.EmbedOne(ctx, s.embedder, content)
	}
	ectx, cancel := context.WithTimeout(ctx, s.writeEmbedTimeout)
	defer cancel()
	return embed.EmbedOne(ectx, s.embedder, content)
}
