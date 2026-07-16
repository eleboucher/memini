package service

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/eleboucher/memini/internal/chunk"
	"github.com/eleboucher/memini/internal/memory"
)

// chunkBackfillBatch bounds how many memories are chunked per tick. Lower than
// backfillBatch because each row here is many embedder calls rather than one:
// a 60k-rune memory is ~50 chunks, so 25 rows can already be a thousand vectors.
const chunkBackfillBatch = 25

// chunkBackfillScanCap bounds how many queue rows one tick may examine while
// paging toward its batch of chunkable ones. The queue can hold rows that
// yield nothing every time — content the splitter declines, a batch the
// embedder deterministically rejects — and the cursor pages past them rather
// than letting them occupy batch slots; this cap keeps a queue made mostly of
// such rows from turning every tick into a full-queue walk (each listed row
// carries its whole content).
const chunkBackfillScanCap = 20 * chunkBackfillBatch

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
// writing them through the store's PutChunks rather than an Upsert: PutChunks
// touches only the chunk tables under an updated_at guard held in the same
// transaction, so it cannot disturb the document vector (which Get never
// carries), cannot revert a concurrent Reinforce or update (a full-snapshot
// Upsert writes every column back), and closes the check-then-act window a
// service-level re-read would leave open. Returns how many memories were
// chunked.
//
// Discovery is a query (ListUnchunked), not a metadata flag, and that choice
// does more work than it looks. Rows that predate chunking carry no flag to
// find them by — a flag would mean rewriting every row before the feature could
// do anything. It also means every path that drops chunks self-heals for free:
// consolidation rewriting merged content and `memini reembed` swapping the
// model both leave rows this query finds again on the next tick. Nothing has
// to remember to stamp anything.
//
// The queue is walked with a cursor, and failures are judged by run length
// rather than position: two rows failing back to back is an embedder outage
// (defer the tick — probing every remaining row against a dead backend helps
// nobody), while a single row failing is that row's problem (skip it and move
// on). Both matter because the queue's order is deterministic — without the
// cursor and the run-length rule, one row whose batch the embedder always
// rejects, or a run of rows the splitter declines, would sit at the head and
// starve everything behind it forever.
func (s *Service) BackfillChunks(ctx context.Context) (int, error) {
	if !s.chunkEmbed || s.chunkStore == nil {
		return 0, nil
	}
	cfg := s.chunkCfg
	done, scanned := 0, 0
	consecutiveEmbedFailures := 0
	afterID := ""

scan:
	for done < chunkBackfillBatch && scanned < chunkBackfillScanCap {
		page, err := s.chunkStore.ListUnchunked(ctx, "", cfg.MinContent, afterID, chunkBackfillBatch)
		if err != nil {
			return done, err
		}
		if len(page) == 0 {
			break
		}
		for _, m := range page {
			afterID = m.ID
			scanned++
			if done >= chunkBackfillBatch || scanned > chunkBackfillScanCap {
				break scan
			}

			res := chunk.Split(m.Content, cfg)
			if len(res.Chunks) == 0 {
				// ListUnchunked bounds on content length, which is necessarily
				// a coarser test than Split's own. The cursor pages past such a
				// row so it cannot occupy a batch slot; it will be examined
				// again next tick, which is the price of keeping the queue
				// stateless.
				slog.DebugContext(ctx, "chunk backfill: nothing to split",
					"namespace", m.Namespace, "id", m.ID, "runes", utf8.RuneCountInString(m.Content))
				continue
			}
			if res.Truncated {
				// The honest version of the ceiling this feature removes: say so.
				slog.WarnContext(ctx, "chunk backfill: content exceeds the chunk cap, tail not searchable by chunk recall",
					"namespace", m.Namespace, "id", m.ID, "chunks", len(res.Chunks), "max", cfg.MaxChunks)
			}

			vecs, err := s.embedChunks(ctx, res.Chunks)
			if err != nil {
				consecutiveEmbedFailures++
				if consecutiveEmbedFailures >= 2 {
					// Two different rows in a row is an outage, not two poison
					// rows: give the embedder the rest of the interval.
					slog.WarnContext(ctx, "chunk backfill: embedder unavailable, deferring tick",
						"done", done, "err", err)
					break scan
				}
				slog.WarnContext(ctx, "chunk backfill: embed chunks",
					"namespace", m.Namespace, "id", m.ID, "err", err)
				continue
			}
			consecutiveEmbedFailures = 0

			// The embeds above take real time, so a write may have landed on
			// this row meanwhile. PutChunks re-checks updated_at inside its own
			// transaction: chunks are derived from content, and writing them
			// for content that changed under us would index text the row no
			// longer holds. A refused row is simply re-listed next tick.
			ok, perr := s.chunkStore.PutChunks(ctx, m.Namespace, m.ID, m.UpdatedAt, vecs)
			if perr != nil {
				slog.WarnContext(ctx, "chunk backfill: write chunks",
					"namespace", m.Namespace, "id", m.ID, "err", perr)
				continue
			}
			if !ok {
				slog.InfoContext(ctx, "chunk backfill: row changed concurrently, deferring to next tick",
					"namespace", m.Namespace, "id", m.ID)
				continue
			}
			done++
		}
	}
	s.reportChunkBacklog(ctx)
	return done, nil
}

// reportChunkBacklog publishes the queue's true depth. The batch caps work per
// tick, not the measurement: a gauge derived from the batch could never exceed
// it and would read 0 after every healthy tick regardless of how many long
// memories were still waiting — the opposite of what its alert description
// promises. Rows the splitter permanently declines are counted too; they
// genuinely have no chunks, and the skip log above is where they become
// diagnosable.
func (s *Service) reportChunkBacklog(ctx context.Context) {
	n, err := s.chunkStore.CountUnchunked(ctx, "", s.chunkCfg.MinContent)
	if err != nil {
		slog.WarnContext(ctx, "chunk backfill: count backlog", "err", err)
		return
	}
	s.metrics.ChunkBackfillPending(n)
}

// ChunkBacklog reports how many long memories still lack chunk rows, 0 when
// chunking is off. The bench's drain loop uses it to tell "queue empty" from
// "tick deferred": BackfillChunks reports both as 0, deliberately, because the
// server's ticker retries either way — a one-shot caller cannot.
func (s *Service) ChunkBacklog(ctx context.Context) (int, error) {
	if !s.chunkEmbed || s.chunkStore == nil {
		return 0, nil
	}
	return s.chunkStore.CountUnchunked(ctx, "", s.chunkCfg.MinContent)
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
		out[i] = memory.Chunk{Idx: idx[i], Text: keep[i], Embedding: vecs[i]}
	}
	return out, nil
}

var errChunkVecCount = errors.New("service: embedder returned a different number of chunk vectors than texts")
