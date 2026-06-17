package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/redact"
	"github.com/eleboucher/memini/internal/rerank"
	"github.com/eleboucher/memini/internal/search"
	"github.com/eleboucher/memini/internal/store"
)

// ttlSecondsMetaKey records a memory's caller-configured TTL (in seconds) so
// reinforcement can slide its expiry by the intended lifetime rather than the
// tier default. Only set when the caller supplied a custom positive TTL.
const ttlSecondsMetaKey = "ttl_seconds"

// Recall tuning.
const (
	reinforceTimeout = 10 * time.Second

	// recallPoolFactor / recallPoolFloor size the per-leg candidate pool for
	// hybrid recall: each leg over-fetches max(k*factor, floor) so a memory
	// ranked just outside the top k in both legs can still win after RRF fusion.
	recallPoolFactor = 5
	recallPoolFloor  = 50

	// maxRecallLimit caps a recall's result count, since poolK over-fetches
	// k*recallPoolFactor per leg per namespace — an unbounded limit is a cheap
	// way to amplify load.
	maxRecallLimit = 100

	// recallSearchConcurrency caps how many search legs run at once across all
	// namespaces. A subtree recall fans out two legs per namespace; this bound
	// keeps a deep subtree from exhausting the store's connection pool.
	recallSearchConcurrency = 16
)

// RecallPoolSize is the per-leg candidate pool Recall over-fetches for a
// final result count of k, with the default pool sizing. Exported so external
// pipelines (bench) that re-create recall stage-by-stage match production
// instead of hardcoding the constants.
func RecallPoolSize(k int) int { return max(k*recallPoolFactor, recallPoolFloor) }

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
	// OpDuration observes end-to-end latency for a public operation
	// (e.g. "recall", "answer").
	OpDuration(op string, d time.Duration)
	// AnswerResult records one Answer call: "ok" or "error".
	AnswerResult(result string)
	// RerankResult records one recall rerank attempt: backend is the reranker's
	// label ("llm"|"cross_encoder"); result is "ok" or "fallback".
	RerankResult(backend, result string)
	// RecallDegraded records one recall that fell back to keyword-only search
	// because the query embed failed or timed out. reason is "embed_timeout" or
	// "embed_error".
	RecallDegraded(reason string)
	// ReinforceResult records one best-effort recall reinforcement write:
	// "ok" or "error".
	ReinforceResult(result string)
	// DedupTombstoned records the total memories tombstoned by one one-shot
	// Service.Dedup call. Called once per call.
	DedupTombstoned(n int)
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
func (nopMetrics) AnswerResult(string)                 {}
func (nopMetrics) RerankResult(string, string)         {}
func (nopMetrics) RecallDegraded(string)               {}
func (nopMetrics) ReinforceResult(string)              {}
func (nopMetrics) DedupTombstoned(int)                 {}

// ErrInvalidInput marks errors caused by the caller's request (missing fields,
// unknown tiers) as opposed to backend failures. API layers map it to 400;
// anything else is a server-side error.
var ErrInvalidInput = errors.New("invalid input")

