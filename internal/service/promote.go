package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/extract"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// promoteBatch bounds how many episodic memories are distilled per LLM call.
const promoteBatch = 20

// promoteBatchTimeout bounds one distillation batch (LLM distill + fact writes
// + source stamping) so a slow provider cannot stall the promoter tick.
const promoteBatchTimeout = 90 * time.Second

// RunPromoter periodically distills frequently-accessed short-term memories
// into durable semantic facts until ctx is cancelled. It is a no-op without a
// positive interval. Call once, typically in its own goroutine.
func (s *Service) RunPromoter(ctx context.Context, interval time.Duration) {
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
			if n, err := s.Promote(ctx); err != nil {
				slog.WarnContext(ctx, "promotion failed", "err", err)
			} else if n > 0 {
				slog.InfoContext(ctx, "promoted short-term memories to semantic", "facts", n)
			}
		}
	}
}

// Promote distills frequently-accessed, not-yet-promoted short-term memories
// (working and episodic) in each namespace into durable semantic facts (written
// via Remember so they get the similarity gate and consolidation dedup), then
// stamps the sources so they aren't reprocessed. Working memories that have
// proven valuable (AccessCount >= promoteMinAccess) are first retiered to
// episodic, then the combined pool is distilled. Without a distiller it falls
// back to the marker extractor, so usage-earned promotion also works on
// LLM-less deployments. Returns the number of facts written.
func (s *Service) Promote(ctx context.Context) (int, error) {
	start := time.Now()
	defer func() { s.metrics.OpDuration("promote", time.Since(start)) }()
	namespaces, err := s.store.ListNamespaces(ctx)
	if err != nil {
		s.metrics.PromoteResult("error", 0)
		return 0, err
	}
	now := s.now()
	total := 0
	for _, ns := range namespaces {
		// Broaden from episodic-only to the full short-term set: working is the
		// default intake tier, so the dominant pool now lives there.
		eps, err := s.store.List(ctx, ns, store.Filter{Tiers: []memory.Tier{memory.TierWorking, memory.TierEpisodic}, Now: s.now()}, 0)
		if err != nil {
			s.metrics.PromoteResult("error", total)
			return total, err
		}
		// Retier valuable working memories to episodic before durable extraction:
		// a working memory that was recalled enough to earn promotion has proven
		// worth keeping longer than the 72h intake TTL. Stamped so it isn't
		// re-retiered on the next tick.
		s.retierWorkingToEpisodic(ctx, ns, eps, now)
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
	if s.distiller == nil {
		return s.promoteHeuristic(ctx, ns, stamped)
	}

	episodes := make([]llm.Episode, len(stamped))
	for i, m := range stamped {
		e := llm.Episode{Content: m.Content}
		// Surface a captured failure (metadata.failed, set by the integrations)
		// into the content so the distiller can pair a failed turn with a later
		// success into a recovery procedure.
		if failedEpisode(m) {
			e.Content = "[failed] " + e.Content
		}
		if !m.CreatedAt.IsZero() {
			e.Date = m.CreatedAt.UTC().Format(time.DateOnly)
		}
		episodes[i] = e
	}
	facts, err := s.distiller.Distill(ctx, llm.DistillInput{
		Episodes: episodes,
		Now:      now.UTC().Format(time.DateOnly),
	})
	if err != nil {
		return 0, err
	}

	// Provenance: every distilled fact records the episodic IDs it was distilled
	// from. Batch-granular (the LLM does not attribute facts to episodes), so a
	// single-episode batch also gets the exact promoted_from pointer and inherits
	// the source's session_id for the integrations' session-echo guard.
	sourceIDs := make([]string, len(stamped))
	for i, m := range stamped {
		sourceIDs[i] = m.ID
	}

	written := 0
	for _, f := range facts {
		if strings.TrimSpace(f.Content) == "" {
			continue
		}
		meta := map[string]any{"source_ids": sourceIDs}
		if cat := strings.ToLower(strings.TrimSpace(f.Category)); cat != "" {
			meta["distill_category"] = cat
		}
		if len(stamped) == 1 {
			meta["promoted_from"] = stamped[0].ID
			if sid, ok := stamped[0].Metadata["session_id"].(string); ok && sid != "" {
				meta["session_id"] = sid
			}
		}
		in := RememberInput{
			Namespace: ns, Content: f.Content, Summary: f.Summary, Tier: tierForCategory(f.Category),
			Level: memory.LevelDeduced, Metadata: meta, Confidence: f.Confidence,
		}
		if _, err := s.Remember(ctx, in); err != nil {
			slog.WarnContext(ctx, "promote: store fact", "err", err)
			continue
		}
		written++
	}
	return written, nil
}

// promoteWholeMaxChars bounds whole-content heuristic promotion: a source this
// short reads as a single statement, so it can become a fact verbatim.
const promoteWholeMaxChars = 240

// promoteHeuristic is the LLM-less promotion path: each stamped source is run
// through the marker extractor and the typed segments are written as durable
// facts (via Remember, so they get fingerprint/write-dedup like distilled
// ones). A short source with no extractable segment is written whole as a
// semantic fact — it earned durability by being recalled repeatedly, even if
// it matches no marker. Write-time extraction already mined most sources at
// capture; fingerprint dedup turns those re-extractions into corroboration
// instead of duplicates.
func (s *Service) promoteHeuristic(ctx context.Context, ns string, batch []*memory.Memory) (int, error) {
	written := 0
	for _, m := range batch {
		src := strings.TrimSpace(m.Content)
		var ins []RememberInput
		if results := extract.Typed(src); len(results) > 0 {
			for _, r := range results {
				ins = append(ins, RememberInput{
					Namespace: ns, Content: r.Content, Tier: r.Kind.Tier(),
					Level:    memory.LevelExplicit,
					Tags:     []string{string(r.Kind)},
					Metadata: map[string]any{"memory_type": string(r.Kind), "promoted_from": m.ID},
				})
			}
		} else if src != "" && len(src) <= promoteWholeMaxChars {
			ins = append(ins, RememberInput{
				Namespace: ns, Content: src, Tier: memory.TierSemantic,
				Level:    memory.LevelExplicit,
				Metadata: map[string]any{"promoted_from": m.ID},
			})
		}
		for _, in := range ins {
			if _, err := s.Remember(ctx, in); err != nil {
				slog.WarnContext(ctx, "promote: store fact", "namespace", ns, "err", err)
				continue
			}
			written++
		}
	}
	return written, nil
}

// tierForCategory routes a distilled fact to a tier by its category: a
// "procedure" (including error→recovery) is procedural; "preference" and "fact"
// (and any unknown/empty value) are semantic.
func tierForCategory(category string) memory.Tier {
	if strings.EqualFold(strings.TrimSpace(category), "procedure") {
		return memory.TierProcedural
	}
	return memory.TierSemantic
}

// failedEpisode reports whether a source episodic was captured from a failed
// turn or command (metadata.failed, set by the integrations and the plugin).
func failedEpisode(m *memory.Memory) bool {
	if m.Metadata == nil {
		return false
	}
	v, ok := m.Metadata["failed"].(bool)
	return ok && v
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

// retieredToEpisodic is the metadata stamp marking a working memory that was
// promoted to episodic, so it isn't re-retiered on the next tick.
const retieredToEpisodic = "episodic"

// retierWorkingToEpisodic promotes working memories that have been recalled
// enough to clear promoteMinAccess from the 72h intake tier to episodic (30d
// TTL), so content that proved valuable survives longer than raw scratch. The
// memory must have its embedding loaded (List omits it); a missing embedding
// skips the retier rather than failing the promote pass. Stamped with
// metadata["retiered_to"]="episodic" so it isn't re-retiered next tick.
// Best-effort: a per-memory failure is logged and skipped.
func (s *Service) retierWorkingToEpisodic(ctx context.Context, ns string, mems []*memory.Memory, now time.Time) {
	for _, m := range mems {
		if m.Tier != memory.TierWorking {
			continue
		}
		if m.AccessCount < s.promoteMinAccess {
			continue
		}
		if m.Metadata != nil {
			if v, ok := m.Metadata["retiered_to"].(string); ok && v == retieredToEpisodic {
				continue
			}
		}
		// Re-embed: List omits embeddings, and Upsert's dim check needs one.
		vecs, err := s.embedder.Embed(ctx, []string{m.Content})
		if err != nil {
			slog.WarnContext(ctx, "promote: retier embed", "namespace", ns, "id", m.ID, "err", err)
			continue
		}
		m.Tier = memory.TierEpisodic
		m.Embedding = vecs[0]
		m.UpdatedAt = now
		exp := now.Add(memory.TierEpisodic.DefaultTTL())
		m.ExpiresAt = &exp
		if m.Metadata == nil {
			m.Metadata = map[string]any{}
		}
		m.Metadata["retiered_to"] = retieredToEpisodic
		if err := s.store.Upsert(ctx, m); err != nil {
			slog.WarnContext(ctx, "promote: retier to episodic", "namespace", ns, "id", m.ID, "err", err)
		}
	}
}
