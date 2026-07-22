package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/eleboucher/memini/internal/memory"
)

// Batched distill-on-write tuning.
const (
	// distillBatchDefaultMaxAge is how long the oldest buffered capture may
	// wait before the buffer flushes regardless of size, so a quiet session's
	// tail still distills promptly.
	distillBatchDefaultMaxAge = 10 * time.Minute
	// distillBatchMaxBuffers bounds live per-session buffers on a long-lived
	// server; past it the stalest buffer is flushed early rather than growing
	// without bound.
	distillBatchMaxBuffers = 64
	// distillBatchTick is how often StartDistillBatcher runs the age check.
	distillBatchTick = 30 * time.Second
)

// distillBatcher accumulates fresh episodic captures per (namespace,
// session_id) so distillation sees a whole exchange in one LLM call instead of
// one call per turn. Mutex-guarded: concurrent Remember calls hit the same
// buffer.
type distillBatcher struct {
	mu        sync.Mutex
	buffers   map[string]*distillBuffer
	maxTokens int
	maxAge    time.Duration
}

// distillBuffer is one session's pending captures.
type distillBuffer struct {
	namespace string
	items     []*memory.Memory
	tokens    int
	oldest    time.Time
}

// WithDistillBatch batches distill-on-write per (namespace, session_id):
// captures accumulate until their estimated tokens reach maxTokens or the
// oldest has waited maxAge, then distill as one LLM call with cross-turn
// context. maxTokens <= 0 disables batching (per-capture distill); captures
// without a session_id always use the per-capture path. Crash-safe by
// construction: sources are already durably stored and are stamped promoted_at
// only at flush, so a lost buffer stays eligible for the batch promoter.
func WithDistillBatch(maxTokens int, maxAge time.Duration) Option {
	return func(s *Service) {
		if maxTokens <= 0 {
			return
		}
		if maxAge <= 0 {
			maxAge = distillBatchDefaultMaxAge
		}
		s.distillBatch = &distillBatcher{
			buffers:   map[string]*distillBuffer{},
			maxTokens: maxTokens,
			maxAge:    maxAge,
		}
	}
}

// StartDistillBatcher runs the age-flush loop for batched distill-on-write
// until ctx is cancelled, then flushes every remaining buffer. A no-op unless
// the service was built with WithDistillBatch. Call once, typically in its own
// goroutine (mirrors StartConsolidator).
func (s *Service) StartDistillBatcher(ctx context.Context) {
	if s.distillBatch == nil {
		return
	}
	t := time.NewTicker(distillBatchTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flushDistillBatches(func(*distillBuffer) bool { return true })
			return
		case <-t.C:
			now := s.now()
			s.flushDistillBatches(func(b *distillBuffer) bool {
				return now.Sub(b.oldest) >= s.distillBatch.maxAge
			})
		}
	}
}

// enqueueDistillBatch buffers a fresh episodic capture for batched
// distillation, flushing the session's buffer once it reaches the token
// threshold. Returns false when batching is off or the capture carries no
// session_id — the caller falls back to per-capture distill.
func (s *Service) enqueueDistillBatch(m *memory.Memory) bool {
	b := s.distillBatch
	if b == nil {
		return false
	}
	sid, _ := m.Metadata["session_id"].(string)
	if sid == "" {
		return false
	}

	var flush []*distillBuffer
	b.mu.Lock()
	key := m.Namespace + "\x00" + sid
	buf := b.buffers[key]
	if buf == nil {
		buf = &distillBuffer{namespace: m.Namespace, oldest: s.now()}
		b.buffers[key] = buf
	}
	buf.items = append(buf.items, m)
	buf.tokens += (len(m.Content) + 3) / 4
	if buf.tokens >= b.maxTokens {
		delete(b.buffers, key)
		flush = append(flush, buf)
	}
	if len(b.buffers) > distillBatchMaxBuffers {
		var staleKey string
		var stale *distillBuffer
		for k, v := range b.buffers {
			if stale == nil || v.oldest.Before(stale.oldest) {
				staleKey, stale = k, v
			}
		}
		delete(b.buffers, staleKey)
		flush = append(flush, stale)
	}
	b.mu.Unlock()

	for _, f := range flush {
		s.flushDistillBuffer(f)
	}
	return true
}

// flushDistillBatches removes and flushes every buffer the predicate selects.
// Split out from the ticker loop so tests can drive the age check against a
// fake clock directly.
func (s *Service) flushDistillBatches(pick func(*distillBuffer) bool) {
	b := s.distillBatch
	if b == nil {
		return
	}
	var flush []*distillBuffer
	b.mu.Lock()
	for k, v := range b.buffers {
		if pick(v) {
			delete(b.buffers, k)
			flush = append(flush, v)
		}
	}
	b.mu.Unlock()
	for _, f := range flush {
		s.flushDistillBuffer(f)
	}
}

// flushDistillBuffer distills one buffered session batch in the background,
// bounded by distillSem like the per-capture path and tracked by s.bg so
// WaitBackground joins it. Reuses the promote path (stamp → distill → write
// deduped facts); a failure is logged, the sources stay.
func (s *Service) flushDistillBuffer(buf *distillBuffer) {
	s.bg.Go(func() {
		s.distillSem <- struct{}{}
		defer func() { <-s.distillSem }()
		ctx, cancel := context.WithTimeout(context.Background(), s.distillTimeout)
		defer cancel()
		if _, err := s.promote(ctx, buf.namespace, buf.items, s.now()); err != nil {
			slog.WarnContext(ctx, "distill batch", "namespace", buf.namespace,
				"captures", len(buf.items), "err", err)
		}
	})
}