// invalidInputf builds a caller-input validation error wrapping ErrInvalidInput.
func invalidInputf(format string, args ...any) error {
	return fmt.Errorf(format+": %w", append(args, ErrInvalidInput)...)
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
	// consolidateQueueCap bounds the async consolidation queue; 0 uses the
	// default. Writes still succeed when the queue is full — the job is dropped
	// (counted by the "dropped" consolidate metric), never the memory.
	consolidateQueueCap int
	// consolidateQueue carries background jobs in async mode; nil otherwise.
	consolidateQueue chan consolidateJob

	// answerer is optional; when set, Answer recalls memories and asks it to
	// generate a grounded answer from them.
	answerer llm.Completer

	// reranker, when set, reorders the top k composite-ranked recall candidates
	// (with an LLM or a cross-encoder model) before truncating to the limit.
	// rerankName labels the backend for metrics. Adds one reranker call per
	// Recall, so it is opt-in (see WithReranker); failures fall back to the
	// composite order.
	reranker   rerank.Reranker
	rerankName string
	// rerankTimeout bounds the reranker call; past it, recall falls back to
	// composite order instead of stalling on a slow backend.
	rerankTimeout time.Duration
	// recallEmbedTimeout bounds the query embed on the recall path; past it, or on
	// any embed error, recall degrades to keyword-only search instead of failing.
	// 0 keeps the query embed unbounded and an embed error fatal.
	recallEmbedTimeout time.Duration
	// recallMinScore is an absolute relevance floor on the fused score; candidates
	// below it are dropped before composite re-ranking. 0 disables filtering.
	recallMinScore float64

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
	// writeDedupMinScore coalesces a fresh write into an existing same-tier
	// memory at or above this similarity instead of storing a near-duplicate,
	// but only when the LLM consolidation pipeline isn't handling the write.
	// 0 (the default) disables it.
	writeDedupMinScore float64
	// fingerprintDedup (default on) reinforces an exact restatement instead of
	// storing a duplicate; see WithFingerprintDedup.
	fingerprintDedup bool
	// redactSecrets (default on) scrubs live credentials from a memory's
	// Content/Summary/Metadata at ingestion, so a database compromise exposes
	// memory content but no usable tokens/keys. See WithSecretRedaction.
	redactSecrets bool
	// temporalBoost (> 0) enables query-conditioned temporal targeting in the
	// re-ranker; temporalAnchor resolves a query's relative-time reference (the
	// regex extractor by default, or an LLM extractor when configured).
	temporalBoost  float64
	temporalAnchor search.AnchorExtractor
	// now and newID are injectable for deterministic tests.
	now   func() time.Time
	newID func() string

	// bg tracks detached best-effort goroutines (async recall reinforcement) so
	// WaitBackground can join them before the store is closed.
	bg sync.WaitGroup
}

// WaitBackground blocks until detached background goroutines (async recall
// reinforcement) finish. Call during shutdown, after the workers have been
// stopped and before closing the store.
func (s *Service) WaitBackground() { s.bg.Wait() }

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

// WithConsolidateQueueCap bounds the async consolidation queue. n <= 0 uses the
// default. Raise it for write-bursty deployments to reduce dropped jobs (a
// dropped job loses the dedup pass, never the memory).
func WithConsolidateQueueCap(n int) Option {
	return func(s *Service) { s.consolidateQueueCap = n }
}

// WithDistiller enables episodic→semantic promotion via RunPromoter.
func WithDistiller(d llm.Distiller) Option { return func(s *Service) { s.distiller = d } }

// WithAnswerer enables Answer: recall memories, then generate a grounded answer
// from them with this chat client.
func WithAnswerer(c llm.Completer) Option { return func(s *Service) { s.answerer = c } }

// defaultRerankTimeout bounds a single reranker call; past it, recall falls
// back to composite order.
const defaultRerankTimeout = 10 * time.Second

// rerankResponseMargin is held back from a caller's deadline when bounding the
// rerank, so the result (composite order on fallback) still has time to reach
// the caller before its own deadline fires.
const rerankResponseMargin = 250 * time.Millisecond

// WithReranker enables reranking of recall candidates: after composite ranking,
// the top k candidates are reordered by the reranker (an LLM or cross-encoder
// model), then truncated to the limit. name labels the backend in metrics. It
// adds one reranker call per Recall, so it is opt-in; a failed rerank falls back
// to the composite order.
func WithReranker(r rerank.Reranker, name string) Option {
	return func(s *Service) {
		s.reranker = r
		s.rerankName = name
	}
}

// WithRerankTimeout bounds a single reranker call; at the deadline recall
// degrades to composite order. d <= 0 keeps the default.
func WithRerankTimeout(d time.Duration) Option {
	return func(s *Service) {
		if d > 0 {
			s.rerankTimeout = d
		}
	}
}

// WithRecallEmbedTimeout bounds the query embed on the recall path. Past the
// deadline, or on any embed error, recall degrades to keyword-only search
// rather than failing or stalling on a slow embeddings backend. d <= 0 keeps
// the query embed unbounded and an embed error fatal (the default).
func WithRecallEmbedTimeout(d time.Duration) Option {
	return func(s *Service) {
		if d > 0 {
			s.recallEmbedTimeout = d
		}
	}
}

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

// WithScoreFusion sets the hybrid fusion weight: the vector leg by alpha and
// the keyword leg by 1-alpha (score fusion). alpha < 0 selects rank fusion
// (RRF). The package default is score fusion at DefaultFusionAlpha.
func WithScoreFusion(alpha float64) Option { return func(s *Service) { s.scoreFusionAlpha = alpha } }

