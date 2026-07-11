package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/eleboucher/memini/internal/contradict"
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

	// readSetMaxEntries caps a read-set after subtree/pattern expansion (raw
	// per-call namespace lists are capped separately, at readSetMaxExplicit,
	// before expansion). A read-set over the cap is clamped, keeping the
	// primary namespace first.
	readSetMaxEntries = 64

	// readSetMaxExplicit caps the raw per-call namespaces list before
	// subtree/pattern expansion; see resolveExplicitReadSet.
	readSetMaxExplicit = 16
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
	// RememberDegraded records one write that stored without a vector (embedding
	// omitted, keyword-searchable only, marked pending_embed) because the content
	// embed failed or timed out. reason is "embed_timeout" or "embed_error".
	RememberDegraded(reason string)
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
	// CorroborateResult records one corroboration-routing attempt on a fresh
	// short-term write: "corroborated" (durable fact reinforced), "cooldown"
	// (match found but inside the per-fact window), "miss" (no durable
	// neighbour at or above the threshold), or "error".
	CorroborateResult(result string)
	// ContradictResult records one contradiction-routing attempt on a fresh
	// durable write: "contradicted" (stale fact invalidated), "no_signal" (a
	// near neighbour, but the detector saw no value/polarity change), "cooldown"
	// (match inside the per-fact window), "miss" (no durable neighbour at or
	// above the threshold, or an untracked-confidence row), or "error".
	ContradictResult(result string)
	// TierClassified records an omitted-tier write the marker classifier
	// routed to a durable tier; tier is "semantic" or "procedural".
	TierClassified(tier string)
	// EmbedBackfillPending reports the number of memories still marked
	// pending_embed after one backfill tick (0 once the queue is drained).
	EmbedBackfillPending(n int)
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
func (nopMetrics) RememberDegraded(string)             {}
func (nopMetrics) WriteSanitized(string)               {}
func (nopMetrics) ReinforceResult(string)              {}
func (nopMetrics) DedupTombstoned(int)                 {}
func (nopMetrics) CorroborateResult(string)            {}
func (nopMetrics) ContradictResult(string)             {}
func (nopMetrics) TierClassified(string)               {}
func (nopMetrics) EmbedBackfillPending(int)            {}

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
	// recallRewriteTimeout bounds the LLM query-expansion call on query_rewrite
	// recalls; past it, expansion yields just the original query and recall
	// falls through to normal single-query recall. 0 keeps the rewrite call
	// unbounded (rides along the LLM client's own HTTP timeout).
	recallRewriteTimeout time.Duration
	// writeEmbedTimeout bounds the content embed on the remember path; past it, or
	// on any embed error, the write degrades to a vectorless (keyword-searchable
	// only) row marked pending_embed instead of failing. 0 keeps the content embed
	// unbounded and an embed error fatal (fail-fast, the pre-fallback default).
	writeEmbedTimeout time.Duration
	// recallMinScore is an absolute relevance floor on the fused score; candidates
	// below it are dropped before composite re-ranking. 0 disables filtering.
	recallMinScore float64
	// recallMinSemanticScore is an absolute floor on the raw vector score
	// (1/(1+L2)): a candidate below it is excluded before fusion and the keyword
	// leg cannot reintroduce it, so a query with nothing semantically relevant
	// recalls empty. 0 disables it. The usable value is embedder-specific (see
	// docs/recall-relevance-gate-2026-06-20.md).
	recallMinSemanticScore float64
	// recallSemanticReserve reserves up to N recall slots for durable tiers
	// (semantic/procedural) that are relevance-competitive with what they
	// displace, the rest by relevance. 0 disables it.
	recallSemanticReserve int
	// reservePromoteRatio is the relevance bar for a reserved slot: a durable
	// is promoted only when its composite score is at least this fraction of
	// the entry it evicts. Defaults to defaultReservePromoteRatio.
	reservePromoteRatio float64
	// reserveTopAnchor is the absolute leg of the reserve gate: a durable is
	// promoted only when its composite score is also at least this fraction of
	// the window's top hit. Defaults to defaultReserveTopAnchor; 0 disables it.
	reserveTopAnchor float64
	// reserveGatePercentile (> 0) switches the reserve's relevance gate to the
	// adaptive form: a durable is promoted only when it reaches this percentile
	// of the window's own scores. 0 keeps the fixed-ratio gate. Tuning/bench
	// knob; see bench/reserve_sweep_test.go.
	reserveGatePercentile float64
	// episodicMinChars drops an episodic write whose substantive content (role
	// scaffolding stripped) is below this many characters — the "keep going" /
	// "ok" chatter that otherwise dominates episodic memory. 0 disables it.
	episodicMinChars int

	// distillOnWrite distils each fresh episodic capture into durable facts at
	// write time, bounded by distillSem. Needs a distiller.
	distillOnWrite bool
	// distillBatch, when non-nil, batches distill-on-write per (namespace,
	// session_id) instead of one LLM call per capture (WithDistillBatch).
	distillBatch *distillBatcher
	distillSem   chan struct{}
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
	// turnEchoWindow is the server-wide default temporal exclusion window for
	// freshly-captured episodic turns. It fires by default on every recall; a
	// caller opts out via IncludeFreshTurns. Zero disables it server-wide.
	turnEchoWindow time.Duration
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
	// corroborateMinScore (> 0) routes short-term writes that restate a durable
	// fact into confidence growth on that fact. 0 disables.
	corroborateMinScore float64
	// contradictMinScore (> 0) routes fresh durable writes that contradict an
	// existing durable fact into invalidating that fact (valid_to stamp +
	// confidence shrink), when the lexical detector confirms a value/polarity
	// change. 0 disables. See WithContradictionDownrank.
	contradictMinScore float64
	// writeDedupAction is what happens at/above writeDedupScore: hint, coalesce,
	// supersede, or off. See WriteDedupAction.
	writeDedupAction WriteDedupAction
	// splitDedupLLMMerge (opt-in, default off) routes ambiguous split-dedup
	// candidates (≥2 close neighbours) through the LLM consolidator for a
	// merge/supersede verdict before the deterministic action fires.
	splitDedupLLMMerge bool
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

// Store returns the underlying store so the REST layer can call
// maintenance-level operations (reassign, split, move) without a separate
// service facade. Exported sparingly; callers should not mutate internal state.
func (s *Service) Store() store.Store { return s.store }

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

// HasAnswerer reports whether an LLM completer is configured for answering.
// Callers (e.g. the MCP server) use this to decide whether to expose
// answer-dependent surfaces at all, rather than exposing them and erroring on
// every call in a headless deployment.
func (s *Service) HasAnswerer() bool { return s.answerer != nil }

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

// WithRecallRewriteTimeout bounds the LLM query-expansion call on
// query_rewrite recalls. Past the deadline, expansion yields just the
// original query and recall falls through to normal single-query recall,
// rather than blocking on the LLM client's much longer HTTP timeout. d <= 0
// keeps the rewrite call unbounded (the default).
func WithRecallRewriteTimeout(d time.Duration) Option {
	return func(s *Service) {
		if d > 0 {
			s.recallRewriteTimeout = d
		}
	}
}

