package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/eleboucher/memini/internal/chunk"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// chunkBackfillBatch bounds how many memories are chunked per tick. Lower than
// backfillBatch because each row here is many embedder calls rather than one:
// a 60k-rune memory is ~50 chunks, so 25 rows can already be a thousand vectors.
const chunkBackfillBatch = 25

// RunChunkBackfill periodically embeds the chunks of long memories that have
// none, until ctx is cancelled. No-op without a positive interval or with
// chunking off. Call once, in its own goroutine.
//
// Chunking runs here rather than on the write path deliberately. A long memory
// is several sequential embedder round-trips; doing that inside Remember would
// blow writeEmbedTimeout (5s by default) and degrade the write to
// pending_embed — for precisely the long memories this feature exists to help.
// The cost is that a long memory becomes fully searchable shortly after it is
// written rather than instantly. Its document vector works the whole time, so
// this is a gap in extra recall, not in recall.
func (s *Service) RunChunkBackfill(ctx context.Context, interval time.Duration) {
	if interval <= 0 || !s.chunkEmbed || s.chunkStore == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.BackfillChunks(ctx); err != nil {
				slog.WarnContext(ctx, "chunk backfill failed", "err", err)
			} else if n > 0 {
				slog.InfoContext(ctx, "chunked long memories", "count", n)
			}
		}
	}
}

// BackfillChunks splits and embeds long memories that carry no chunk rows,
// writing them straight to the store rather than through Remember: this repairs
// an existing row's index, and re-running the write gates (scrubbing, tier
// classification, the episodic value gate) could mutate or drop a row that
// already passed them once. Returns how many memories were chunked.
//
// Discovery is a query (ListUnchunked), not a metadata flag, and that choice
// does more work than it looks. Rows that predate chunking carry no flag to
// find them by — a flag would mean rewriting every row before the feature could
// do anything. It also means every path that drops chunks self-heals for free:
// an Upsert without chunks clears them (Memory.Chunks' contract), so the
// embed backfill re-embedding a row, consolidation rewriting merged content,
// and `memini reembed` swapping the model all leave rows this query finds again
// on the next tick. Nothing has to remember to stamp anything.
//
// Like the embed backfill, a first-row failure aborts the tick — the embedder
// is almost certainly down, and probing every remaining row against a dead
// backend helps nobody. A later row failing is logged and skipped so one bad
// row cannot wedge the tick.
func (s *Service) BackfillChunks(ctx context.Context) (int, error) {
	if !s.chunkEmbed || s.chunkStore == nil {
		return 0, nil
	}
	cfg := s.chunkCfg
	pending, err := s.chunkStore.ListUnchunked(ctx, "", cfg.MinContent, chunkBackfillBatch)
	if err != nil {
		return 0, err
	}

	done := 0
	for i, m := range pending {
		res := chunk.Split(m.Content, cfg)
		if len(res.Chunks) == 0 {
			// ListUnchunked bounds on content length, which is necessarily a
			// coarser test than Split's own. A row it returns that Split declines
			// would be listed again every tick forever, so skip it loudly enough
			// to be diagnosable rather than spinning.
			slog.DebugContext(ctx, "chunk backfill: nothing to split",
				"namespace", m.Namespace, "id", m.ID, "runes", len([]rune(m.Content)))
			continue
		}
		if res.Truncated {
			// The honest version of the ceiling this feature removes: say so.
			slog.WarnContext(ctx, "chunk backfill: content exceeds the chunk cap, tail not searchable by chunk recall",
				"namespace", m.Namespace, "id", m.ID, "chunks", len(res.Chunks), "max", cfg.MaxChunks)
		}

		vecs, err := s.embedChunks(ctx, res.Chunks)
		if err != nil {
			if i == 0 {
				slog.WarnContext(ctx, "chunk backfill: embedder unavailable, deferring tick", "err", err)
				return done, nil
			}
			slog.WarnContext(ctx, "chunk backfill: embed chunks", "namespace", m.Namespace, "id", m.ID, "err", err)
			continue
		}

		// The embeds above take real time, so a write may have landed on this
		// row meanwhile. Compare UpdatedAt against the snapshot: chunks are
		// derived from content, so writing them for content that changed under
		// us would index text the row no longer holds — the stale-chunk failure
		// Memory.Chunks warns about, arrived by a different road. A skipped row
		// is simply re-listed next tick.
		fresh, gerr := s.store.Get(ctx, m.Namespace, m.ID)
		if gerr != nil {
			if !errors.Is(gerr, store.ErrNotFound) {
				slog.WarnContext(ctx, "chunk backfill: re-get row", "namespace", m.Namespace, "id", m.ID, "err", gerr)
			}
			continue
		}
		if !fresh.UpdatedAt.Equal(m.UpdatedAt) {
			slog.InfoContext(ctx, "chunk backfill: row changed concurrently, deferring to next tick",
				"namespace", m.Namespace, "id", m.ID)
			continue
		}

		fresh.Chunks = vecs
		// UpdatedAt is deliberately NOT advanced: chunks are an index over
		// content that did not change. Touching it would make every backfilled
		// row look freshly written to recency ranking and to the next tick's own
		// concurrent-write guard.
		if err := s.store.Upsert(ctx, fresh); err != nil {
			slog.WarnContext(ctx, "chunk backfill: upsert row", "namespace", m.Namespace, "id", m.ID, "err", err)
			continue
		}
		done++
	}
	return done, nil
}

// embedChunks embeds one memory's segments, returning them indexed. The
// embedder is already batched (internal/embed.Batched), so this is one call
// that fans out under the configured per-request budgets rather than a call per
// chunk. A whitespace-only chunk is dropped: it carries no signal and would
// cost a vector and a row.
func (s *Service) embedChunks(ctx context.Context, texts []string) ([]memory.Chunk, error) {
	keep := make([]string, 0, len(texts))
	idx := make([]int, 0, len(texts))
	for i, t := range texts {
		if chunk.IsWhitespaceOnly(t) {
			continue
		}
		keep = append(keep, t)
		idx = append(idx, i)
	}
	if len(keep) == 0 {
		return nil, nil
	}
	vecs, err := s.embedder.Embed(ctx, keep)
	if err != nil {
		return nil, err
	}
	if len(vecs) != len(keep) {
		return nil, errChunkVecCount
	}
	out := make([]memory.Chunk, len(keep))
	for i := range keep {
		out[i] = memory.Chunk{Idx: idx[i], Embedding: vecs[i]}
	}
	return out, nil
}

var errChunkVecCount = errors.New("service: embedder returned a different number of chunk vectors than texts")