// WithRecallMinScore sets an absolute relevance floor on the fused score:
// candidates below the threshold are dropped before composite re-ranking and
// before the reranker. 0 (the default) disables filtering. For score fusion
// (alpha >= 0) the fused score is in [0,1]; for RRF it is a small rank-based
// value (~0.016 for the top position). See MEMINI_RECALL_MIN_SCORE.
func WithRecallMinScore(minScore float64) Option {
	return func(s *Service) { s.recallMinScore = minScore }
}

// WithTemporalTargeting enables temporal targeting in the re-ranker: when a
// query names a relative time, candidates dated near the referenced point are
// boosted by up to `boost` on the composite score. ex resolves the reference
// (use search.RegexAnchorExtractor{} for the no-LLM default). boost <= 0 or a
// nil extractor disables it.
func WithTemporalTargeting(boost float64, ex search.AnchorExtractor) Option {
	return func(s *Service) { s.temporalBoost = boost; s.temporalAnchor = ex }
}

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

// WithWriteDedup coalesces a fresh write into an existing same-tier memory when
// their vector similarity is at or above minScore, instead of storing a
// near-duplicate. It only acts when the LLM consolidation pipeline is not
// handling the write (no consolidator, a non-durable tier, or consolidation
// off), giving headless deployments basic corpus hygiene. 0 disables it.
func WithWriteDedup(minScore float64) Option {
	return func(s *Service) { s.writeDedupMinScore = minScore }
}

// WithFingerprintDedup toggles exact-restatement dedup: when on (the default), a
// fresh write whose normalized content exactly matches a live same-tier memory
// reinforces that memory instead of storing a duplicate, without embedding it.
// It is independent of WithWriteDedup (the fuzzy vector gate) and the LLM
// consolidation pipeline.
func WithFingerprintDedup(on bool) Option {
	return func(s *Service) { s.fingerprintDedup = on }
}

// WithSecretRedaction toggles server-side scrubbing of live credentials from a
// memory's Content/Summary/Metadata at ingestion (on by default). It bounds a
// database compromise to information disclosure — leaked memory holds no usable
// tokens, keys, or passwords. Disable only if redaction mangles legitimate
// content; storing raw secrets re-opens the lateral-movement risk.
func WithSecretRedaction(on bool) Option {
	return func(s *Service) { s.redactSecrets = on }
}

// New builds a Service from a store and embedder.
func New(st store.Store, e embed.Embedder, opts ...Option) *Service {
	s := &Service{
		store:               st,
		embedder:            e,
		consolidateMode:     ConsolidateAsync,
		consolidateMinScore: 0.6,
		promoteMinAccess:    3,
		rerankTimeout:       defaultRerankTimeout,
		scoreFusionAlpha:    search.DefaultFusionAlpha, // convex score fusion by default; negative selects RRF
		poolFactor:          recallPoolFactor,
		poolFloor:           recallPoolFloor,
		fingerprintDedup:    true,
		redactSecrets:       true,
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
		cap := s.consolidateQueueCap
		if cap <= 0 {
			cap = defaultConsolidateQueueCap
		}
		s.consolidateQueue = make(chan consolidateJob, cap)
	}
	return s
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
	// Confidence overrides the seed corroboration for a durable fact (e.g. a
	// trusted import). nil uses the default seed. Ignored for short-term tiers.
	Confidence *float64
	// ValidFrom / ValidTo set the interval the fact was true, for recording
	// historical facts that time-travel (AsOf) recall can surface. ValidFrom
	// defaults to now (or the existing row on update); ValidTo defaults to open.
	ValidFrom *time.Time
	ValidTo   *time.Time
}

// scrubInput redacts live credentials from a write before it is persisted, so a
// database compromise yields no usable tokens/keys. A no-op when redaction is
// disabled (WithSecretRedaction(false)).
func (s *Service) scrubInput(in RememberInput) RememberInput {
	if !s.redactSecrets {
		return in
	}
	in.Content = redact.Secrets(in.Content)
	in.Summary = redact.Secrets(in.Summary)
	in.Metadata = redact.Metadata(in.Metadata)
	return in
}

