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
			n, err := s.promote(ctx, ns, pending[start:end], now)
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

// promote distills one batch of episodic memories into facts, stores the facts,
// and stamps the sources as promoted.
func (s *Service) promote(ctx context.Context, ns string, batch []*memory.Memory, now time.Time) (int, error) {
	episodes := make([]string, len(batch))
	for i, m := range batch {
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

	// Stamp the sources so they're not reprocessed. List omits embeddings, so
	// re-embed (deterministic, cache-friendly) to satisfy Upsert's dim check.
	if err := s.stampPromoted(ctx, batch, now); err != nil {
		slog.WarnContext(ctx, "promote: stamp sources", "err", err)
	}
	return written, nil
}

// stampPromoted marks each source memory with metadata["promoted_at"].
func (s *Service) stampPromoted(ctx context.Context, batch []*memory.Memory, now time.Time) error {
	texts := make([]string, len(batch))
	for i, m := range batch {
		texts[i] = m.Content
	}
	vecs, err := s.embedder.Embed(ctx, texts)
	if err != nil {
		return err
	}
	stamp := now.UTC().Format(time.RFC3339)
	for i, m := range batch {
		if m.Metadata == nil {
			m.Metadata = map[string]any{}
		}
		m.Metadata["promoted_at"] = stamp
		m.Embedding = vecs[i]
		m.UpdatedAt = now
		if err := s.store.Upsert(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// alreadyPromoted reports whether a memory has been distilled before.
func alreadyPromoted(m *memory.Memory) bool {
	if m.Metadata == nil {
		return false
	}
	_, ok := m.Metadata["promoted_at"]
	return ok
}
