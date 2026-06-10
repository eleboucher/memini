package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/search"
	"github.com/eleboucher/memini/internal/store"
)

// consolidateCandidates is how many near-neighbours are offered to the LLM when
// deciding whether a new memory is novel, a refinement, or a contradiction.
const consolidateCandidates = 5

// promoteBatch bounds how many episodic memories are distilled per LLM call.
const promoteBatch = 20

// Async-consolidation tuning.
const (
	consolidateQueueCap     = 1024
	consolidateDrainTimeout = 30 * time.Second
	reinforceTimeout        = 10 * time.Second

	// recallPoolFactor / recallPoolFloor size the per-leg candidate pool for
	// hybrid recall: each leg over-fetches max(k*factor, floor) so a memory
	// ranked just outside the top k in both legs can still win after RRF fusion.
	recallPoolFactor = 5
	recallPoolFloor  = 50
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

// Metrics receives service-level events for observability. Methods must be safe
// for concurrent use; a nil Metrics is replaced by a no-op.
type Metrics interface {
	// ConsolidateResult records one consolidation outcome: one of
	// "gated", "new", "update", "supersede", "noop", "error", "dropped".
	ConsolidateResult(result string)
	// ConsolidateQueueDepth reports the current async queue depth.
	ConsolidateQueueDepth(depth int)
	// RememberResult records the outcome of a Remember call: result is
	// "ok"|"error" and tier is the memory's tier.
	RememberResult(result, tier string)
	// RecallResult records the outcome of a Recall call. result is
	// "ok"|"error"; tierFilter is one of "all"|"working"|"episodic"|
	// "semantic"|"procedural"|"mixed"; hitsBucket is a pre-bucketed
	// count of returned memories: "0"|"1"|"2-5"|"6-20"|"21+".
	RecallResult(result, tierFilter, hitsBucket string)
	// ForgetResult records the outcome of a Forget call: "ok"|"not_found"|"error".
	ForgetResult(result string)
	// PromoteResult records one Promote batch: result is "ok"|"error";
	// facts is the number of semantic facts written.
	PromoteResult(result string, facts int)
	// FsckResult records one fsck pass: "ok"|"error". Counters for the
	// work done (purged, evicted, duplicate groups) are exposed separately
	// via the store's maintenance metrics.
	FsckResult(result string)
	// OpDuration observes end-to-end latency for a public operation.
	OpDuration(op string, d time.Duration)
}

type nopMetrics struct{}

func (nopMetrics) ConsolidateResult(string)            {}
func (nopMetrics) ConsolidateQueueDepth(int)           {}
func (nopMetrics) RememberResult(string, string)       {}
func (nopMetrics) RecallResult(string, string, string) {}
func (nopMetrics) ForgetResult(string)                 {}
func (nopMetrics) PromoteResult(string, int)           {}
func (nopMetrics) FsckResult(string)                   {}
func (nopMetrics) OpDuration(string, time.Duration)    {}

// consolidateJob identifies an already-stored memory awaiting background
// consolidation.
type consolidateJob struct {
	namespace string
	id        string
}

// Service wires storage and embeddings together. It is safe for concurrent use.
type Service struct {
	store    store.Store
	embedder embed.Embedder
	// consolidator is optional; when set, durable writes are deduplicated and
	// contradiction-resolved against existing memories.
	consolidator llm.Consolidator

	consolidateMode     ConsolidateMode
	consolidateMinScore float64
	// consolidateQueue carries background jobs in async mode; nil otherwise.
	consolidateQueue chan consolidateJob

	// distiller is optional; when set, RunPromoter distills frequently-accessed
	// episodic memories into durable semantic facts.
	distiller        llm.Distiller
	promoteMinAccess int

	metrics Metrics
	// syncReinforce makes recall reinforcement synchronous (deterministic tests).
	syncReinforce bool

	// shortTermCap bounds short-term memories per namespace during fsck (0 = off).
	shortTermCap int
	// queryPrefix is prepended to recall queries before embedding, enabling the
	// asymmetric instruction mode of instruction-tuned embedders. Documents are
	// always embedded bare.
	queryPrefix string
	// scoreFusionAlpha selects the hybrid fusion strategy: >= 0 (the default) uses
	// convex-combination score fusion with this vector-vs-keyword weight; < 0
	// falls back to rank fusion (RRF).
	scoreFusionAlpha float64
	// poolFactor / poolFloor size the per-leg recall candidate pool as
	// max(k*poolFactor, poolFloor); zero values use the package defaults.
	poolFactor, poolFloor int
	// now and newID are injectable for deterministic tests.
	now   func() time.Time
	newID func() string
}

// Option customizes a Service.
type Option func(*Service)

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithIDGenerator overrides ID generation (tests).
func WithIDGenerator(gen func() string) Option { return func(s *Service) { s.newID = gen } }

// WithConsolidator enables the opt-in LLM consolidation pipeline.
func WithConsolidator(c llm.Consolidator) Option { return func(s *Service) { s.consolidator = c } }

// WithConsolidateMode selects async (default), sync, or off.
func WithConsolidateMode(m ConsolidateMode) Option {
	return func(s *Service) { s.consolidateMode = m }
}

// WithConsolidateMinScore sets the similarity gate: the LLM is only consulted
// when the nearest candidate scores at least minScore. 0 disables the gate.
func WithConsolidateMinScore(minScore float64) Option {
	return func(s *Service) { s.consolidateMinScore = minScore }
}

// WithDistiller enables episodic→semantic promotion via RunPromoter.
func WithDistiller(d llm.Distiller) Option { return func(s *Service) { s.distiller = d } }

// WithPromoteMinAccess sets the minimum access_count for an episodic memory to
// be eligible for promotion.
func WithPromoteMinAccess(n int) Option { return func(s *Service) { s.promoteMinAccess = n } }

// WithMetrics installs an observability sink for consolidation events.
func WithMetrics(m Metrics) Option { return func(s *Service) { s.metrics = m } }

// WithSyncReinforce makes recall reinforcement run synchronously (tests).
func WithSyncReinforce() Option { return func(s *Service) { s.syncReinforce = true } }

// WithShortTermCap bounds short-term memories per namespace, enforced by fsck.
func WithShortTermCap(cap int) Option { return func(s *Service) { s.shortTermCap = cap } }

// WithQueryPrefix prepends an instruction to recall queries before embedding
// (e.g. the retrieval instruct expected by Qwen3-Embedding or bge models).
// Documents keep bare embeddings; the keyword leg keeps the raw query.
func WithQueryPrefix(p string) Option { return func(s *Service) { s.queryPrefix = p } }

// WithScoreFusion switches hybrid recall from rank fusion (RRF) to convex-
// combination score fusion, weighting the vector leg by alpha and the keyword
// leg by 1-alpha. alpha < 0 keeps RRF (the default).
func WithScoreFusion(alpha float64) Option { return func(s *Service) { s.scoreFusionAlpha = alpha } }

// WithRecallPool overrides the per-leg candidate pool sizing
// (max(k*factor, floor)) for hybrid recall. Non-positive values keep the
// defaults. Used by the benchmark harness to sweep pool depth.
func WithRecallPool(factor, floor int) Option {
	return func(s *Service) {
		if factor > 0 {
			s.poolFactor = factor
		}
		if floor > 0 {
			s.poolFloor = floor
		}
	}
}

// New builds a Service from a store and embedder.
func New(st store.Store, e embed.Embedder, opts ...Option) *Service {
	s := &Service{
		store:               st,
		embedder:            e,
		consolidateMode:     ConsolidateAsync,
		consolidateMinScore: 0.6,
		promoteMinAccess:    3,
		scoreFusionAlpha:    search.DefaultFusionAlpha, // convex score fusion by default; negative selects RRF
		poolFactor:          recallPoolFactor,
		poolFloor:           recallPoolFloor,
		metrics:             nopMetrics{},
		now:                 func() time.Time { return time.Now().UTC() },
		newID:               func() string { return uuid.NewString() },
	}
	for _, o := range opts {
		o(s)
	}
	if s.metrics == nil {
		s.metrics = nopMetrics{}
	}
	// The async queue exists only when consolidation can actually run.
	if s.consolidator != nil && s.consolidateMode == ConsolidateAsync {
		s.consolidateQueue = make(chan consolidateJob, consolidateQueueCap)
	}
	return s
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

// RememberInput describes a memory to store. Only Namespace and Content are
// required; Tier defaults to working and TTL to the tier default.
type RememberInput struct {
	Namespace  string
	Content    string
	Tier       memory.Tier
	Summary    string
	Tags       []string
	Metadata   map[string]any
	Importance float64
	// TTL overrides the tier default. A negative TTL means "never expire".
	TTL *time.Duration
	// ID upserts an existing memory when set; otherwise a new ID is generated.
	ID string
}

// Remember embeds and stores a memory, returning the persisted record.
func (s *Service) Remember(ctx context.Context, in RememberInput) (*memory.Memory, error) {
	start := time.Now()
	defer func() {
		s.metrics.OpDuration("remember", time.Since(start))
	}()
	if in.Namespace == "" {
		s.metrics.RememberResult("error", "")
		return nil, fmt.Errorf("remember: namespace is required")
	}
	if in.Content == "" {
		s.metrics.RememberResult("error", "")
		return nil, fmt.Errorf("remember: content is required")
	}
	tier := in.Tier
	if tier == "" {
		tier = memory.TierWorking
	}
	if !tier.Valid() {
		s.metrics.RememberResult("error", string(tier))
		return nil, fmt.Errorf("remember: invalid tier %q", tier)
	}

	vec, err := embed.EmbedOne(ctx, s.embedder, in.Content)
	if err != nil {
		s.metrics.RememberResult("error", string(tier))
		return nil, fmt.Errorf("remember: embed: %w", err)
	}

	now := s.now()
	id := in.ID
	if id == "" {
		id = s.newID()
	}
	m := &memory.Memory{
		ID:             id,
		Namespace:      in.Namespace,
		Tier:           tier,
		Content:        in.Content,
		Summary:        in.Summary,
		Tags:           in.Tags,
		Metadata:       in.Metadata,
		Importance:     in.Importance,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastAccessedAt: now,
		Embedding:      vec,
	}
	if exp := resolveExpiry(now, tier, in.TTL); exp != nil {
		m.ExpiresAt = exp
	}

	// Opt-in consolidation: on fresh writes to durable tiers, let the LLM dedup
	// or contradiction-resolve against existing memories.
	durable := tier == memory.TierSemantic || tier == memory.TierProcedural
	consolidate := in.ID == "" && s.consolidator != nil && durable && s.consolidateMode != ConsolidateOff

	// Sync mode resolves the write against existing memories before storing, so
	// the caller sees the consolidated result.
	if consolidate && s.consolidateMode == ConsolidateSync {
		if result, handled, err := s.consolidateSync(ctx, m); err != nil {
			s.metrics.RememberResult("error", string(tier))
			return nil, err
		} else if handled {
			s.metrics.RememberResult("ok", string(tier))
			return result, nil
		}
	}

	if err := s.store.Upsert(ctx, m); err != nil {
		s.metrics.RememberResult("error", string(tier))
		return nil, fmt.Errorf("remember: store: %w", err)
	}

	// Async mode stores immediately and consolidates in the background.
	if consolidate && s.consolidateMode == ConsolidateAsync {
		s.enqueueConsolidate(m.Namespace, m.ID)
	}
	s.metrics.RememberResult("ok", string(tier))
	return m, nil
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
		store.Filter{Tiers: []memory.Tier{memory.TierSemantic, memory.TierProcedural}}, limit)
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
			// live. If this also fails, the next fsck will flag the duplicate.
			_ = s.store.Delete(ctx, m.Namespace, m.ID)
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

// RecallInput describes a hybrid recall query.
type RecallInput struct {
	Namespace string
	Query     string
	Tiers     []memory.Tier
	Limit     int
	// IncludeExpired / IncludeSuperseded relax the default live-only filter.
	IncludeExpired    bool
	IncludeSuperseded bool
}

// Recall runs hybrid (vector + keyword) retrieval fused with RRF.
func (s *Service) Recall(ctx context.Context, in RecallInput) ([]store.Scored, error) {
	start := time.Now()
	tf := tierFilterLabel(in.Tiers)
	defer func() {
		s.metrics.OpDuration("recall", time.Since(start))
	}()
	if in.Namespace == "" {
		s.metrics.RecallResult("error", tf, "0")
		return nil, fmt.Errorf("recall: namespace is required")
	}
	if in.Query == "" {
		s.metrics.RecallResult("error", tf, "0")
		return nil, fmt.Errorf("recall: query is required")
	}
	k := in.Limit
	if k <= 0 {
		k = 10
	}
	filter := store.Filter{
		Tiers:             in.Tiers,
		IncludeExpired:    in.IncludeExpired,
		IncludeSuperseded: in.IncludeSuperseded,
	}

	vec, err := embed.EmbedOne(ctx, s.embedder, s.queryPrefix+in.Query)
	if err != nil {
		s.metrics.RecallResult("error", tf, "0")
		return nil, fmt.Errorf("recall: embed: %w", err)
	}

	// Over-fetch a deep candidate pool from each strategy: a memory ranked just
	// outside the top k in both legs is invisible at pool depth k, yet RRF would
	// rank it above single-leg hits. Fusion, re-rank, and dedup then cut the
	// pool back down to k.
	poolK := max(k*s.poolFactor, s.poolFloor)
	vres, err := s.store.VectorSearch(ctx, in.Namespace, vec, filter, poolK)
	if err != nil {
		s.metrics.RecallResult("error", tf, "0")
		return nil, fmt.Errorf("recall: vector search: %w", err)
	}
	kres, err := s.store.KeywordSearch(ctx, in.Namespace, in.Query, filter, poolK)
	if err != nil {
		s.metrics.RecallResult("error", tf, "0")
		return nil, fmt.Errorf("recall: keyword search: %w", err)
	}
	// Fuse (no truncation), re-rank by composite relevance/recency/importance,
	// drop near-duplicates, then cap at k.
	var fused []store.Scored
	if s.scoreFusionAlpha >= 0 {
		fused = search.FuseScores([][]store.Scored{vres, kres},
			[]float64{s.scoreFusionAlpha, 1 - s.scoreFusionAlpha}, 0)
	} else {
		fused = search.Fuse([][]store.Scored{vres, kres}, 0, search.DefaultRRFK)
	}
	ranked := search.Rerank(fused, s.now())
	results := search.Dedup(ranked, k)
	s.reinforceResults(ctx, in.Namespace, results)
	s.metrics.RecallResult("ok", tf, hitsBucket(len(results)))
	return results, nil
}

// reinforceResults records that recalled memories were used. By default it runs
// in the background so recall latency excludes the reinforcement writes; tests
// can force synchronous behaviour with WithSyncReinforce.
func (s *Service) reinforceResults(ctx context.Context, namespace string, results []store.Scored) {
	if s.syncReinforce {
		s.reinforce(ctx, namespace, results)
		return
	}
	// Detach from the request lifetime but keep its values; bound the work.
	bg := context.WithoutCancel(ctx)
	go func() {
		rctx, cancel := context.WithTimeout(bg, reinforceTimeout)
		defer cancel()
		s.reinforce(rctx, namespace, results)
	}()
}

// reinforce records that recalled memories were just used: it bumps their
// access stats and slides the TTL forward for short-term tiers, so frequently
// recalled memories don't decay. Best-effort — a failure never fails the recall.
func (s *Service) reinforce(ctx context.Context, namespace string, results []store.Scored) {
	now := s.now()
	byTier := map[memory.Tier][]string{}
	for _, r := range results {
		byTier[r.Memory.Tier] = append(byTier[r.Memory.Tier], r.Memory.ID)
	}
	for tier, ids := range byTier {
		ttl := tier.DefaultTTL()
		var newExpiry *time.Time
		if ttl > 0 { // short-term tiers slide their expiry forward on use
			t := now.Add(ttl)
			newExpiry = &t
		}
		_ = s.store.Reinforce(ctx, namespace, ids, now, newExpiry)
	}
}

// Get returns a single memory by ID.
func (s *Service) Get(ctx context.Context, namespace, id string) (*memory.Memory, error) {
	return s.store.Get(ctx, namespace, id)
}

// Forget deletes a memory by ID.
func (s *Service) Forget(ctx context.Context, namespace, id string) error {
	start := time.Now()
	defer func() { s.metrics.OpDuration("forget", time.Since(start)) }()
	err := s.store.Delete(ctx, namespace, id)
	switch {
	case err == nil:
		s.metrics.ForgetResult("ok")
	case errors.Is(err, store.ErrNotFound):
		s.metrics.ForgetResult("not_found")
	default:
		s.metrics.ForgetResult("error")
	}
	return err
}

// Fsck runs a consistency sweep: purge expired, enforce the short-term cap, and
// audit live memories for duplicate clusters.
func (s *Service) Fsck(ctx context.Context) (maintenance.Report, error) {
	start := time.Now()
	defer func() { s.metrics.OpDuration("fsck", time.Since(start)) }()
	rep, err := maintenance.Fsck(ctx, s.store, s.shortTermCap, s.now())
	if err != nil {
		s.metrics.FsckResult("error")
		return rep, err
	}
	s.metrics.FsckResult("ok")
	return rep, nil
}

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
		eps, err := s.store.List(ctx, ns, store.Filter{Tiers: []memory.Tier{memory.TierEpisodic}}, 0)
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

// resolveExpiry computes the absolute expiry from an optional TTL override and
// the tier default. A negative override disables expiry.
func resolveExpiry(now time.Time, tier memory.Tier, ttl *time.Duration) *time.Time {
	d := tier.DefaultTTL()
	if ttl != nil {
		if *ttl < 0 {
			return nil
		}
		d = *ttl
	}
	if d <= 0 {
		return nil
	}
	t := now.Add(d)
	return &t
}

// tierFilterLabel returns a low-cardinality label summarising the tier
// filter a recall was issued with. Mixed means more than one distinct
// non-empty value; all means none.
func tierFilterLabel(tiers []memory.Tier) string {
	switch len(tiers) {
	case 0:
		return "all"
	case 1:
		return string(tiers[0])
	default:
		return "mixed"
	}
}

// hitsBucket returns a coarse bucket of n for use as a Prometheus label.
func hitsBucket(n int) string {
	switch {
	case n <= 0:
		return "0"
	case n == 1:
		return "1"
	case n <= 5:
		return "2-5"
	case n <= 20:
		return "6-20"
	default:
		return "21+"
	}
}