// Remember embeds and stores a memory, returning the persisted record.
func (s *Service) Remember(ctx context.Context, in RememberInput) (*memory.Memory, error) {
	start := time.Now()
	defer func() {
		s.metrics.OpDuration("remember", time.Since(start))
	}()
	if in.Namespace == "" {
		s.metrics.RememberResult("error", "")
		return nil, invalidInputf("remember: namespace is required")
	}
	if in.Content == "" {
		s.metrics.RememberResult("error", "")
		return nil, invalidInputf("remember: content is required")
	}
	tier := in.Tier
	if tier == "" {
		tier = memory.TierWorking
	}
	if !tier.Valid() {
		s.metrics.RememberResult("error", string(tier))
		return nil, invalidInputf("remember: invalid tier %q", tier)
	}

	// Scrub live credentials before anything persists them — content, the
	// embedding, and the dedup fingerprint are all computed on the redacted
	// text, so a leaked database yields no usable tokens/keys.
	in = s.scrubInput(in)

	// Exact-restatement fast path: a fresh write whose normalized content already
	// exists live in this tier reinforces that memory instead of duplicating it,
	// before embedding so an exact repeat costs no embedder call.
	if existing, ok := s.fingerprintHit(ctx, in, tier); ok {
		s.metrics.RememberResult("ok", string(tier))
		return existing, nil
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
	// An update by ID preserves the existing row's validity start and confidence;
	// load it so the upsert below doesn't reset them.
	var existing *memory.Memory
	if in.ID != "" {
		ex, gerr := s.store.Get(ctx, in.Namespace, in.ID)
		switch {
		case gerr == nil:
			existing = ex
		case errors.Is(gerr, store.ErrNotFound):
		default:
			s.metrics.RememberResult("error", string(tier))
			return nil, fmt.Errorf("remember: load existing: %w", gerr)
		}
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
		markCustomTTL(m, in.TTL)
	}
	resolveValidity(m, existing, in, now)
	// Durable facts start uncorroborated and earn trust as they recur; an
	// explicit value wins, and an update keeps the existing confidence.
	if tier.Term() == memory.LongTerm {
		switch {
		case in.Confidence != nil:
			c := *in.Confidence
			m.Confidence = &c
		case existing != nil:
			m.Confidence = existing.Confidence
		default:
			c := memory.ConfidenceSeedFresh
			m.Confidence = &c
		}
	}

	// Opt-in consolidation: on fresh writes to durable tiers, let the LLM dedup
	// or contradiction-resolve against existing memories.
	durable := tier == memory.TierSemantic || tier == memory.TierProcedural
	consolidate := in.ID == "" && s.consolidator != nil && durable && s.consolidateMode != ConsolidateOff

	// Write-time dedup (non-LLM corpus hygiene): when the consolidation pipeline
	// isn't handling this write, coalesce a near-identical repeat into the
	// existing memory instead of storing a duplicate.
	if in.ID == "" && !consolidate && s.writeDedupMinScore > 0 {
		if existing := s.dedupExisting(ctx, m); existing != nil {
			s.metrics.RememberResult("ok", string(tier))
			return existing, nil
		}
	}

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

// dedupExisting returns the nearest same-tier memory when its similarity is at
// or above writeDedupMinScore, after reinforcing it (so the repeat refreshes
// its recency/TTL). The caller coalesces the write into that record instead of
// storing a duplicate. It never supersedes or rewrites the existing memory.
// Returns nil to fall through to a normal insert.
func (s *Service) dedupExisting(ctx context.Context, m *memory.Memory) *memory.Memory {
	cands, err := s.store.VectorSearch(ctx, m.Namespace, m.Embedding,
		store.Filter{Tiers: []memory.Tier{m.Tier}, Now: s.now()}, 1)
	if err != nil {
		// Falling through to a plain insert is right, but a persistent vector
		// search problem means duplicates quietly accumulate — say so.
		slog.WarnContext(ctx, "remember: dedup search failed, storing without dedup",
			"namespace", m.Namespace, "err", err)
		return nil
	}
	if len(cands) == 0 || cands[0].Score < s.writeDedupMinScore {
		return nil
	}
	existing := cands[0].Memory
	s.reinforce(ctx, []store.Scored{{Memory: existing}})
	s.corroborate(ctx, existing) // a near-identical repeat raises the fact's confidence
	return existing
}

// fingerprintHit returns a live same-tier memory whose normalized content
// matches in.Content exactly, reinforced and corroborated, when fingerprint
// dedup applies. ok is false (fall through to a normal write) for an update by
// ID, when dedup is off, when nothing matches, or when the lookup fails. It
// never rewrites the existing memory.
func (s *Service) fingerprintHit(ctx context.Context, in RememberInput, tier memory.Tier) (*memory.Memory, bool) {
	if in.ID != "" || !s.fingerprintDedup {
		return nil, false
	}
	existing, err := s.store.GetByFingerprint(ctx, in.Namespace, tier, memory.Fingerprint(in.Content), s.now())
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "remember: fingerprint lookup failed, storing without dedup",
				"namespace", in.Namespace, "err", err)
		}
		return nil, false
	}
	s.reinforce(ctx, []store.Scored{{Memory: existing}})
	s.corroborate(ctx, existing) // an exact repeat raises the fact's confidence
	return existing, true
}

