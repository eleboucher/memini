package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// promoteBatch bounds how many episodic memories are distilled per LLM call.
const promoteBatch = 20

// promoteBatchTimeout bounds one distillation batch (LLM distill + fact writes
// + source stamping) so a slow provider cannot stall the promoter tick.
const promoteBatchTimeout = 90 * time.Second

// RunPromoter periodically distills frequently-accessed episodic memories into
// durable semantic facts until ctx is cancelled. It is a no-op without a
// distiller or a positive interval. Call once, typically in its own goroutine.
func (s *Service) RunPromoter(ctx context.Context, interval time.Duration) {
	if s.distiller == nil || interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.Promote(ctx); err != nil {
				slog.WarnContext(ctx, "promotion failed", "err", err)
			} else if n > 0 {
				slog.InfoContext(ctx, "promoted episodic memories to semantic", "facts", n)
			}
		}
	}
}

// Promote distills frequently-accessed, not-yet-promoted episodic memories in
// each namespace into durable semantic facts (written via Remember so they get
// the similarity gate and consolidation dedup), then stamps the sources so they
// aren't reprocessed. Returns the number of facts written. No-op without a
// distiller.
func (s *Service) Promote(ctx context.Context) (int, error) {
	start := time.Now()
	defer func() { s.metrics.OpDuration("promote", time.Since(start)) }()
	if s.distiller == nil {
		s.metrics.PromoteResult("ok", 0)
		return 0, nil
	}
	namespaces, err := s.store.ListNamespaces(ctx)
	if err != nil {
		s.metrics.PromoteResult("error", 0)
		return 0, err
	}
	now := s.now()
	total := 0
	for _, ns := range namespaces {
		eps, err := s.store.List(ctx, ns, store.Filter{Tiers: []memory.Tier{memory.TierEpisodic}, Now: s.now()}, 0)
		if err != nil {
			s.metrics.PromoteResult("error", total)
			return total, err
		}
		var pending []*memory.Memory
		for _, m := range eps {
			if m.AccessCount >= s.promoteMinAccess && !alreadyPromoted(m) {
				pending = append(pending, m)
			}
		}
		for start := 0; start < len(pending); start += promoteBatch {
			end := min(start+promoteBatch, len(pending))
			batchCtx, cancel := context.WithTimeout(ctx, promoteBatchTimeout)
			n, err := s.promote(batchCtx, ns, pending[start:end], now)
			cancel()
			if err != nil {
				slog.WarnContext(ctx, "promote batch", "namespace", ns, "err", err)
				continue
			}
			total += n
		}
	}
	s.metrics.PromoteResult("ok", total)
	return total, nil
}

// promote stamps the sources as promoted FIRST, then distills only the ones it
// stamped. Stamping first makes promotion idempotent: a later fact-write
// failure can't re-distill them next tick into duplicate (non-deterministically
// reworded) facts. The cost is losing this batch's facts on such a failure
// (logged, recoverable) rather than silently duplicating them.
func (s *Service) promote(ctx context.Context, ns string, batch []*memory.Memory, now time.Time) (int, error) {
	stamped := s.stampPromoted(ctx, batch, now)
	if len(stamped) == 0 {
		return 0, nil
	}

	episodes := make([]string, len(stamped))
	for i, m := range stamped {
		episodes[i] = m.Content
	}
	facts, err := s.distiller.Distill(ctx, llm.DistillInput{Episodes: episodes})
	if err != nil {
		return 0, err
	}

	written := 0
	for _, f := range facts {
		if strings.TrimSpace(f.Content) == "" {
			continue
		}
		if _, err := s.Remember(ctx, RememberInput{
			Namespace: ns, Content: f.Content, Summary: f.Summary, Tier: memory.TierSemantic,
		}); err != nil {
			slog.WarnContext(ctx, "promote: store fact", "err", err)
			continue
		}
		written++
	}
	return written, nil
}

// stampPromoted marks each source with metadata["promoted_at"] and returns the
// ones it stamped. Best-effort: a per-source Upsert failure is logged and
// skipped (that source stays eligible for the next tick). List omits
// embeddings, so re-embed to satisfy Upsert's dim check.
func (s *Service) stampPromoted(ctx context.Context, batch []*memory.Memory, now time.Time) []*memory.Memory {
	texts := make([]string, len(batch))
	for i, m := range batch {
		texts[i] = m.Content
	}
	vecs, err := s.embedder.Embed(ctx, texts)
	if err != nil {
		slog.WarnContext(ctx, "promote: embed sources for stamping", "err", err)
		return nil
	}
	stamp := now.UTC().Format(time.RFC3339)
	stamped := make([]*memory.Memory, 0, len(batch))
	for i, m := range batch {
		if m.Metadata == nil {
			m.Metadata = map[string]any{}
		}
		m.Metadata["promoted_at"] = stamp
		m.Embedding = vecs[i]
		m.UpdatedAt = now
		if err := s.store.Upsert(ctx, m); err != nil {
			slog.WarnContext(ctx, "promote: stamp source", "id", m.ID, "err", err)
			continue
		}
		stamped = append(stamped, m)
	}
	return stamped
}

// alreadyPromoted reports whether a memory has been distilled before.
func alreadyPromoted(m *memory.Memory) bool {
	if m.Metadata == nil {
		return false
	}
	_, ok := m.Metadata["promoted_at"]
	return ok
}
