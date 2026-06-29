package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/extract"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/redact"
	"github.com/eleboucher/memini/internal/rerank"
	"github.com/eleboucher/memini/internal/sanitize"
	"github.com/eleboucher/memini/internal/search"
	"github.com/eleboucher/memini/internal/store"
)

// ttlSecondsMetaKey records a memory's caller-configured TTL (in seconds) so
// reinforcement can slide its expiry by the intended lifetime rather than the
// tier default. Only set when the caller supplied a custom positive TTL.
const ttlSecondsMetaKey = "ttl_seconds"

// Session-marker id prefixes written by the Claude Code session-end / stop
// hooks (plugin/scripts/session-end.mjs, stop.mjs). See WithReinforceSkipMarkers.
const (
	sessionEndIDPrefix = "session-end:"
	stopIDPrefix       = "stop:"
)

// distillOnWriteTimeout bounds one write-time distillation (LLM call + fact
// writes); distillSemCap bounds concurrent write-time distillations.
const (
	distillOnWriteTimeout = 60 * time.Second
	distillSemCap         = 4
)

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
	// SupersedeResult records the outcome of a Supersede call:
	// "ok"|"not_found"|"error". Supersede tombstones a memory (sets
	// superseded_by) rather than deleting it; the maintenance sweeper
	// hard-deletes tombstoned rows after TombstoneTTL.
	SupersedeResult(result string)
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
	// WriteSanitized records one ingestion content-hygiene action: "cleaned"
	// (unambiguous corruption stripped from content) or "quarantined"
	// (script-salad downranked when corruption quarantine is enabled).
	WriteSanitized(action string)
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
func (nopMetrics) SupersedeResult(string)              {}
func (nopMetrics) PromoteResult(string, int)           {}
func (nopMetrics) FsckResult(string)                   {}
func (nopMetrics) OpDuration(string, time.Duration)    {}
func (nopMetrics) AnswerResult(string)                 {}
func (nopMetrics) RerankResult(string, string)         {}
func (nopMetrics) RecallDegraded(string)               {}
func (nopMetrics) WriteSanitized(string)               {}
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
	// recallMinSemanticScore is an absolute floor on the raw vector score
	// (1/(1+L2)): a candidate below it is excluded before fusion and the keyword
	// leg cannot reintroduce it, so a query with nothing semantically relevant
	// recalls empty. 0 disables it. The usable value is embedder-specific (see
	// docs/recall-relevance-gate-2026-06-20.md).
	recallMinSemanticScore float64
	// recallSemanticReserve guarantees up to N recall slots for durable tiers
	// (semantic/procedural), the rest by relevance. 0 disables it.
	recallSemanticReserve int
	// episodicMinChars drops an episodic write whose substantive content (role
	// scaffolding stripped) is below this many characters — the "keep going" /
	// "ok" chatter that otherwise dominates episodic memory. 0 disables it.
	episodicMinChars int

	// distillOnWrite distils each fresh episodic capture into durable facts at
	// write time, bounded by distillSem. Needs a distiller.
	distillOnWrite bool
	distillSem     chan struct{}
	// distillDropNoFact, with distillOnWrite, deletes the episodic when
	// distillation yields no durable fact (the LLM becomes a write filter).
	distillDropNoFact bool
	// extractOnWrite runs each fresh episodic capture through the no-LLM heuristic
	// extractor (internal/extract). Only fires when no distiller is set.
	extractOnWrite bool
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
	// writeDedupScore is the similarity at/above which a write's nearest
	// same-tier memory triggers writeDedupAction. 0 disables write-time dedup.
	writeDedupScore float64
	// writeDedupAction is what happens at/above writeDedupScore: hint, coalesce,
	// supersede, or off. See WriteDedupAction.
	writeDedupAction WriteDedupAction
	// globalNamespace, when set, is merged read-only into every other
	// namespace's recall and briefing — durable tiers only. See
	// WithGlobalNamespace. Empty disables it.
	globalNamespace string
	// fingerprintDedup (default on) reinforces an exact restatement instead of
	// storing a duplicate; see WithFingerprintDedup.
	fingerprintDedup bool
	// reinforceSkipMarkers drops session markers from recall reinforcement;
	// see WithReinforceSkipMarkers.
	reinforceSkipMarkers bool
	// redactSecrets (default on) scrubs live credentials from a memory's
	// Content/Summary/Metadata at ingestion, so a database compromise exposes
	// memory content but no usable tokens/keys. See WithSecretRedaction.
	redactSecrets bool
	// quarantineGarbled (default off) downranks writes whose content looks like
	// script-salad — garbled multilingual model output. Flagged, not rejected:
	// importance is zeroed and metadata.quarantined set so the memory sinks in
	// recall but stays inspectable. See WithCorruptionQuarantine.
	quarantineGarbled bool
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
// value (~0.016 for the top position). Baked to 0.1 by the server; the
// benchmark harness overrides it via this Option.
func WithRecallMinScore(minScore float64) Option {
	return func(s *Service) { s.recallMinScore = minScore }
}