// resolveValidity sets m's validity window: an explicit ValidFrom/ValidTo wins
// (for backdating a historical fact), else an update keeps the existing row's
// bounds, else ValidFrom starts now and ValidTo is open ("still true").
func resolveValidity(m, existing *memory.Memory, in RememberInput, now time.Time) {
	m.ValidFrom = &now
	if existing != nil {
		m.ValidFrom = existing.ValidFrom
		m.ValidTo = existing.ValidTo
	}
	if in.ValidFrom != nil {
		vf := in.ValidFrom.UTC()
		m.ValidFrom = &vf
	}
	if in.ValidTo != nil {
		vt := in.ValidTo.UTC()
		m.ValidTo = &vt
	}
}

// corroborate raises a durable memory's confidence one logistic step, because
// it was just re-observed. It works from the lazily-decayed current confidence
// and persists the result (which resets the decay baseline). No-op for
// short-term memories or ones without tracked confidence. Best-effort: a failure
// is logged, never surfaced to the caller.
func (s *Service) corroborate(ctx context.Context, m *memory.Memory) {
	if m.Tier.Term() != memory.LongTerm || m.Confidence == nil {
		return
	}
	now := s.now()
	grown := memory.GrowConfidence(m.EffectiveConfidence(now))
	if err := s.store.SetConfidence(ctx, m.Namespace, m.ID, grown, now); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "corroborate: set confidence failed",
			"namespace", m.Namespace, "id", m.ID, "err", err)
	}
}