// WithWriteEmbedTimeout bounds the content embed on the remember path. Past the
// deadline, or on any embed error, the write degrades to a vectorless
// (keyword-searchable only) row marked pending_embed rather than failing or
// stalling on a slow embeddings backend. d <= 0 keeps the content embed
// unbounded and an embed error fatal (the default).
func WithWriteEmbedTimeout(d time.Duration) Option {
	return func(s *Service) {
		if d > 0 {
			s.writeEmbedTimeout = d
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

// WithTurnEchoWindow sets the server-wide default temporal exclusion window
// for freshly-captured episodic turns. It fires by default on every recall; a
// caller opts out via IncludeFreshTurns. Zero disables it server-wide.
func WithTurnEchoWindow(d time.Duration) Option { return func(s *Service) { s.turnEchoWindow = d } }

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

// WithRecallSemanticReserve reserves up to n of the recall slots for durable
// tiers (semantic/procedural); a durable takes a slot only when it is
// relevance-competitive with the entry it displaces (reservePromoteRatio).
// 0 (the default) disables it. Baked to 2 by the server; the benchmark
// harness overrides it via this Option.
func WithRecallSemanticReserve(n int) Option {
	return func(s *Service) { s.recallSemanticReserve = n }
}

// WithReservePromoteRatio overrides the evictee-relative leg of the reserve's
// relevance gate: a durable takes a reserved slot only when its composite
// score is at least ratio× the entry it evicts. Tuning/bench knob
// (bench/reserve_sweep_test.go); the production default is
// defaultReservePromoteRatio.
func WithReservePromoteRatio(ratio float64) Option {
	return func(s *Service) { s.reservePromoteRatio = ratio }
}

// WithReserveTopAnchor overrides the absolute leg of the reserve's relevance
// gate: a durable takes a reserved slot only when its composite score is at
// least anchor× the window's top hit. Tuning/bench knob
// (bench/reserve_sweep_test.go); the production default is
// defaultReserveTopAnchor, and 0 disables the leg.
func WithReserveTopAnchor(anchor float64) Option {
	return func(s *Service) { s.reserveTopAnchor = anchor }
}

// WithReserveGatePercentile (pct > 0) switches the reserve's relevance gate to
// the adaptive form: a durable takes a reserved slot only when its composite
// score reaches the pct-th percentile of the window's own scores, so the bar
// derives from the pool's score distribution instead of a fixed ratio.
// Tuning/bench knob (bench/reserve_sweep_test.go); 0 keeps the ratio gate.
func WithReserveGatePercentile(pct float64) Option {
	return func(s *Service) { s.reserveGatePercentile = pct }
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

// WithSplitDedupLLMMerge enables the opt-in LLM merge path in the split-dedup
// pipeline: when ≥2 candidates score above writeDedupScore and are within 0.05
// of each other, the LLM consolidator is consulted for a merge/supersede
// verdict before the deterministic action fires. Default off — requires a
// consolidator (WithConsolidator) to have any effect.
func WithSplitDedupLLMMerge(b bool) Option {
	return func(s *Service) { s.splitDedupLLMMerge = b }
}

// WithCorroboration enables corroboration routing: a fresh short-term write
// whose nearest durable neighbour scores at or above minScore reinforces that
// fact and grows its confidence (rate-limited by corroborateCooldown) instead
// of only piling up as chatter. The write is still stored. minScore <= 0
// disables.
func WithCorroboration(minScore float64) Option {
	return func(s *Service) { s.corroborateMinScore = minScore }
}

// WithContradictionDownrank enables contradiction routing: a fresh durable
// write whose nearest durable neighbour scores at or above minScore, and which
// the lexical detector (internal/contradict) confirms is a value/polarity
// change rather than a restatement, invalidates that stale neighbour —
// stamping its valid_to (so it leaves live recall but stays reachable via AsOf)
// and shrinking its confidence, off the request path and rate-limited by
// contradictCooldown. The new write is stored unchanged. minScore <= 0 disables.
func WithContradictionDownrank(minScore float64) Option {
	return func(s *Service) { s.contradictMinScore = minScore }
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
		consolidateMinScore:  0.3,
		promoteMinAccess:     3,
		rerankTimeout:        defaultRerankTimeout,
		scoreFusionAlpha:     search.DefaultFusionAlpha, // convex score fusion by default; negative selects RRF
		reservePromoteRatio:  defaultReservePromoteRatio,
		reserveTopAnchor:     defaultReserveTopAnchor,
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
// required; an omitted Tier is classified from the content (episodic when
// unclear) and TTL follows the tier default.
type RememberInput struct {
	Namespace string
	// Home is the caller's personal namespace (X-Memini-Home / MEMINI_HOME).
	// Consumed by resolveVisibility when Visibility is "personal".
	Home string
	// Visibility steers which namespace the write actually lands in: ""/
	// "project" (default) is Namespace itself; "personal" is Home; anything
	// else must name an ancestor of Namespace (by exact path or unambiguous
	// last segment). See resolveVisibility for the full resolution and the
	// tier clamp (episodic/working writes always stay in Namespace).
	Visibility string
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
	// Level labels the derivation provenance at write time: explicit (user-stated
	// or heuristic) vs deduced (LLM-distilled). Empty string means legacy/unknown
	// and falls through to "no constraint" in filter operations.
	Level memory.Level
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

// validateRememberInput checks the required fields, resolves the tier (an
// omitted tier defaults to working — the intake tier — but the marker
// heuristic can classify a terse, unhedged decision/preference/problem
// statement directly into a durable tier; classification only ever raises
// the tier from the working default, never lowers it, so a miss costs
// nothing), and resolves in.Namespace against Visibility/Home via
// resolveVisibility.
//
// Visibility resolution is folded in here — rather than left as a separate
// step in Remember — for two reasons: resolveVisibility's tier clamp must
// see the FINAL, post-classification tier (see its doc comment), which this
// function already computes; and doing so keeps Remember's own error
// handling to one `if err != nil` for the whole validate+resolve phase
// instead of two, which is also what keeps this the single place upstream
// of the rest of the write pipeline (dedup gate, fingerprint check,
// distillation, extraction) that touches in.Namespace, so everything below
// it sees the resolved target rather than the request primary (gap G4).
//
// The returned tier is "" for the namespace/content errors and the
// offending tier for an invalid-tier/visibility error, matching the metric
// label the caller records. The returned RememberInput carries the resolved
// Namespace only on success; an error return leaves it as given, which the
// caller never uses since it discards in on that path.
func validateRememberInput(in RememberInput) (RememberInput, memory.Tier, error) {
	if in.Namespace == "" {
		return in, "", invalidInputf("remember: namespace is required")
	}
	if in.Content == "" {
		return in, "", invalidInputf("remember: content is required")
	}
	tier := in.Tier
	if tier == "" {
		tier = memory.TierWorking
		if kind, ok := extract.Classify(in.Content); ok {
			tier = kind.Tier()
		}
	}
	if !tier.Valid() {
		return in, tier, invalidInputf("remember: invalid tier %q", tier)
	}
	if in.Level != "" && !in.Level.Valid() {
		return in, tier, invalidInputf("remember: invalid level %q", in.Level)
	}
	ns, err := resolveVisibility(in, tier)
	if err != nil {
		return in, tier, err
	}
	in.Namespace = ns
	return in, tier, nil
}

// embedForRemember embeds fresh write content. When writeEmbedTimeout is set
// (> 0), the embed is bounded and a timeout or error degrades the write: it
// returns a nil vector (the caller stores the memory keyword-searchable only)
// and stamps in.Metadata["pending_embed"] = "true" so a background backfill
// can pick it up later, rather than failing the write. writeEmbedTimeout <= 0
// keeps the embed unbounded and any error fatal — the pre-fallback,
// fail-fast default. in.Metadata is mutated in place (allocated if nil),
// mirroring stampClassifiedTier/scrubInput: callers that share a Metadata map
// across writes will see the flag too.
func (s *Service) embedForRemember(ctx context.Context, in *RememberInput) ([]float32, error) {
	if s.writeEmbedTimeout <= 0 {
		vec, err := embed.EmbedOne(ctx, s.embedder, in.Content)
		if err != nil {
			return nil, fmt.Errorf("remember: embed: %w", err)
		}
		delete(in.Metadata, "pending_embed")
		return vec, nil
	}
	ectx, cancel := context.WithTimeout(ctx, s.writeEmbedTimeout)
	defer cancel()
	vec, err := embed.EmbedOne(ectx, s.embedder, in.Content)
	if err == nil {
		// Clear a pre-existing pending_embed flag: this row is being
		// re-embedded (e.g. memory_update after the embedder recovered) and
		// now carries a fresh vector, so it must not still read as degraded —
		// a stale flag would falsely report degraded:"pending_embed" to the
		// caller, inflate the backfill gauge, and cause a redundant
		// re-embed next tick. delete on a nil map is a no-op, so this is
		// safe even when in.Metadata was never set.
		delete(in.Metadata, "pending_embed")
		return vec, nil
	}
	reason := "embed_error"
	if errors.Is(err, context.DeadlineExceeded) {
		reason = "embed_timeout"
	}
	slog.WarnContext(ctx, "remember: content embed failed, storing without vector",
		"namespace", in.Namespace, "reason", reason, "err", err)
	s.metrics.RememberDegraded(reason)
	if in.Metadata == nil {
		in.Metadata = map[string]any{}
	}
	in.Metadata["pending_embed"] = "true"
	return nil, nil
}

// Remember embeds and stores a memory, returning the persisted record.
func (s *Service) Remember(ctx context.Context, in RememberInput) (*memory.Memory, error) {
	start := time.Now()
	defer func() {
		s.metrics.OpDuration("remember", time.Since(start))
	}()
	in, tier, err := validateRememberInput(in)
	if err != nil {
		s.metrics.RememberResult("error", string(tier))
		return nil, err
	}
	in = s.stampClassifiedTier(in, tier)

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

	vec, err := s.embedForRemember(ctx, &in)
	if err != nil {
		s.metrics.RememberResult("error", string(tier))
		return nil, err
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
		Level:          in.Level,
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
	// An update by ID preserves the original creation time, so a tag- or
	// metadata-only edit doesn't make an old memory appear freshly created and
	// win every "prefer the most recent" recency conflict in recall/answer.
	if existing != nil {
		m.CreatedAt = existing.CreatedAt
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
			c := clampConfidence(*in.Confidence)
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
	// the caller sees the consolidated result. Skipped for a vectorless write
	// (embed degraded, see embedForRemember): the search it needs has no query
	// vector to run with, so the write falls through to a normal insert instead.
	if consolidate && s.consolidateMode == ConsolidateSync && len(m.Embedding) > 0 {
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
	// Write-time fact building: distil (LLM) or extract (heuristic) durable
	// facts from the fresh capture in the background, so durable knowledge is
	// created at write rather than waiting on the access-gated batch promoter.
	s.buildFactsOnWrite(ctx, m, tier, existing == nil)
	// Corroboration routing: a fresh short-term write that restates an existing
	// durable fact is a re-observation of that fact — grow its confidence and
	// reinforce it in the background. The write itself is stored unchanged.
	s.corroborateNearestAsync(ctx, m, in.ID == "")
	// Contradiction routing: the mirror of corroboration. A fresh durable write
	// that contradicts an existing durable fact (changed value or flipped
	// polarity) invalidates the stale fact in the background — it is not merely
	// a re-observation, it supersedes.
	s.contradictNearestAsync(ctx, m, in.ID == "")
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
	// A vectorless write (embed degraded, see embedForRemember) has nothing for
	// dedupCheck's vector search to run against; skip the gate and let the write
	// proceed as a normal insert rather than searching on an empty vector.
	if len(m.Embedding) == 0 {
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

// dedupCandidates is the number of nearest neighbours checked for write-time
// dedup. k>1 lets the action pick the best-fit from a small pool rather than
// relying on a single nearest neighbour.
const dedupCandidates = 5

// dedupLLMCloseness is the max score gap between the top two candidates that
// triggers the opt-in LLM merge path: when ≥2 candidates are above the dedup
// threshold AND within this gap, the LLM consolidator is consulted.
const dedupLLMCloseness = 0.05

// dedupCheck looks up the nearest same-tier memories and, when one scores at or
// above writeDedupScore, applies writeDedupAction. It returns:
//   - hit: the existing memory the write was coalesced into (action "coalesce");
//     the caller stores nothing new.
//   - hint: a MergeHint (action "hint") — the caller proceeds with the write and
//     surfaces the hint so the LLM (or the human) can merge via memory_update.
//   - supersedeID: the id of the near-duplicate to tombstone (action
//     "supersede", or a coalesce the incoming phrasing wins on informativeness).
//     Deferred to the caller so it only runs once the replacement is durably
//     stored — a failed insert must never drop the old memory.
//
// At most one is non-zero; all empty when nothing scores above the threshold.
func (s *Service) dedupCheck(ctx context.Context, m *memory.Memory) (hit *memory.Memory, hint *MergeHint, supersedeID string) {
	cands, err := s.store.VectorSearch(ctx, m.Namespace, m.Embedding,
		store.Filter{Tiers: []memory.Tier{m.Tier}, Now: s.now()}, dedupCandidates)
	if err != nil {
		slog.WarnContext(ctx, "remember: dedup search failed, storing without dedup",
			"namespace", m.Namespace, "err", err)
		return nil, nil, ""
	}
	// Filter to candidates at or above the dedup threshold (sorted by score
	// descending from the store).
	var above []store.Scored
	for _, c := range cands {
		if c.Score >= s.writeDedupScore {
			above = append(above, c)
		}
	}
	if len(above) == 0 {
		return nil, nil, ""
	}
	existing := above[0].Memory

	// C2: opt-in LLM merge when ≥2 close candidates indicate genuine ambiguity.
	if s.splitDedupLLMMerge && s.consolidator != nil && len(above) >= 2 &&
		above[0].Score-above[1].Score < dedupLLMCloseness {
		if llmHit, llmSid := s.dedupLLMMerge(ctx, m, above); llmHit != nil || llmSid != "" {
			return llmHit, nil, llmSid
		}
		// LLM error or no decision: fall through to deterministic dedup.
	}

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
			Score:          above[0].Score,
			Tier:           existing.Tier,
		}, ""
	case WriteDedupCoalesce:
		// Informativeness tiebreak (adapted from honcho's token-set
		// duplicate-superiority heuristic): when the incoming phrasing is
		// strictly richer than the stored one, replace instead of dropping —
		// store the new copy and tombstone the old (reversible via supersede),
		// carrying earned confidence forward so the swap doesn't reset trust.
		if wordSetScore(m.Content) > wordSetScore(existing.Content) {
			if existing.Confidence != nil && (m.Confidence == nil || *existing.Confidence > *m.Confidence) {
				c := *existing.Confidence
				m.Confidence = &c
			}
			return nil, nil, existing.ID
		}
		s.reinforce(ctx, []store.Scored{{Memory: existing}})
		s.corroborate(ctx, existing)
		return existing, nil, ""
	}
	return nil, nil, ""
}

// dedupLLMMerge consults the LLM consolidator with the top dedup candidates to
// resolve genuine ambiguity (≥2 close neighbours). On LLM error or no decision,
// returns all-zero so the caller falls through to deterministic dedup.
func (s *Service) dedupLLMMerge(
	ctx context.Context, m *memory.Memory, above []store.Scored,
) (hit *memory.Memory, supersedeID string) {
	cands := make([]llm.Candidate, 0, len(above))
	for _, c := range above {
		cands = append(cands, llm.Candidate{ID: c.Memory.ID, Content: c.Memory.Content})
	}
	dec, err := s.consolidator.Consolidate(ctx, llm.Input{
		New:        m.Content,
		Tier:       string(m.Tier),
		Candidates: cands,
	})
	if err != nil {
		slog.WarnContext(ctx, "remember: split-dedup LLM merge failed, falling back to deterministic",
			"namespace", m.Namespace, "err", err)
		return nil, ""
	}
	switch dec.Action {
	case llm.ActionNew:
		return nil, ""
	case llm.ActionSupersede:
		if dec.Target != "" {
			return nil, dec.Target
		}
	case llm.ActionUpdate:
		// Merge content into target: rewrite the target's content with the
		// merged text and re-embed. Falls through to store on error.
		if dec.Target == "" {
			return nil, ""
		}
		content := dec.Content
		if content == "" {
			content = m.Content
		}
		target, err := s.store.Get(ctx, m.Namespace, dec.Target)
		if err != nil {
			return nil, ""
		}
		vec, err := embed.EmbedOne(ctx, s.embedder, content)
		if err != nil {
			return nil, ""
		}
		target.Content = content
		target.Embedding = vec
		if dec.Summary != "" {
			target.Summary = dec.Summary
		}
		if err := s.store.Upsert(ctx, target); err != nil {
			return nil, ""
		}
		s.metrics.ConsolidateResult("split-merge-update")
		return target, ""
	}
	return nil, ""
}

// wordSetScore measures how informative a phrasing is: total words plus a 10×
// premium on distinct words, so added detail wins the coalesce tiebreak but
// repetition alone doesn't.
func wordSetScore(s string) int {
	words := strings.Fields(strings.ToLower(s))
	uniq := make(map[string]struct{}, len(words))
	for _, w := range words {
		uniq[w] = struct{}{}
	}
	return len(words) + 10*len(uniq)
}

// clampConfidence bounds a confidence value to [0.1, 0.7], the LLM-provided seed
// range. Below 0.1 is too uncertain to use as a seed; above 0.7 is too high for
// a fact that has never been corroborated.
func clampConfidence(c float64) float64 {
	switch {
	case c < 0.1:
		return 0.1
	case c > 0.7:
		return 0.7
	default:
		return c
	}
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
	s.bg.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.store.SetSuperseded(ctx, ns, oldID, newID); err != nil {
			slog.WarnContext(ctx, "remember: auto-supersede failed",
				"namespace", ns, "old", oldID, "err", err)
		}
	})
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
// stampClassifiedTier marks a heuristically-classified durable write with
// metadata["tier_classified"]="marker" so a bad call is auditable (and
// demotable) later, and records the classification metric. A no-op for
// explicit tiers and the episodic fallback.
func (s *Service) stampClassifiedTier(in RememberInput, tier memory.Tier) RememberInput {
	if in.Tier != "" || tier.Term() != memory.LongTerm {
		return in
	}
	s.metrics.TierClassified(string(tier))
	if in.Metadata == nil {
		in.Metadata = map[string]any{}
	}
	in.Metadata["tier_classified"] = "marker"
	return in
}

// corroborateCooldown rate-limits confidence growth per fact: however many
// times a session restates a fact, it counts as one observation per window.
// Restatement echo must not manufacture confidence — only re-observation
// spread over time may (see arXiv:2606.29279 on redundant sourcing).
const corroborateCooldown = 24 * time.Hour

// corroborateNearestAsync routes a fresh short-term write to the durable fact
// it restates, when one scores at or above corroborateMinScore: the fact is
// reinforced and its confidence grown, off the request path. The nearest-fact
// lookup and the cooldown guard (UpdatedAt is bumped by SetConfidence, so it
// doubles as the last-corroborated stamp) both run in the background goroutine.
func (s *Service) corroborateNearestAsync(ctx context.Context, m *memory.Memory, fresh bool) {
	if !fresh || m.Tier.Term() != memory.ShortTerm ||
		s.corroborateMinScore <= 0 || len(m.Embedding) == 0 {
		return
	}
	bg := context.WithoutCancel(ctx)
	s.bg.Go(func() {
		cctx, cancel := context.WithTimeout(bg, 30*time.Second)
		defer cancel()
		cands, err := s.store.VectorSearch(cctx, m.Namespace, m.Embedding,
			store.Filter{Tiers: []memory.Tier{memory.TierSemantic, memory.TierProcedural}, Now: s.now()}, 1)
		if err != nil {
			slog.WarnContext(cctx, "corroborate: durable lookup failed",
				"namespace", m.Namespace, "err", err)
			s.metrics.CorroborateResult("error")
			return
		}
		if len(cands) == 0 || cands[0].Score < s.corroborateMinScore {
			s.metrics.CorroborateResult("miss")
			return
		}
		fact := cands[0].Memory
		if s.now().Sub(fact.UpdatedAt) < corroborateCooldown {
			s.metrics.CorroborateResult("cooldown")
			return
		}
		s.reinforce(cctx, []store.Scored{{Memory: fact}})
		s.corroborate(cctx, fact)
		s.metrics.CorroborateResult("corroborated")
	})
}

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

// contradictCooldown protects a freshly created fact from immediate
// invalidation, keyed on CreatedAt. It must NOT key on UpdatedAt: corroboration
// bumps UpdatedAt, so a stale fact restated at least daily would be permanently
// shielded from the genuine update that supersedes it — and the blocked update,
// once stored, shadows the stale fact for every retry (bench/interaction_test.go).
// Oscillation needs no window here: an invalidated fact is valid_to-filtered
// out of both the corroborate and contradict lookups, so it can neither regrow
// nor be re-invalidated.
const contradictCooldown = 24 * time.Hour

// contradictNearestAsync is the mirror of corroborateNearestAsync: a fresh
// durable write is routed to the durable fact it contradicts, when one scores
// at or above contradictMinScore AND the lexical detector confirms a value or
// polarity change (not a restatement). That stale fact is invalidated —
// valid_to stamped, confidence shrunk usage-aware so the fresh write outranks
// it — off the request path. The write itself is stored unchanged.
func (s *Service) contradictNearestAsync(ctx context.Context, m *memory.Memory, fresh bool) {
	if !fresh || m.Tier.Term() != memory.LongTerm ||
		s.contradictMinScore <= 0 || len(m.Embedding) == 0 {
		return
	}
	bg := context.WithoutCancel(ctx)
	s.bg.Go(func() {
		cctx, cancel := context.WithTimeout(bg, 30*time.Second)
		defer cancel()
		// k=3: this runs after Upsert, so the top hit may be the write itself —
		// and an earlier same-value update (a blocked or duplicate one) may sit
		// between the write and the stale fact it should invalidate. Scanning
		// past candidates the detector reads as restatements reaches it.
		cands, err := s.store.VectorSearch(cctx, m.Namespace, m.Embedding,
			store.Filter{Tiers: []memory.Tier{memory.TierSemantic, memory.TierProcedural}, Now: s.now()}, 3)
		if err != nil {
			slog.WarnContext(cctx, "contradict: durable lookup failed",
				"namespace", m.Namespace, "err", err)
			s.metrics.ContradictResult("error")
			return
		}
		const miss = "miss"
		result := miss
		for i := range cands {
			if cands[i].Memory.ID == m.ID || cands[i].Score < s.contradictMinScore {
				continue
			}
			fact := cands[i].Memory
			// Never retroactively penalise a legacy row that predates confidence
			// tracking — it is trusted, not stale (mirrors corroborate).
			if fact.Confidence == nil {
				continue
			}
			if s.now().Sub(fact.CreatedAt) < contradictCooldown {
				if result == miss {
					result = "cooldown"
				}
				continue
			}
			if contradict.Classify(m.Content, fact.Content, contradict.Default).Class != contradict.Update {
				if result == miss {
					result = "no_signal"
				}
				continue
			}
			s.invalidate(cctx, fact, m.ID)
			s.metrics.ContradictResult("contradicted")
			return
		}
		s.metrics.ContradictResult(result)
	})
}

// invalidate marks a durable fact as contradicted by newID: its confidence is
// set usage-aware so the fresh contradicting write outranks it under the
// composite (DurableScore = salience × confidence × usage, and the composite is
// relevance-dominated so a bounded shrink alone does not suffice), and its
// valid_to is stamped so it leaves live recall while AsOf can still surface it.
func (s *Service) invalidate(ctx context.Context, m *memory.Memory, newID string) {
	if m.Tier.Term() != memory.LongTerm || m.Confidence == nil {
		return
	}
	now := s.now()
	usage := 1 + math.Log1p(float64(m.AccessCount))
	target := math.Min(m.EffectiveConfidence(now), 0.9*memory.ConfidenceSeedFresh/usage)
	if err := s.store.MarkContradicted(ctx, m.Namespace, m.ID, newID, target, now); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "contradict: mark failed",
			"namespace", m.Namespace, "id", m.ID, "contradicted_by", newID, "err", err)
		return
	}
	slog.InfoContext(ctx, "contradict: invalidated stale fact",
		"namespace", m.Namespace, "id", m.ID, "contradicted_by", newID)
}

// RecallInput describes a hybrid recall query.
type RecallInput struct {
	Namespace string
	Query     string
	Tiers     []memory.Tier
	// Levels restricts recall to memories whose derivation level matches one of the
	// listed values; empty means no level constraint.
	Levels []memory.Level
	// Tags narrows recall to memories carrying every listed tag (AND).
	Tags []string
	// Metadata narrows recall to memories whose top-level metadata contains each
	// listed key=value string pair (AND).
	Metadata map[string]string
	// ExcludeMetadata drops memories whose top-level metadata contains every
	// listed key=value pair (AND), applied after Metadata. Lets a caller exclude
	// its own session's just-captured turns from auto-recall.
	ExcludeMetadata map[string]string
	// IncludeFreshTurns, when true, disables the server-side temporal echo
	// guard for this call: just-captured episodic turns (metadata.format="turn"
	// younger than the server's turnEchoWindow) are NOT dropped. Default
	// (false) means the guard fires — a just-captured turn is still live
	// context and must not be recalled back as long-term memory. Opt out only
	// when a caller genuinely needs fresh turns (e.g. a "what did I just say"
	// debug query).
	IncludeFreshTurns bool
	// QueryRewrite, when true and an LLM answerer is configured, rewrites the
	// query into 2-3 diverse variants before recall and fuses the results via
	// RRF. Cheapest read-path LLM lever; opt-in per call. No-op when no answerer
	// is configured (falls through to single-query recall).
	QueryRewrite bool
	Limit        int
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
	// Namespaces, when non-empty, REPLACES the default read set (Namespace +
	// ancestors + home + links, optionally + subtree) with exactly these
	// namespaces — no cascade merge, no subtree of Namespace unless an entry
	// spells it with "/*". Wins over Scope regardless of its value. An entry
	// ending in "/*" also includes every namespace nested under it. Max 16
	// entries; each is searched with the request's own tier filter (Tiers).
	Namespaces []string
	// Home is the caller's personal namespace (from the X-Memini-Home
	// header), merged read-only into the default read set — durable tiers
	// only, like an ancestor. Empty means no home leg.
	Home string
	// Scope selects the read-set shape: "" or "full" (default: Namespace +
	// ancestors + home + links), "project" (Namespace only, no cascade), or
	// "everywhere" (full + subtree). An unrecognized value is an invalid-input
	// error. Namespaces, when set, replaces the cascade outright regardless of
	// Scope (explicit beats scope). Subtree (legacy) still works standalone;
	// Scope "everywhere" is equivalent to Subtree: true.
	Scope string
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
	// Degraded (output-only) is set to the degradation reason
	// ("embed_error"/"embed_timeout") when this recall fell back to
	// keyword-only search because the query embed failed or timed out. The
	// caller passes the address of a local string; it is left untouched
	// (empty) on a healthy recall. nil disables reporting. Same out-param
	// pattern as MergeHint/AutoSuperseded on RememberInput.
	Degraded *string
	// ReadSet (output-only) is set to the resolved read-set — every namespace
	// this recall searched, with the origin recorded when that leg was
	// appended during resolution (primary/ancestor/home/link/call) and any
	// per-namespace tier restriction. The caller passes the address of a
	// local slice; it is left untouched (nil) when ReadSet is nil. Same
	// out-param pattern as Degraded — lets MCP/REST render read-set
	// provenance (e.g. "from: acme") without a second resolveReadSet call.
	ReadSet *[]ReadSetEntry
	// IncludeLinked, when true, expands recall to include memories linked to
	// any result via LinkedMemoryIDs (1-hop expansion). Linked memories that
	// are superseded are skipped. Default (false) is no expansion.
	IncludeLinked bool
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

// reportRecallDegraded records and surfaces a query-embed failure that
// downgraded a recall to keyword-only search. It is a no-op when embedErr is
// nil (the healthy path). degraded, when non-nil, receives the reason
// ("embed_error"/"embed_timeout") — the RecallInput.Degraded out-param.
func (s *Service) reportRecallDegraded(ctx context.Context, embedErr error, degraded *string) {
	if embedErr == nil {
		return
	}
	reason := "embed_error"
	if errors.Is(embedErr, context.DeadlineExceeded) {
		reason = "embed_timeout"
	}
	slog.WarnContext(ctx, "recall: query embed failed, falling back to keyword-only search", "reason", reason, "err", embedErr)
	s.metrics.RecallDegraded(reason)
	if degraded != nil {
		*degraded = reason
	}
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
	scopeBare, scopeSubtree, err := parseScope(in.Scope)
	if err != nil {
		s.metrics.RecallResult("error", tf, "0")
		return nil, err
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
		Levels:            in.Levels,
		Tags:              in.Tags,
		Metadata:          in.Metadata,
		ExcludeMetadata:   in.ExcludeMetadata,
		IncludeExpired:    in.IncludeExpired,
		IncludeSuperseded: in.IncludeSuperseded,
		Now:               s.now(),
		AsOf:              in.AsOf,
	}

	if results, ok := s.tryQueryRewrite(ctx, in); ok {
		return results, nil
	}

	// Embed the query and resolve the read-set concurrently: two independent
	// blocking calls (embedding, and — for subtree/explicit-namespace recalls —
	// a store.ListNamespaces scan), so overlapping them keeps only the slower on
	// the critical path.
	var vec []float32
	var embedErr error
	var entries []scopeEntry
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
	g1.Go(func() error {
		es, err := s.resolveReadSet(g1ctx, readScope{
			primary:  in.Namespace,
			home:     in.Home,
			explicit: in.Namespaces,
			subtree:  in.Subtree || scopeSubtree,
			bare:     scopeBare,
			reqTiers: in.Tiers,
		})
		if err != nil {
			return fmt.Errorf("recall: resolve read-set: %w", err)
		}
		entries = es
		return nil
	})
	if err := g1.Wait(); err != nil {
		s.metrics.RecallResult("error", tf, "0")
		return nil, err
	}
	s.metrics.OpDuration("recall_embed", time.Since(embedStart))
	s.reportRecallDegraded(ctx, embedErr, in.Degraded)
	if in.ReadSet != nil {
		*in.ReadSet = toReadSetEntries(entries)
	}

	// Over-fetch a deep candidate pool from each strategy: a memory ranked just
	// outside the top k in both legs is invisible at pool depth k, yet RRF would
	// rank it above single-leg hits. Fusion, re-rank, and dedup then cut the
	// pool back down to k.
	poolK := max(k*s.poolFactor, s.poolFloor)

	// Run both legs of every namespace concurrently into pre-sized,
	// index-addressed slots so there is no shared append to guard. SetLimit caps
	// in-flight store calls so a deep subtree can't exhaust the connection pool.
	searchStart := time.Now()
	vres := make([][]store.Scored, len(entries))
	kres := make([][]store.Scored, len(entries))
	g2, g2ctx := errgroup.WithContext(ctx)
	g2.SetLimit(recallSearchConcurrency)
	for i, e := range entries {
		ns := e.ns
		f := filter
		if e.tiers != nil {
			f.Tiers = e.tiers // per-entry override (e.g. global namespace: durable tiers only)
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

	perNS := make([][]store.Scored, len(entries))
	for i := range entries {
		perNS[i] = s.fuseLegs(vres[i], kres[i])
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
	ranked = reserveDurableTiers(ranked, k, s.resolveSemanticReserve(in), s.reservePromoteRatio, s.reserveTopAnchor, s.reserveGatePercentile)
	ranked = s.applyTurnEchoGuard(in, ranked)
	results := s.finalizeRecall(ctx, in.Query, ranked, k)
	results = s.maybeExpandLinked(ctx, in, results, k)
	s.reinforceResults(ctx, results)
	s.metrics.RecallResult("ok", tf, hitsBucket(len(results)))
	return results, nil
}

// fuseLegs fuses the vector and keyword legs of one namespace into a single
// best-first list (RRF by default, or convex score fusion).
func (s *Service) fuseLegs(v, kw []store.Scored) []store.Scored {
	if s.scoreFusionAlpha >= 0 {
		return search.FuseScores([][]store.Scored{v, kw},
			[]float64{s.scoreFusionAlpha, 1 - s.scoreFusionAlpha}, 0)
	}
	return search.Fuse([][]store.Scored{v, kw}, 0, search.DefaultRRFK)
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

// resolveTurnEchoWindow returns the effective temporal echo window: the
// server-wide default, unless the caller opted out via IncludeFreshTurns
// (returns 0 = disabled).
func (s *Service) resolveTurnEchoWindow(in RecallInput) time.Duration {
	if in.IncludeFreshTurns {
		return 0
	}
	return s.turnEchoWindow
}

// tryQueryRewrite handles the query-expansion path: when QueryRewrite is set
// and an answerer is configured, it rewrites the query into 2-3 variants and
// recalls each concurrently (bounded by recallSearchConcurrency), then fuses
// via RRF. Returns (results, true) when the path fires; (nil, false) to fall
// through to single-query recall.
//
// Results are collected into a pre-sized, index-addressed slice (one slot per
// variant) so fusion always sees the variants in expansion order regardless
// of which goroutine finishes first -- same fused order as the old sequential
// loop, just running concurrently.
//
// RecallInput.Degraded is a *string out-param; `sub := in` only copies the
// pointer, so handing every variant goroutine the SAME pointer would be a
// data race the moment more than one variant degrades at once. Each variant
// instead gets its own local slot; once every variant has joined, the
// coordinating goroutine (this one, after g.Wait()) aggregates them into the
// caller's Degraded exactly once.
func (s *Service) tryQueryRewrite(ctx context.Context, in RecallInput) ([]store.Scored, bool) {
	if !in.QueryRewrite || s.answerer == nil {
		return nil, false
	}
	queries := s.expandQueries(ctx, in.Query)
	if len(queries) <= 1 {
		return nil, false
	}
	results := make([][]store.Scored, len(queries))
	degradedReasons := make([]string, len(queries))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(recallSearchConcurrency)
	for i, q := range queries {
		g.Go(func() error {
			sub := in
			sub.Query = q
			sub.QueryRewrite = false // prevent recursion into tryQueryRewrite
			sub.Degraded = &degradedReasons[i]
			if i != 0 {
				// RecallInput.ReadSet is a *[]ReadSetEntry out-param: handing every
				// variant goroutine the SAME pointer would race the moment more
				// than one variant writes concurrently (same hazard as Degraded
				// above). Every variant resolves an identical read-set (same
				// namespace/home/scope, only Query differs), so only the first
				// variant needs to populate it.
				sub.ReadSet = nil
			}
			res, err := s.Recall(gctx, sub)
			if err != nil {
				return err
			}
			results[i] = res
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, false
	}
	if in.Degraded != nil {
		for _, reason := range degradedReasons {
			if reason != "" {
				*in.Degraded = reason
				break
			}
		}
	}
	return search.Fuse(results, in.Limit, search.DefaultRRFK), true
}

// applyTurnEchoGuard drops just-captured episodic turns (metadata.format="turn"
// younger than the server's window) unless the caller opted out. A just-captured
// turn is live context, not long-term memory — echoing it back makes the agent
// parrot itself.
func (s *Service) applyTurnEchoGuard(in RecallInput, ranked []store.Scored) []store.Scored {
	window := s.resolveTurnEchoWindow(in)
	if window <= 0 {
		return ranked
	}
	cutoff := s.now().Add(-window)
	filtered := ranked[:0]
	for _, r := range ranked {
		if r.Memory.Tier.Term() == memory.ShortTerm &&
			r.Memory.CreatedAt.After(cutoff) &&
			isTurnCapture(r.Memory.Metadata) {
			continue
		}
		filtered = append(filtered, r)
	}
	return filtered
}

// isTurnCapture reports whether metadata marks this memory as a captured turn
// (format="turn"), the content the echo guard targets.
func isTurnCapture(meta map[string]any) bool {
	v, ok := meta["format"].(string)
	return ok && v == "turn"
}

// buildFactsOnWrite routes a fresh short-term capture to write-time fact
// building: LLM distillation (batched per session when configured and the
// capture carries a session_id, else per-capture) or the heuristic extractor
// when no LLM is set.
func (s *Service) buildFactsOnWrite(ctx context.Context, m *memory.Memory, tier memory.Tier, isCreate bool) {
	switch {
	case s.shouldDistillShortTermOnWrite(tier, isCreate):
		if !s.enqueueDistillBatch(m) {
			s.distillShortTermAsync(ctx, m)
		}
	case s.shouldExtractShortTermOnWrite(tier, isCreate):
		s.extractShortTermAsync(ctx, m)
	}
}

// shouldDistillShortTermOnWrite reports whether a fresh short-term capture
// should be distilled into durable facts at write time. isCreate must be false
// for an update (an existing row re-written by ID, e.g. a re-fired session
// digest), so a capture is distilled once on creation and never again when
// it's overwritten.
func (s *Service) shouldDistillShortTermOnWrite(tier memory.Tier, isCreate bool) bool {
	return s.distillOnWrite && s.distiller != nil && tier.Term() == memory.ShortTerm && isCreate
}

// shouldExtractShortTermOnWrite mirrors shouldDistillShortTermOnWrite for the
// heuristic path: it requires no distiller, so distill-on-write supersedes it
// when an LLM is set.
func (s *Service) shouldExtractShortTermOnWrite(tier memory.Tier, isCreate bool) bool {
	return s.extractOnWrite && s.distiller == nil && tier.Term() == memory.ShortTerm && isCreate
}

// distillShortTermAsync distils a freshly-written short-term memory into durable
// facts in the background, detached from the request so the capture isn't
// blocked on the LLM. It reuses the promote path (stamp → distill → write
// deduped facts) for one memory, bounded by distillSem so a write burst can't
// fan out unbounded LLM calls. Best-effort: a failure is logged, the source
// stays. Observes semaphore-wait time via OpDuration so queue saturation is
// visible before it silently loses facts to TTL expiry.
func (s *Service) distillShortTermAsync(ctx context.Context, m *memory.Memory) {
	bg := context.WithoutCancel(ctx)
	s.bg.Go(func() {
		semStart := time.Now()
		s.distillSem <- struct{}{}
		semWait := time.Since(semStart)
		defer func() { <-s.distillSem }()
		s.metrics.OpDuration("distill_sem_wait", semWait)
		dctx, cancel := context.WithTimeout(bg, distillOnWriteTimeout)
		defer cancel()
		n, err := s.promote(dctx, m.Namespace, []*memory.Memory{m}, s.now())
		if err != nil {
			slog.WarnContext(dctx, "distill-on-write", "namespace", m.Namespace, "id", m.ID, "err", err)
			return
		}
		// Drop-when-no-fact: the LLM found nothing durable in this turn, so delete
		// the kept source rather than let low-value chatter sit for its full TTL.
		if n == 0 && s.distillDropNoFact {
			if err := s.store.Delete(dctx, m.Namespace, m.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				slog.WarnContext(dctx, "distill-on-write: drop no-fact source", "namespace", m.Namespace, "id", m.ID, "err", err)
				return
			}
			s.metrics.RememberResult("dropped", string(m.Tier))
		}
	})
}

// extractShortTermAsync stores the heuristic extractor's typed facts from a
// freshly-written short-term memory. The marker scan runs inline; only the
// embed+store of each fact is detached. The raw source is kept; a per-fact
// failure is logged without blocking the rest.
func (s *Service) extractShortTermAsync(ctx context.Context, m *memory.Memory) {
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
			// source_id links the derived fact back to the episodic it was mined
			// from, so "why does this memory exist" stays answerable.
			meta := map[string]any{"memory_type": string(r.Kind), "source": "extract", "source_id": m.ID}
			// Extracted facts inherit the parent's session_id: a turn capture's
			// content includes the agent's own response, and without the stamp the
			// integrations' exclude_metadata session guard can never keep a
			// session's own words from echoing back as durable facts.
			if sid, ok := m.Metadata["session_id"].(string); ok && sid != "" {
				meta["session_id"] = sid
			}
			in := RememberInput{
				Namespace: m.Namespace,
				Content:   r.Content,
				Tier:      r.Kind.Tier(),
				Level:     memory.LevelExplicit,
				Tags:      []string{string(r.Kind)},
				Metadata:  meta,
			}
			if _, err := s.Remember(dctx, in); err != nil {
				slog.WarnContext(dctx, "extract-on-write: store fact",
					"namespace", m.Namespace, "id", m.ID, "kind", string(r.Kind), "err", err)
			}
		}
	})
}

// defaultReservePromoteRatio is the evictee-relative leg of the reserve's
// promotion gate: a durable takes a reserved slot only when its composite
// score is at least this fraction of the entry it would evict. 0.5 under the
// relevance-modulated composite imposes the same effective relevance bar the
// benched 0.6 imposed under the old additive composite, which gave durables a
// flat +0.2 floor. This leg is load-bearing on strong flat windows, where the
// evictee scores close to the top hit and the bar exceeds the anchor's: with
// it removed, the spray regression (bench/reserve_test.go) leaks an off-topic
// durable at 0.38x the window top. An adaptive window-percentile gate was
// rejected: a crowded-out durable scores below the window floor by
// construction.
const defaultReservePromoteRatio = 0.5

// defaultReserveTopAnchor is the absolute leg of the promotion gate: a durable
// must also score at least this fraction of the window's top hit. The
// evictee-relative leg degenerates on low-signal windows (one strong hit over
// a noise tail), where the evictee is itself noise and nearly any durable
// clears a bar derived from it. Benched (bench/quality_test.go): relevant
// buried facts score 0.47-0.74 of the window top, off-topic durables on weak
// windows at most ~0.22; 0.4 splits the bands with margin.
const defaultReserveTopAnchor = 0.4

// reserveDurableTiers recomposes a relevance-ordered pool so up to `reserve` of
// the top `limit` slots hold durable tiers (semantic/procedural): durables just
// outside the window are promoted in, evicting the lowest-relevance episodic
// entries, until the reserve is met. Promotion is relevance-gated so a query
// with no relevance-competitive durable keeps its pure-relevance window: with
// gatePct <= 0 a candidate must score at least ratio× the entry it evicts and
// topAnchor× the window's top hit; with gatePct > 0 it must instead reach the
// gatePct-th percentile of the original window's scores.
//
// Promoted durables surface directly below the top hit rather than at the
// window bottom; the top hit is never displaced. Everything else keeps
// relevance order. reserve <= 0 or a pool no deeper than `limit` returns the
// input unchanged.
func reserveDurableTiers(ranked []store.Scored, limit, reserve int, ratio, topAnchor, gatePct float64) []store.Scored {
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

	adaptiveBar := 0.0
	if gatePct > 0 {
		window := make([]float64, limit)
		for i := range limit {
			window[i] = ranked[i].Score
		}
		adaptiveBar = percentile(window, gatePct)
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
		bar := max(ratio*ranked[evict].Score, topAnchor*ranked[0].Score)
		if gatePct > 0 {
			bar = adaptiveBar
		}
		if ranked[i].Score < bar {
			// Not relevance-competitive with what it would displace. Later
			// candidates score lower still and (in ratio mode) the bar never
			// falls — the anchor leg is fixed and each eviction only raises
			// the evictee leg — so none of them can clear it either.
			break
		}
		delete(selected, evict)
		selected[i] = struct{}{}
		durableCount++
	}

	window := make([]store.Scored, 0, limit)
	promoted := make([]store.Scored, 0, reserve)
	rest := make([]store.Scored, 0, len(ranked))
	for i := range ranked {
		_, ok := selected[i]
		switch {
		case ok && i < limit:
			window = append(window, ranked[i])
		case ok:
			promoted = append(promoted, ranked[i])
		default:
			rest = append(rest, ranked[i])
		}
	}
	head := min(1, len(window))
	out := make([]store.Scored, 0, len(ranked))
	out = append(out, window[:head]...)
	out = append(out, promoted...)
	out = append(out, window[head:]...)
	return append(out, rest...)
}

// percentile returns the p-th percentile (0 < p <= 100) of scores by linear
// interpolation over the sorted values. scores is not mutated.
func percentile(scores []float64, p float64) float64 {
	sorted := slices.Clone(scores)
	slices.Sort(sorted)
	if p >= 100 || len(sorted) == 1 {
		return sorted[len(sorted)-1]
	}
	pos := p / 100 * float64(len(sorted)-1)
	lo := int(pos)
	frac := pos - float64(lo)
	return sorted[lo] + frac*(sorted[lo+1]-sorted[lo])
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

// maybeExpandLinked conditionally expands results to include linked memories.
func (s *Service) maybeExpandLinked(ctx context.Context, in RecallInput, results []store.Scored, k int) []store.Scored {
	if !in.IncludeLinked || len(results) == 0 {
		return results
	}
	return s.expandLinked(ctx, results, k)
}

// expandLinked expands results by including memories linked via LinkedMemoryIDs
// (1-hop expansion). Linked memories that are superseded or already in results
// are skipped. Linked results get a score penalty (multiplied by 0.5) so they
// rank below direct hits. The result is re-sorted and truncated to k.
func (s *Service) expandLinked(ctx context.Context, results []store.Scored, k int) []store.Scored {
	// Collect unique linked IDs not already in results, paired with the
	// namespace of the result that linked to them: LinkedMemoryIDs is written
	// alongside its owning memory, so a link always resolves within that
	// memory's own namespace — which, with a multi-namespace read-set, may
	// differ from the request's primary namespace.
	type fetch struct{ id, ns string }
	seen := make(map[string]bool, len(results))
	for _, r := range results {
		seen[r.Memory.ID] = true
	}
	var toFetch []fetch
	for _, r := range results {
		for _, lid := range r.Memory.LinkedMemoryIDs {
			if !seen[lid] {
				seen[lid] = true
				toFetch = append(toFetch, fetch{id: lid, ns: r.Memory.Namespace})
			}
		}
	}
	if len(toFetch) == 0 {
		return results
	}

	// Fetch each linked memory. Skip superseded ones.
	var linked []store.Scored
	// Assign linked memories a score penalty: 0.5 × the minimum score among
	// direct results. This ensures linked hits rank below all direct hits.
	minScore := results[len(results)-1].Score
	for _, f := range toFetch {
		m, err := s.store.Get(ctx, f.ns, f.id)
		if errors.Is(err, store.ErrNotFound) {
			continue // stale link: memory was deleted
		}
		if err != nil {
			slog.WarnContext(ctx, "recall: linked fetch failed", "id", f.id, "namespace", f.ns, "err", err)
			continue
		}
		if m.SupersededBy != nil {
			continue // stale link: target was superseded
		}
		linked = append(linked, store.Scored{
			Memory: m,
			Score:  minScore * 0.5, // penalty factor
		})
	}
	if len(linked) == 0 {
		return results
	}

	// Merge, sort by score descending, truncate to k.
	merged := make([]store.Scored, 0, len(results)+len(linked))
	merged = append(merged, results...)
	merged = append(merged, linked...)
	slices.SortFunc(merged, func(a, b store.Scored) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		return 0
	})
	if k > 0 && len(merged) > k {
		merged = merged[:k]
	}
	return merged
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
// raw working-intake notes.
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