// WithRecallMinSemanticScore sets an absolute relevance floor on the raw vector
// (semantic) score: a candidate below the floor is excluded entirely, so the
// keyword leg cannot reintroduce an off-topic memory on a shared token. 0 (the
// default) disables it. The usable value is embedder-dependent; baked to 0 (off)
// by the server, overridden by the benchmark harness via this Option.
func WithRecallMinSemanticScore(minSemanticScore float64) Option {
	return func(s *Service) { s.recallMinSemanticScore = minSemanticScore }
}

// WithRecallSemanticReserve guarantees up to n of the recall slots for durable
// tiers (semantic/procedural). 0 (the default) disables it. Baked to 2 by the
// server; the benchmark harness overrides it via this Option.
func WithRecallSemanticReserve(n int) Option {
	return func(s *Service) { s.recallSemanticReserve = n }
}

// WithEpisodicMinChars drops episodic writes whose substantive content is below
// n characters. 0 (the default) disables it. See MEMINI_EPISODIC_MIN_CHARS.
func WithEpisodicMinChars(n int) Option {
	return func(s *Service) { s.episodicMinChars = n }
}

// WithDistillOnWrite distils each fresh episodic capture into durable facts at
// write time. No-op without a distiller, so the server always enables it and
// lets LLM presence decide whether it runs.
func WithDistillOnWrite(on bool) Option {
	return func(s *Service) { s.distillOnWrite = on }
}

// WithDistillDropNoFact, with WithDistillOnWrite, deletes an episodic capture
// when distillation extracts no durable fact. Off by default; not wired to a
// server flag (the server always keeps episodic captures).
func WithDistillDropNoFact(on bool) Option {
	return func(s *Service) { s.distillDropNoFact = on }
}