// RecallInput describes a hybrid recall query.
type RecallInput struct {
	Namespace string
	Query     string
	Tiers     []memory.Tier
	// Tags narrows recall to memories carrying every listed tag (AND).
	Tags []string
	// Metadata narrows recall to memories whose top-level metadata contains each
	// listed key=value string pair (AND).
	Metadata map[string]string
	// ExcludeMetadata drops memories whose top-level metadata contains every
	// listed key=value pair (AND), applied after Metadata. Lets a caller exclude
	// its own session's just-captured turns from auto-recall.
	ExcludeMetadata map[string]string
	Limit           int
	// IncludeExpired / IncludeSuperseded relax the default live-only filter.
	IncludeExpired    bool
	IncludeSuperseded bool
	// AsOf, when non-zero, runs time-travel recall: it returns the facts whose
	// validity window contained AsOf (including ones since superseded), instead
	// of only currently-live memories.
	AsOf time.Time
	// Subtree expands recall to Namespace and every namespace nested under it
	// ("project" also reads "project/agent-a", "project/agent-b", ...), for the
	// multi-agent "read shared + private" pattern. Default (false) is exact scope,
	// so cross-agent recall never happens unless asked for.
	Subtree bool
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
		return nil, invalidInputf("recall: namespace is required")
	}
	if in.Query == "" {
		s.metrics.RecallResult("error", tf, "0")
		return nil, invalidInputf("recall: query is required")
	}
	k := in.Limit
	if k <= 0 {
		k = 10
	}
	if k > maxRecallLimit {
		k = maxRecallLimit
	}
	filter := store.Filter{
		Tiers:             in.Tiers,
		Tags:              in.Tags,
		Metadata:          in.Metadata,
		ExcludeMetadata:   in.ExcludeMetadata,
		IncludeExpired:    in.IncludeExpired,
		IncludeSuperseded: in.IncludeSuperseded,
		Now:               s.now(),
		AsOf:              in.AsOf,
	}

	// Embed the query and resolve namespaces concurrently: two independent
	// blocking calls, so overlapping them keeps only the slower on the critical
	// path. Namespaces default to in.Namespace; a subtree recall adds everything
	// nested under it.
	var vec []float32
	var embedErr error
	namespaces := []string{in.Namespace}
	embedStart := time.Now()
	g1, g1ctx := errgroup.WithContext(ctx)
	g1.Go(func() error {
		ectx := g1ctx
		if s.recallEmbedTimeout > 0 {
			var cancel context.CancelFunc
			ectx, cancel = context.WithTimeout(g1ctx, s.recallEmbedTimeout)
			defer cancel()
		}
		v, err := embed.EmbedOne(ectx, s.embedder, s.queryPrefix+in.Query)
		if err != nil {
			// With an embed budget set, recall is best-effort: capture the error
			// and degrade to keyword-only rather than aborting the group.
			if s.recallEmbedTimeout > 0 {
				embedErr = err
				return nil
			}
			return fmt.Errorf("recall: embed: %w", err)
		}
		vec = v
		return nil
	})
	if in.Subtree {
		g1.Go(func() error {
			ns, err := s.subtreeNamespaces(g1ctx, in.Namespace)
			if err != nil {
				return fmt.Errorf("recall: resolve subtree: %w", err)
			}
			namespaces = ns
			return nil
		})
	}
	if err := g1.Wait(); err != nil {
		s.metrics.RecallResult("error", tf, "0")
		return nil, err
	}
	s.metrics.OpDuration("recall_embed", time.Since(embedStart))
	if embedErr != nil {
		reason := "embed_error"
		if errors.Is(embedErr, context.DeadlineExceeded) {
			reason = "embed_timeout"
		}
		slog.WarnContext(ctx, "recall: query embed failed, falling back to keyword-only search", "reason", reason, "err", embedErr)
		s.metrics.RecallDegraded(reason)
	}

	// Over-fetch a deep candidate pool from each strategy: a memory ranked just
	// outside the top k in both legs is invisible at pool depth k, yet RRF would
	// rank it above single-leg hits. Fusion, re-rank, and dedup then cut the
	// pool back down to k.
	poolK := max(k*s.poolFactor, s.poolFloor)

	// Fuse the vector and keyword legs of one namespace into a single best-first
	// list (RRF by default, or convex score fusion).
	fuseLegs := func(v, kw []store.Scored) []store.Scored {
		if s.scoreFusionAlpha >= 0 {
			return search.FuseScores([][]store.Scored{v, kw},
				[]float64{s.scoreFusionAlpha, 1 - s.scoreFusionAlpha}, 0)
		}
		return search.Fuse([][]store.Scored{v, kw}, 0, search.DefaultRRFK)
	}

	// Run both legs of every namespace concurrently into pre-sized,
	// index-addressed slots so there is no shared append to guard. SetLimit caps
	// in-flight store calls so a deep subtree can't exhaust the connection pool.
	searchStart := time.Now()
	vres := make([][]store.Scored, len(namespaces))
	kres := make([][]store.Scored, len(namespaces))
	g2, g2ctx := errgroup.WithContext(ctx)
	g2.SetLimit(recallSearchConcurrency)
	for i, ns := range namespaces {
		if vec != nil {
			g2.Go(func() error {
				v, err := s.store.VectorSearch(g2ctx, ns, vec, filter, poolK)
				if err != nil {
					return fmt.Errorf("recall: vector search: %w", err)
				}
				vres[i] = v
				return nil
			})
		}
		g2.Go(func() error {
			kw, err := s.store.KeywordSearch(g2ctx, ns, in.Query, filter, poolK)
			if err != nil {
				return fmt.Errorf("recall: keyword search: %w", err)
			}
			kres[i] = kw
			return nil
		})
	}
	if err := g2.Wait(); err != nil {
		s.metrics.RecallResult("error", tf, "0")
		return nil, err
	}
	s.metrics.OpDuration("recall_search", time.Since(searchStart))

	perNS := make([][]store.Scored, len(namespaces))
	for i := range namespaces {
		perNS[i] = fuseLegs(vres[i], kres[i])
	}

	// Merge the per-namespace lists so each namespace's hits rank by their own
	// position rather than their offset in a concatenated slice. Honor the
	// configured fusion mode: score fusion (alpha >= 0) keeps scores in [0,1]
	// so the min-score gate below is meaningful; RRF preserves rank-only
	// fusion and is paired with a skipped gate.
	var fused []store.Scored
	if len(perNS) == 1 {
		fused = perNS[0]
	} else if s.scoreFusionAlpha >= 0 {
		fused = search.FuseScores(perNS, nil, 0)
	} else {
		fused = search.Fuse(perNS, 0, search.DefaultRRFK)
	}
	// Absolute relevance gate: drop candidates whose fused score is below the
	// threshold, preventing the "poison" problem where irrelevant memories are
	// min-max normalised into competitive values. Applied before composite
	// re-ranking and before the reranker, so low-scoring candidates never
	// consume the reranker's budget or pollute the result. Only meaningful for
	// score fusion (RRF scores are not comparable to the [0,1] threshold).
	if s.scoreFusionAlpha >= 0 && s.recallMinScore > 0 {
		filtered := make([]store.Scored, 0, len(fused))
		for _, r := range fused {
			if r.Score >= s.recallMinScore {
				filtered = append(filtered, r)
			}
		}
		fused = filtered
	}
	var ranked []store.Scored
	if s.temporalBoost > 0 && s.temporalAnchor != nil {
		ranked = search.RerankTemporal(fused, in.Query, s.now(),
			search.DefaultRerankWeights, s.temporalAnchor, s.temporalBoost)
	} else {
		ranked = search.Rerank(fused, s.now())
	}
	results := s.finalizeRecall(ctx, in.Query, ranked, k)
	s.reinforceResults(ctx, results)
	s.metrics.RecallResult("ok", tf, hitsBucket(len(results)))
	return results, nil
}

// subtreeNamespaces returns root and every namespace nested under it (root +
// "root/..."), the set a subtree recall searches.
func (s *Service) subtreeNamespaces(ctx context.Context, root string) ([]string, error) {
	all, err := s.store.ListNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	prefix := root + "/"
	out := []string{root}
	for _, ns := range all {
		if ns != root && strings.HasPrefix(ns, prefix) {
			out = append(out, ns)
		}
	}
	return out, nil
}

// finalizeRecall dedups the composite-ranked candidates to the result set. With
// no reranker it simply caps at k; with one it reorders the top k candidates by
// the reranker's verdict and returns up to k. A rerank failure falls back to
// the composite order so recall never errors on the reranker's account.
func (s *Service) finalizeRecall(ctx context.Context, query string, ranked []store.Scored, k int) []store.Scored {
	if s.reranker == nil {
		return search.Dedup(ranked, k)
	}
	pool := search.Dedup(ranked, k)
	cands := make([]rerank.Candidate, len(pool))
	for i, r := range pool {
		cands[i] = rerank.Candidate{ID: r.Memory.ID, Content: r.Memory.Content}
	}
	// Bound the rerank by the configured timeout and, when the caller imposed a
	// deadline, by the time left before it minus a response margin — whichever is
	// tighter. If no time remains, skip the rerank and keep composite order so the
	// caller gets a result before its deadline rather than after.
	budget := s.rerankTimeout
	if dl, ok := ctx.Deadline(); ok {
		rem := time.Until(dl) - rerankResponseMargin
		if rem <= 0 {
			slog.WarnContext(ctx, "recall: no time left to rerank, using composite order", "backend", s.rerankName)
			s.metrics.RerankResult(s.rerankName, "fallback")
			return search.Dedup(ranked, k)
		}
		if budget <= 0 || rem < budget {
			budget = rem
		}
	}
	rctx := ctx
	if budget > 0 {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	start := time.Now()
	order, err := s.reranker.Rerank(rctx, query, cands)
	s.metrics.OpDuration("rerank", time.Since(start))
	if err != nil {
		slog.WarnContext(ctx, "recall: rerank failed, using composite order", "backend", s.rerankName, "err", err)
		s.metrics.RerankResult(s.rerankName, "fallback")
		return search.Dedup(ranked, k)
	}
	byID := make(map[string]store.Scored, len(pool))
	for _, r := range pool {
		byID[r.Memory.ID] = r
	}
	out := make([]store.Scored, 0, len(order))
	for _, id := range order {
		if r, ok := byID[id]; ok {
			out = append(out, r)
			delete(byID, id)
		}
	}
	if len(out) == 0 {
		slog.WarnContext(ctx, "recall: rerank matched no candidates, using composite order", "backend", s.rerankName)
		s.metrics.RerankResult(s.rerankName, "fallback")
		return search.Dedup(ranked, k)
	}
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	s.metrics.RerankResult(s.rerankName, "ok")
	return out
}

// reinforceResults records that recalled memories were used. By default it runs
// in the background so recall latency excludes the reinforcement writes; tests
// can force synchronous behaviour with WithSyncReinforce.
func (s *Service) reinforceResults(ctx context.Context, results []store.Scored) {
	if s.syncReinforce {
		s.reinforce(ctx, results)
		return
	}
	// Detach from the request lifetime but keep its values; bound the work.
	bg := context.WithoutCancel(ctx)
	s.bg.Go(func() {
		rctx, cancel := context.WithTimeout(bg, reinforceTimeout)
		defer cancel()
		s.reinforce(rctx, results)
	})
}

// reinforce records that recalled memories were just used: it bumps their
// access stats and slides the expiry forward by each memory's own lifetime, so
// frequently recalled memories don't decay. Best-effort — a failure never fails
// the recall.
func (s *Service) reinforce(ctx context.Context, results []store.Scored) {
	now := s.now()
	// Group by (namespace, ttl): Reinforce is namespace-scoped and writes one new
	// expiry per call, so memories sliding by the same lifetime batch together.
	type key struct {
		ns  string
		ttl time.Duration
	}
	byKey := map[key][]string{}
	for _, r := range results {
		k := key{r.Memory.Namespace, reinforceTTL(r.Memory)}
		byKey[k] = append(byKey[k], r.Memory.ID)
	}
	for k, ids := range byKey {
		var newExpiry *time.Time
		if k.ttl > 0 { // expiring memories slide their expiry forward on use
			t := now.Add(k.ttl)
			newExpiry = &t
		}
		if err := s.store.Reinforce(ctx, k.ns, ids, now, newExpiry); err != nil {
			// Persistent failures here mean TTLs stop sliding and promotion
			// never fires, so make them observable even though the recall
			// itself must not fail.
			slog.WarnContext(ctx, "recall: reinforce failed",
				"namespace", k.ns, "ttl", k.ttl, "count", len(ids), "err", err)
			s.metrics.ReinforceResult("error")
			continue
		}
		s.metrics.ReinforceResult("ok")
	}
}

// markCustomTTL records a caller-supplied positive TTL on m (as metadata) so
// reinforcement can slide its expiry by the intended lifetime rather than the
// tier default. A nil or non-positive ttl records nothing.
func markCustomTTL(m *memory.Memory, ttl *time.Duration) {
	if ttl == nil || *ttl <= 0 {
		return
	}
	if m.Metadata == nil {
		m.Metadata = map[string]any{}
	}
	m.Metadata[ttlSecondsMetaKey] = int64(ttl.Seconds())
}

// reinforceTTL is the lifetime to slide a recalled memory's expiry by: the
// caller's configured TTL when one was recorded at write time (see
// ttlSecondsMetaKey), else the tier default. Returning 0 means "do not slide"
// (durable memories with no recorded TTL).
func reinforceTTL(m *memory.Memory) time.Duration {
	if m.Metadata != nil {
		if secs, ok := metaSeconds(m.Metadata[ttlSecondsMetaKey]); ok && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return m.Tier.DefaultTTL()
}

// metaSeconds reads a seconds count from a metadata value, tolerating the
// float64 that JSON round-tripping produces as well as integer types.
func metaSeconds(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	default:
		return 0, false
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

// ForgetByTag deletes every memory in a namespace carrying tag, including
// superseded and expired ones, and returns the count deleted. With the import
// provenance tag (import:<source>:<date>), this undoes a bulk import in one call.
func (s *Service) ForgetByTag(ctx context.Context, namespace, tag string) (int64, error) {
	start := time.Now()
	defer func() { s.metrics.OpDuration("forget_by_tag", time.Since(start)) }()
	if strings.TrimSpace(tag) == "" {
		return 0, invalidInputf("forget by tag: tag is required")
	}
	deleted, err := maintenance.ForgetByTag(ctx, s.store, namespace, tag)
	if err != nil {
		s.metrics.ForgetResult("error")
		return deleted, err
	}
	s.metrics.ForgetResult("ok")
	return deleted, nil
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