// WithExtractOnWrite runs each fresh episodic capture through the no-LLM
// heuristic extractor. Only fires when no distiller is configured, so the server
// always enables it and lets LLM absence decide whether it runs.
func WithExtractOnWrite(on bool) Option {
	return func(s *Service) { s.extractOnWrite = on }
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

// WriteDedupAction selects what write-time dedup does when a fresh write scores
// at or above the dedup threshold against its nearest same-tier memory.
type WriteDedupAction string

const (
	// WriteDedupOff disables write-time fuzzy dedup. The exact-restatement
	// fingerprint pass (WithFingerprintDedup) is independent and still runs.
	WriteDedupOff WriteDedupAction = "off"
	// WriteDedupHint stores the write and returns a MergeHint for the caller to
	// merge. Non-destructive; scoped to durable tiers (semantic/procedural).
	WriteDedupHint WriteDedupAction = "hint"
	// WriteDedupCoalesce reinforces the existing memory and drops the write
	// (headless corpus hygiene). Applies to all tiers.
	WriteDedupCoalesce WriteDedupAction = "coalesce"
	// WriteDedupSupersede stores the write and tombstones the old memory.
	WriteDedupSupersede WriteDedupAction = "supersede"
)

// WithWriteDedup configures write-time dedup: when a fresh write's nearest
// same-tier memory scores at or above score, the given action fires (see
// WriteDedupAction). A score of 0 or action "off" disables it. This replaces the
// former three-gate band system — there is no ordering to misconfigure.
func WithWriteDedup(score float64, action WriteDedupAction) Option {
	return func(s *Service) {
		s.writeDedupScore = score
		s.writeDedupAction = action
	}
}

// WithGlobalNamespace sets a namespace whose durable (semantic/procedural)
// memories are merged read-only into every other namespace's recall and
// briefing — a shared space for cross-project rules. Empty disables it.
func WithGlobalNamespace(ns string) Option {
	return func(s *Service) { s.globalNamespace = ns }
}

// MergeHint surfaces a near-duplicate the caller may want to merge into.
type MergeHint struct {
	// SimilarID is the id of the near-duplicate memory. Empty when unknown.
	SimilarID string
	// SimilarContent is a preview of the near-duplicate memory's content.
	SimilarContent string
	// Score is the fused similarity (0..1) between the new write and the
	// near-duplicate.
	Score float64
	// Tier is the tier of the near-duplicate.
	Tier memory.Tier
}

// WithFingerprintDedup toggles exact-restatement dedup: when on (the default), a
// fresh write whose normalized content exactly matches a live same-tier memory
// reinforces that memory instead of storing a duplicate, without embedding it.
// It is independent of WithWriteDedup (the fuzzy vector gate) and the LLM
// consolidation pipeline.
func WithFingerprintDedup(on bool) Option {
	return func(s *Service) { s.fingerprintDedup = on }
}

// WithReinforceSkipMarkers drops session-end / stop marker memories from recall
// reinforcement. The pre-tool-use hook searches once per edited file, so markers
// would otherwise inflate their access_count and TTL out of proportion. They
// stay searchable; only the reinforce write is skipped.
func WithReinforceSkipMarkers(on bool) Option {
	return func(s *Service) { s.reinforceSkipMarkers = on }
}

// WithSecretRedaction toggles server-side scrubbing of live credentials from a
// memory's Content/Summary/Metadata at ingestion (on by default). It bounds a
// database compromise to information disclosure — leaked memory holds no usable
// tokens, keys, or passwords. Disable only if redaction mangles legitimate
// content; storing raw secrets re-opens the lateral-movement risk.
func WithSecretRedaction(on bool) Option {
	return func(s *Service) { s.redactSecrets = on }
}

// WithCorruptionQuarantine toggles downranking of writes whose content looks
// like script-salad — garbled multilingual output from an upstream model or
// harness glitch (off by default). When on, a flagged write is still stored but
// has its importance zeroed and metadata.quarantined set, so it sinks in recall
// instead of surfacing verbatim. It is a heuristic and can misjudge rare
// legitimate mixed-script text, so it only downranks (never rejects); leave it
// off unless garbled digests are a problem for a deployment.
func WithCorruptionQuarantine(on bool) Option {
	return func(s *Service) { s.quarantineGarbled = on }
}

// New builds a Service from a store and embedder.
func New(st store.Store, e embed.Embedder, opts ...Option) *Service {
	s := &Service{
		store:                st,
		embedder:             e,
		consolidateMode:      ConsolidateAsync,
		consolidateMinScore:  0.6,
		promoteMinAccess:     3,
		rerankTimeout:        defaultRerankTimeout,
		scoreFusionAlpha:     search.DefaultFusionAlpha, // convex score fusion by default; negative selects RRF
		poolFactor:           recallPoolFactor,
		poolFloor:            recallPoolFloor,
		fingerprintDedup:     true,
		reinforceSkipMarkers: true,
		redactSecrets:        true,
		metrics:              nopMetrics{},
		now:                  func() time.Time { return time.Now().UTC() },
		newID:                func() string { return uuid.NewString() },
	}
	for _, o := range opts {
		o(s)
	}
	if s.metrics == nil {
		s.metrics = nopMetrics{}
	}
	// The async queue exists only when consolidation can actually run.
	if s.consolidator != nil && s.consolidateMode == ConsolidateAsync {
		s.consolidateQueue = make(chan consolidateJob, defaultConsolidateQueueCap)
	}
	// Bounds concurrent write-time enrichment (LLM distillation or the no-LLM
	// heuristic extraction; the two are mutually exclusive at runtime).
	if s.distillOnWrite || s.extractOnWrite {
		s.distillSem = make(chan struct{}, distillSemCap)
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
	// MergeHint (output-only) is set to a non-nil MergeHint when the write's
	// nearest same-tier candidate landed in the merge-hint band. The caller
	// passes the address of a local `*MergeHint`; after the call it holds the
	// hint (or remains nil). nil disables hint reporting.
	MergeHint *MergeHint
	// AutoSuperseded (output-only) is set to true when the write triggered a
	// background supersede. The caller passes the address of a local bool.
	// nil disables reporting.
	AutoSuperseded *bool
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

// sanitizeContent applies write-path content hygiene: it always strips
// unambiguous corruption (invalid UTF-8, control/non-character codepoints) from
// content/summary, and — when quarantine is enabled — downranks script-salad
// writes (Importance=0 + metadata.quarantined) rather than rejecting them. It
// returns an error when the content is empty after cleaning (pure binary
// garbage). Leaves valid text in any language untouched.
func (s *Service) sanitizeContent(ctx context.Context, in RememberInput, tier memory.Tier) (RememberInput, error) {
	origBytes := len(in.Content) + len(in.Summary)
	in.Content = sanitize.Clean(in.Content)
	in.Summary = sanitize.Clean(in.Summary)
	if removed := origBytes - (len(in.Content) + len(in.Summary)); removed > 0 {
		s.metrics.WriteSanitized("cleaned")
		// Debug, not Warn: this runs for every deployment, so keep it off the
		// default log. It surfaces how often corruption is actually arriving.
		slog.DebugContext(ctx, "remember: stripped corrupted bytes from content",
			"namespace", in.Namespace, "removed_bytes", removed)
	}
	if in.Content == "" {
		return in, invalidInputf("remember: content is empty after sanitization")
	}
	// Off by default: the heuristic can misjudge rare legitimate mixed-script
	// text, so it only downranks (never rejects) — the memory stays inspectable.
	if s.quarantineGarbled && sanitize.Garbled(in.Content) {
		in.Importance = 0
		if in.Metadata == nil {
			in.Metadata = map[string]any{}
		}
		in.Metadata["quarantined"] = true
		s.metrics.WriteSanitized("quarantined")
		// Audit trail: only fires when an operator opted in, so it can't add
		// noise to other deployments. Lets them catch false positives on
		// legitimate mixed-script content.
		slog.WarnContext(ctx, "remember: quarantined garbled content (downranked, not rejected)",
			"namespace", in.Namespace, "tier", string(tier))
	}
	return in, nil
}

// validateRememberInput checks the required fields and resolves the tier
// (defaulting to working). The returned tier is "" for the namespace/content
// errors and the offending tier for an invalid-tier error, matching the metric
// label the caller records.
func validateRememberInput(in RememberInput) (memory.Tier, error) {
	if in.Namespace == "" {
		return "", invalidInputf("remember: namespace is required")
	}
	if in.Content == "" {
		return "", invalidInputf("remember: content is required")
	}
	tier := in.Tier
	if tier == "" {
		tier = memory.TierWorking
	}
	if !tier.Valid() {
		return tier, invalidInputf("remember: invalid tier %q", tier)
	}
	return tier, nil
}

// Remember embeds and stores a memory, returning the persisted record.
func (s *Service) Remember(ctx context.Context, in RememberInput) (*memory.Memory, error) {
	start := time.Now()
	defer func() {
		s.metrics.OpDuration("remember", time.Since(start))
	}()
	tier, err := validateRememberInput(in)
	if err != nil {
		s.metrics.RememberResult("error", string(tier))
		return nil, err
	}

	// Scrub live credentials before anything persists them — content, the
	// embedding, and the dedup fingerprint are all computed on the redacted
	// text, so a leaked database yields no usable tokens/keys.
	in = s.scrubInput(in)

	// Content hygiene: strip unambiguous corruption (always) and downrank
	// script-salad (opt-in). Rejects a write that is empty after cleaning.
	in, err = s.sanitizeContent(ctx, in, tier)
	if err != nil {
		s.metrics.RememberResult("error", string(tier))
		return nil, err
	}

	// Episodic value gate: drop low-signal per-turn chatter ("keep going", "ok")
	// before it embeds and lands in episodic memory for 90 days. Only episodic is
	// gated, so durable writes and promotion are unaffected. A drop returns
	// (nil, nil) — accepted, not stored.
	if s.dropsLowSignalEpisodic(tier, in.Content) {
		s.metrics.RememberResult("dropped", string(tier))
		return nil, nil
	}

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
		Importance:     resolveImportance(in, existing, tier),
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

	// Write-time dedup (non-LLM corpus hygiene): run the split dedup check
	// when neither the consolidation pipeline nor an explicit ID is in play.
	var supersedeID string
	if in.ID == "" && !consolidate {
		handled, result, sid := s.runSplitDedup(ctx, m, in)
		if handled {
			return result, nil
		}
		supersedeID = sid
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

	// Auto-supersede: now that the replacement is durably stored, tombstone the
	// near-duplicate in the background. Deferred to here so a failed Upsert above
	// can never drop the old fact without a stored replacement. No-op when there
	// is nothing to supersede.
	s.autoSupersede(m.Namespace, supersedeID, m.ID, in.AutoSuperseded)

	// Async mode stores immediately and consolidates in the background.
	if consolidate && s.consolidateMode == ConsolidateAsync {
		s.enqueueConsolidate(m.Namespace, m.ID)
	}
	// Write-time distillation: distil a fresh episodic capture into durable facts
	// in the background, so durable knowledge is created at write rather than
	// waiting on the access-gated batch promoter. Opt-in, needs a distiller.
	switch {
	case s.shouldDistillOnWrite(tier, existing == nil):
		s.distillEpisodicAsync(ctx, m)
	case s.shouldExtractOnWrite(tier, existing == nil):
		s.extractEpisodicAsync(ctx, m)
	}
	s.metrics.RememberResult("ok", string(tier))
	return m, nil
}

// runSplitDedup runs the write-time dedup gate. Returns handled=true and the
// existing memory when action=coalesce drops the write into it. Returns
// handled=false when the write should proceed; supersedeID then names a
// near-duplicate the caller must tombstone *after* storing the new memory
// (action=supersede), and any merge-hint (action=hint) is stashed on
// in.MergeHint for the caller to surface.
func (s *Service) runSplitDedup(
	ctx context.Context, m *memory.Memory, in RememberInput,
) (handled bool, result *memory.Memory, supersedeID string) {
	if s.writeDedupScore <= 0 || s.writeDedupAction == WriteDedupOff || s.writeDedupAction == "" {
		return false, nil, ""
	}
	// The hint action is scoped to durable tiers (semantic/procedural): that's
	// where the threshold was calibrated and where the hint is actually consumed
	// (model-driven writes). Episodic/working writes — notably the per-turn
	// capture flood — skip the lookup entirely, never paying a vector search for
	// a hint nobody reads. Coalesce/supersede apply to all tiers.
	if s.writeDedupAction == WriteDedupHint && m.Tier.Term() != memory.LongTerm {
		return false, nil, ""
	}
	hit, hint, supersedeID := s.dedupCheck(ctx, m)
	if hit != nil {
		return true, hit, ""
	}
	if in.MergeHint != nil && hint != nil {
		*in.MergeHint = *hint
	}
	return false, nil, supersedeID
}

// dedupCheck looks up the nearest same-tier memory and, when it scores at or
// above writeDedupScore, applies writeDedupAction. It returns:
//   - hit: the existing memory the write was coalesced into (action "coalesce");
//     the caller stores nothing new.
//   - hint: a MergeHint (action "hint") — the caller proceeds with the write and
//     surfaces the hint so the LLM (or the human) can merge via memory_update.
//   - supersedeID: the id of the near-duplicate to tombstone (action
//     "supersede"). Deferred to the caller so it only runs once the replacement
//     is durably stored — a failed insert must never drop the old memory.
//
// At most one is non-zero; all empty when nothing scores above the threshold.
func (s *Service) dedupCheck(ctx context.Context, m *memory.Memory) (hit *memory.Memory, hint *MergeHint, supersedeID string) {
	cands, err := s.store.VectorSearch(ctx, m.Namespace, m.Embedding,
		store.Filter{Tiers: []memory.Tier{m.Tier}, Now: s.now()}, 1)
	if err != nil {
		slog.WarnContext(ctx, "remember: dedup search failed, storing without dedup",
			"namespace", m.Namespace, "err", err)
		return nil, nil, ""
	}
	if len(cands) == 0 || cands[0].Score < s.writeDedupScore {
		return nil, nil, ""
	}
	existing := cands[0].Memory

	switch s.writeDedupAction {
	case WriteDedupSupersede:
		return nil, nil, existing.ID
	case WriteDedupHint:
		preview := existing.Content
		if len(preview) > 200 {
			preview = preview[:200] + "…"
		}
		return nil, &MergeHint{
			SimilarID:      existing.ID,
			SimilarContent: preview,
			Score:          cands[0].Score,
			Tier:           existing.Tier,
		}, ""
	case WriteDedupCoalesce:
		s.reinforce(ctx, []store.Scored{{Memory: existing}})
		s.corroborate(ctx, existing)
		return existing, nil, ""
	}
	return nil, nil, ""
}

// autoSupersede tombstones oldID (replaced by newID) in the background, after
// the replacement has been durably stored; done (when non-nil) records that it
// fired. A no-op when oldID is empty. Fire-and-forget so the write path doesn't
// pay the latency; a sweep GCs the tombstoned row.
func (s *Service) autoSupersede(ns, oldID, newID string, done *bool) {
	if oldID == "" {
		return
	}
	if done != nil {
		*done = true
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.store.SetSuperseded(ctx, ns, oldID, newID); err != nil {
			slog.WarnContext(ctx, "remember: auto-supersede failed",
				"namespace", ns, "old", oldID, "err", err)
		}
	}()
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
	// MinScore, when > 0, overrides the server's default recallMinScore for
	// this call. Lets a caller request a stricter relevance floor per
	// integration (e.g. the pre-tool-use hook only injects highly-relevant
	// hits). 0 (the zero value) falls back to the server-wide gate. Only
	// meaningful with score fusion; RRF scores are not comparable to [0,1].
	MinScore float64
	// MinSemanticScore, when > 0, overrides the server's default
	// recallMinSemanticScore (the absolute vector-relevance gate) for this call.
	// 0 falls back to the server-wide gate.
	MinSemanticScore float64
	// SemanticReserve, when > 0, overrides the server's default
	// recallSemanticReserve (durable-tier slot reservation) for this call. 0
	// falls back to the server-wide value.
	SemanticReserve int
}

// Recall runs hybrid (vector + keyword) retrieval fused with RRF.
// addGlobalNamespace appends the configured global namespace to the recall
// fan-out (when set, distinct, and not already present), returning the new list,
// the global namespace's index (-1 when none was added), and the durable tier
// filter it should use — the global namespace contributes durable tiers only.
func (s *Service) addGlobalNamespace(namespaces []string, in RecallInput) ([]string, int, []memory.Tier) {
	if s.globalNamespace == "" || s.globalNamespace == in.Namespace || slices.Contains(namespaces, s.globalNamespace) {
		return namespaces, -1, nil
	}
	gt := durableTiers(in.Tiers)
	if len(gt) == 0 {
		return namespaces, -1, nil
	}
	return append(namespaces, s.globalNamespace), len(namespaces), gt
}

// durableTiers restricts a requested tier set to the durable tiers (semantic,
// procedural) that the global namespace may contribute. An empty request (all
// tiers) becomes durable-only; an episodic-only request yields none.
func durableTiers(requested []memory.Tier) []memory.Tier {
	if len(requested) == 0 {
		return []memory.Tier{memory.TierSemantic, memory.TierProcedural}
	}
	var out []memory.Tier
	for _, t := range requested {
		if t == memory.TierSemantic || t == memory.TierProcedural {
			out = append(out, t)
		}
	}
	return out
}

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
	// Merge the global namespace (durable tiers only) into the fan-out so shared
	// cross-project rules surface in every recall. It gets its own tier filter;
	// every other namespace uses the requested one.
	namespaces, globalIdx, globalTiers := s.addGlobalNamespace(namespaces, in)
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
		f := filter
		if i == globalIdx {
			f.Tiers = globalTiers // global namespace contributes durable tiers only
		}
		if vec != nil {
			g2.Go(func() error {
				v, err := s.store.VectorSearch(g2ctx, ns, vec, f, poolK)
				if err != nil {
					return fmt.Errorf("recall: vector search: %w", err)
				}
				vres[i] = v
				return nil
			})
		}
		g2.Go(func() error {
			kw, err := s.store.KeywordSearch(g2ctx, ns, in.Query, f, poolK)
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

	// Absolute semantic-relevance gate: drop candidates whose raw vector score is
	// below the floor before fusing, so the keyword leg cannot reintroduce an
	// off-topic memory on a shared token.
	gateSemantic(vres, kres, s.resolveSemanticFloor(in))

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
	// A per-call MinScore (set by the integration) overrides the server-wide
	// default, so a hook can request a stricter floor without changing the
	// global gate.
	if s.scoreFusionAlpha >= 0 {
		floor := s.recallMinScore
		if in.MinScore > 0 {
			floor = in.MinScore
		}
		if floor > 0 {
			filtered := make([]store.Scored, 0, len(fused))
			for _, r := range fused {
				if r.Score >= floor {
					filtered = append(filtered, r)
				}
			}
			fused = filtered
		}
	}
	var ranked []store.Scored
	if s.temporalBoost > 0 && s.temporalAnchor != nil {
		ranked = search.RerankTemporal(fused, in.Query, s.now(),
			search.DefaultRerankWeights, s.temporalAnchor, s.temporalBoost)
	} else {
		ranked = search.Rerank(fused, s.now())
	}
	// Reserve slots for durable tiers so episodic chatter can't crowd out
	// consolidated facts/rules; the pool is already relevance-filtered upstream.
	ranked = reserveDurableTiers(ranked, k, s.resolveSemanticReserve(in))
	results := s.finalizeRecall(ctx, in.Query, ranked, k)
	s.reinforceResults(ctx, results)
	s.metrics.RecallResult("ok", tf, hitsBucket(len(results)))
	return results, nil
}

// resolveSemanticFloor returns the per-call MinSemanticScore override when set,
// else the server-wide recallMinSemanticScore.
func (s *Service) resolveSemanticFloor(in RecallInput) float64 {
	if in.MinSemanticScore > 0 {
		return in.MinSemanticScore
	}
	return s.recallMinSemanticScore
}

// resolveSemanticReserve returns the per-call SemanticReserve override when set,
// else the server-wide recallSemanticReserve.
func (s *Service) resolveSemanticReserve(in RecallInput) int {
	if in.SemanticReserve > 0 {
		return in.SemanticReserve
	}
	return s.recallSemanticReserve
}

// shouldDistillOnWrite reports whether a fresh episodic capture should be
// distilled into durable facts at write time. isCreate must be false for an
// update (an existing row re-written by ID, e.g. a re-fired session digest), so
// a capture is distilled once on creation and never again when it's overwritten.
func (s *Service) shouldDistillOnWrite(tier memory.Tier, isCreate bool) bool {
	return s.distillOnWrite && s.distiller != nil && tier == memory.TierEpisodic && isCreate
}

// shouldExtractOnWrite mirrors shouldDistillOnWrite for the heuristic path: it
// requires no distiller, so distill-on-write supersedes it when an LLM is set.
func (s *Service) shouldExtractOnWrite(tier memory.Tier, isCreate bool) bool {
	return s.extractOnWrite && s.distiller == nil && tier == memory.TierEpisodic && isCreate
}

// distillEpisodicAsync distils a freshly-written episodic into durable facts in
// the background, detached from the request so the capture isn't blocked on the
// LLM. It reuses the promote path (stamp → distill → write deduped facts) for
// one memory, bounded by distillSem so a write burst can't fan out unbounded
// LLM calls. Best-effort: a failure is logged, the episodic stays.
func (s *Service) distillEpisodicAsync(ctx context.Context, m *memory.Memory) {
	bg := context.WithoutCancel(ctx)
	s.bg.Go(func() {
		s.distillSem <- struct{}{}
		defer func() { <-s.distillSem }()
		dctx, cancel := context.WithTimeout(bg, distillOnWriteTimeout)
		defer cancel()
		n, err := s.promote(dctx, m.Namespace, []*memory.Memory{m}, s.now())
		if err != nil {
			slog.WarnContext(dctx, "distill-on-write", "namespace", m.Namespace, "id", m.ID, "err", err)
			return
		}
		// Drop-when-no-fact: the LLM found nothing durable in this turn, so delete
		// the kept episodic rather than let low-value chatter the heuristic missed
		// sit for 90 days.
		if n == 0 && s.distillDropNoFact {
			if err := s.store.Delete(dctx, m.Namespace, m.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				slog.WarnContext(dctx, "distill-on-write: drop no-fact episodic", "namespace", m.Namespace, "id", m.ID, "err", err)
				return
			}
			s.metrics.RememberResult("dropped", string(memory.TierEpisodic))
		}
	})
}

// extractEpisodicAsync stores the heuristic extractor's typed facts from a
// freshly-written episodic. The marker scan runs inline; only the embed+store of
// each fact is detached. The raw episodic is kept; a per-fact failure is logged
// without blocking the rest.
func (s *Service) extractEpisodicAsync(ctx context.Context, m *memory.Memory) {
	results := extract.Typed(m.Content)
	if len(results) == 0 {
		return
	}
	bg := context.WithoutCancel(ctx)
	s.bg.Go(func() {
		s.distillSem <- struct{}{}
		defer func() { <-s.distillSem }()
		dctx, cancel := context.WithTimeout(bg, distillOnWriteTimeout)
		defer cancel()
		for _, r := range results {
			in := RememberInput{
				Namespace: m.Namespace,
				Content:   r.Content,
				Tier:      r.Kind.Tier(),
				Tags:      []string{string(r.Kind)},
				Metadata:  map[string]any{"memory_type": string(r.Kind)},
			}
			if _, err := s.Remember(dctx, in); err != nil {
				slog.WarnContext(dctx, "extract-on-write: store fact",
					"namespace", m.Namespace, "id", m.ID, "kind", string(r.Kind), "err", err)
			}
		}
	})
}

// reserveDurableTiers recomposes a relevance-ordered pool so up to `reserve` of
// the top `limit` slots hold durable tiers (semantic/procedural): durables just
// outside the window are promoted in, evicting the lowest-relevance episodic
// entries, until the reserve is met. Relevance order is preserved. reserve <= 0
// or a pool no deeper than `limit` returns the input unchanged.
func reserveDurableTiers(ranked []store.Scored, limit, reserve int) []store.Scored {
	if reserve <= 0 || limit <= 0 || len(ranked) <= limit {
		return ranked
	}
	if reserve > limit {
		reserve = limit
	}
	durable := func(t memory.Tier) bool { return t == memory.TierSemantic || t == memory.TierProcedural }

	selected := make(map[int]struct{}, limit)
	durableCount := 0
	for i := range limit {
		selected[i] = struct{}{}
		if durable(ranked[i].Memory.Tier) {
			durableCount++
		}
	}
	if durableCount >= reserve {
		return ranked
	}

	for i := limit; i < len(ranked) && durableCount < reserve; i++ {
		if !durable(ranked[i].Memory.Tier) {
			continue
		}
		evict := -1
		for j := limit - 1; j >= 0; j-- {
			if _, ok := selected[j]; ok && !durable(ranked[j].Memory.Tier) {
				evict = j
				break
			}
		}
		if evict < 0 {
			break // window is all durable; nothing to give up
		}
		delete(selected, evict)
		selected[i] = struct{}{}
		durableCount++
	}

	out := make([]store.Scored, 0, len(ranked))
	for i := range ranked {
		if _, ok := selected[i]; ok {
			out = append(out, ranked[i])
		}
	}
	for i := range ranked {
		if _, ok := selected[i]; !ok {
			out = append(out, ranked[i])
		}
	}
	return out
}

// gateSemantic drops vector candidates scoring below floor and restricts the
// keyword leg to the survivors, so a sub-threshold candidate cannot re-enter via
// a keyword match. floor <= 0 is a no-op. vres and kres are per-namespace,
// index-aligned, and filtered in place.
func gateSemantic(vres, kres [][]store.Scored, floor float64) {
	if floor <= 0 {
		return
	}
	for i := range vres {
		passed := make(map[string]struct{}, len(vres[i]))
		kept := vres[i][:0]
		for _, r := range vres[i] {
			if r.Score >= floor {
				kept = append(kept, r)
				passed[r.Memory.ID] = struct{}{}
			}
		}
		vres[i] = kept
		keptKW := kres[i][:0]
		for _, r := range kres[i] {
			if _, ok := passed[r.Memory.ID]; ok {
				keptKW = append(keptKW, r)
			}
		}
		kres[i] = keptKW
	}
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
	// Filter here, not in reinforce(): the write-dedup callers (dedupExisting,
	// fingerprintHit) invoke reinforce() directly with a memory they want
	// corroborated. Markers stay in recall output; only their reinforce is skipped.
	if s.reinforceSkipMarkers {
		kept := results[:0:0]
		for _, r := range results {
			if !isSessionMarker(r.Memory) {
				kept = append(kept, r)
			}
		}
		results = kept
	}
	if len(results) == 0 {
		return
	}
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

// isSessionMarker reports whether m is a session-end / stop hook digest.
func isSessionMarker(m *memory.Memory) bool {
	return strings.HasPrefix(m.ID, sessionEndIDPrefix) ||
		strings.HasPrefix(m.ID, stopIDPrefix)
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

// resolveImportance decides a write's importance: an explicit value wins, a
// quarantined write keeps its zeroed importance, an update preserves the
// existing value, and otherwise a fresh write is seeded by tier so memories
// aren't all stored at 0 (a dead ranking signal).
func resolveImportance(in RememberInput, existing *memory.Memory, tier memory.Tier) float64 {
	switch {
	case in.Importance != 0:
		return in.Importance
	case in.Metadata["quarantined"] == true:
		return 0
	case existing != nil:
		return existing.Importance
	default:
		return seedImportance(tier)
	}
}

// seedImportance is the tier-based importance floor for a fresh write that
// carried none: durable curated tiers outrank episodic turns, which outrank
// ephemeral working notes.
func seedImportance(tier memory.Tier) float64 {
	switch tier {
	case memory.TierSemantic, memory.TierProcedural:
		return 0.6
	case memory.TierEpisodic:
		return 0.3
	default: // working
		return 0.1
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

// History returns the full supersession lineage of a memory: the memory itself,
// every memory it superseded (walking PredecessorIDs backwards) and every one
// that superseded it (following SupersededBy forwards), including tombstoned
// rows, ordered oldest-first by CreatedAt. Returns ErrNotFound when id is
// absent. Walks breadth-first so a merge (several memories superseded by one)
// is followed in every direction without revisiting a node.
func (s *Service) History(ctx context.Context, namespace, id string) ([]*memory.Memory, error) {
	root, err := s.store.Get(ctx, namespace, id)
	if err != nil {
		return nil, err
	}
	seen := map[string]*memory.Memory{root.ID: root}
	queue := []*memory.Memory{root}
	visit := func(mid string) error {
		if mid == "" {
			return nil
		}
		if _, ok := seen[mid]; ok {
			return nil
		}
		m, err := s.store.Get(ctx, namespace, mid)
		if errors.Is(err, store.ErrNotFound) {
			return nil // a dangling link is not fatal to the rest of the chain
		}
		if err != nil {
			return err
		}
		seen[m.ID] = m
		queue = append(queue, m)
		return nil
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.SupersededBy != nil {
			if err := visit(*cur.SupersededBy); err != nil {
				return nil, err
			}
		}
		preds, err := s.store.PredecessorIDs(ctx, namespace, cur.ID)
		if err != nil {
			return nil, err
		}
		for _, pid := range preds {
			if err := visit(pid); err != nil {
				return nil, err
			}
		}
	}
	out := make([]*memory.Memory, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
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

// Supersede tombstones (namespace, id), recording that it was replaced by
// supersededBy. The row is hidden from default recall but kept for the
// audit/time-travel chain; the sweeper hard-deletes it after TombstoneTTL.
// NotFound surfaces to the caller so a missing target is not silently
// swallowed. Idempotent: re-superseding overwrites superseded_by.
func (s *Service) Supersede(ctx context.Context, namespace, id, supersededBy string) error {
	if strings.TrimSpace(id) == "" {
		return invalidInputf("supersede: id is required")
	}
	if strings.TrimSpace(supersededBy) == "" {
		return invalidInputf("supersede: supersededBy is required")
	}
	err := s.store.SetSuperseded(ctx, namespace, id, supersededBy)
	switch {
	case err == nil:
		s.metrics.SupersedeResult("ok")
	case errors.Is(err, store.ErrNotFound):
		s.metrics.SupersedeResult("not_found")
	default:
		s.metrics.SupersedeResult("error")
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
