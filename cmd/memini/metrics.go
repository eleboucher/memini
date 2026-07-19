package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// consolidateMetrics is the prometheus-backed service.Metrics implementation
// for consolidation. The same struct also embeds store.Metrics so the store's
// events (insert/update/delete/sweep) are reported to the same registry.
type consolidateMetrics struct {
	results    *prometheus.CounterVec
	queueDepth prometheus.Gauge

	// service-level (added with the dashboards work)
	rememberResults      *prometheus.CounterVec
	recallResults        *prometheus.CounterVec
	forgetResults        *prometheus.CounterVec
	supersedeResults     *prometheus.CounterVec
	promoteResults       *prometheus.CounterVec
	fsckResults          *prometheus.CounterVec
	answerResults        *prometheus.CounterVec
	rerankResults        *prometheus.CounterVec
	recallDegraded       *prometheus.CounterVec
	recallFloored        *prometheus.CounterVec
	rememberDegraded     *prometheus.CounterVec
	corroborateResults   *prometheus.CounterVec
	contradictResults    *prometheus.CounterVec
	tierClassified       *prometheus.CounterVec
	promoteFacts         prometheus.Counter
	reinforceResults     *prometheus.CounterVec
	writeSanitized       *prometheus.CounterVec
	opDuration           *prometheus.HistogramVec
	embedBackfillPending prometheus.Gauge
	chunkBackfillPending prometheus.Gauge

	// store-level
	storeUpsert     *prometheus.CounterVec
	storeDelete     prometheus.Counter
	storeSoftDelete prometheus.Counter
	storeSweep      *prometheus.CounterVec
	activeByTier    *prometheus.GaugeVec
	dedupTombstoned prometheus.Counter

	// embed-level
	embedDuration  *prometheus.HistogramVec
	embedTokens    *prometheus.CounterVec
	embedItems     *prometheus.HistogramVec
	embedErrors    *prometheus.CounterVec
	embedInFlight  prometheus.Gauge
	rerankInFlight prometheus.Gauge
}

const (
	labelAction     = "action"
	labelBackend    = "backend"
	labelHitsBucket = "hits_bucket"
	labelMemoryType = "memory_type"
	labelOp         = "op"
	labelReason     = "reason"
	labelResult     = "result"
	labelTier       = "tier"
	labelTierFilter = "tier_filter"
)

var (
	_ service.Metrics = (*consolidateMetrics)(nil)
	_ store.Metrics   = (*consolidateMetrics)(nil)
	_ embed.Metrics   = (*consolidateMetrics)(nil)
)

func newConsolidateMetrics(reg prometheus.Registerer) *consolidateMetrics {
	factory := promauto.With(reg)
	m := &consolidateMetrics{
		results: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_consolidate_results_total",
			Help: "Consolidation pipeline outcomes by result (gated, new, update, supersede, noop, error, dropped).",
		}, []string{labelResult}),
		queueDepth: factory.NewGauge(prometheus.GaugeOpts{
			Name: "memini_consolidate_queue_depth",
			Help: "Current depth of the async consolidation queue.",
		}),
		rememberResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_remember_results_total",
			Help: "Outcomes of the Remember API by tier.",
		}, []string{labelResult, labelTier}),
		recallResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_recall_results_total",
			Help: "Outcomes of the Recall API by tier filter and hit-count bucket.",
		}, []string{labelResult, labelTierFilter, labelHitsBucket}),
		forgetResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_forget_results_total",
			Help: "Outcomes of the Forget API (ok, not_found, error).",
		}, []string{labelResult}),
		supersedeResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_supersede_results_total",
			Help: "Outcomes of the Supersede API (ok, not_found, error).",
		}, []string{labelResult}),
		promoteResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_promote_results_total",
			Help: "Outcomes of episodic→semantic promotion.",
		}, []string{labelResult}),
		fsckResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_fsck_results_total",
			Help: "Outcomes of consistency sweeps.",
		}, []string{labelResult}),
		answerResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_answer_results_total",
			Help: "Outcomes of the Answer API (ok, error).",
		}, []string{labelResult}),
		rerankResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_rerank_results_total",
			Help: "Recall rerank outcomes by backend (llm, cross_encoder) and result (ok, fallback).",
		}, []string{labelBackend, labelResult}),
		recallDegraded: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_recall_degraded_total",
			Help: "Recalls that fell back to keyword-only search by reason (embed_timeout, embed_error).",
		}, []string{labelReason}),
		recallFloored: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_recall_floored_total",
			Help: "Recall candidates dropped from the response by the min_rank_score composite floor, by tier filter.",
		}, []string{labelTierFilter}),
		rememberDegraded: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_remember_degraded_total",
			Help: "Writes that stored without a vector (keyword-searchable only, pending_embed) by reason (embed_timeout, embed_error).",
		}, []string{labelReason}),
		corroborateResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_corroborate_results_total",
			Help: "Corroboration-routing attempts on fresh short-term writes (corroborated, cooldown, miss, error).",
		}, []string{labelResult}),
		contradictResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_contradict_results_total",
			Help: "Contradiction-routing attempts on fresh durable writes (contradicted, no_signal, cooldown, miss, error).",
		}, []string{labelResult}),
		tierClassified: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_tier_classified_total",
			Help: "Omitted-tier writes the marker classifier routed to a durable tier, by tier.",
		}, []string{labelTier}),
		promoteFacts: factory.NewCounter(prometheus.CounterOpts{
			Name: "memini_promote_facts_total",
			Help: "Durable facts written by the promotion pass (LLM distiller or marker extractor).",
		}),
		reinforceResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_reinforce_results_total",
			Help: "Best-effort recall reinforcement writes (ok, error).",
		}, []string{labelResult}),
		writeSanitized: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_write_sanitized_total",
			Help: "Ingestion content-hygiene actions by action (cleaned, quarantined).",
		}, []string{labelAction}),
		opDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "memini_op_duration_seconds",
			Help:    "End-to-end latency of public service operations.",
			Buckets: prometheus.DefBuckets,
		}, []string{labelOp}),
		embedBackfillPending: factory.NewGauge(prometheus.GaugeOpts{
			Name: "memini_embed_backfill_pending",
			Help: "Memories still marked pending_embed (stored vectorless) after the most recent backfill tick.",
		}),
		chunkBackfillPending: factory.NewGauge(prometheus.GaugeOpts{
			Name: "memini_chunk_backfill_pending",
			Help: "Long memories still without chunk vectors after the most recent chunk-backfill tick. " +
				"A number that never falls means chunked recall is not reaching those memories.",
		}),
		storeUpsert: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_store_upserts_total",
			Help: "Store upsert outcomes (insert, update) by tier and typed-extraction memory_type.",
		}, []string{labelOp, labelTier, labelMemoryType}),
		storeDelete: factory.NewCounter(prometheus.CounterOpts{
			Name: "memini_store_deletes_total",
			Help: "Hard deletes (Forget) executed by the store.",
		}),
		storeSoftDelete: factory.NewCounter(prometheus.CounterOpts{
			Name: "memini_store_soft_deletes_total",
			Help: "Tombstones (SetSuperseded) written by consolidation.",
		}),
		storeSweep: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_store_swept_total",
			Help: "Memories purged by the decay sweeper, by tier.",
		}, []string{labelTier}),
		activeByTier: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "memini_memories_active",
			Help: "Live (non-superseded, non-expired) memory count by tier, refreshed after sweeps and fsck.",
		}, []string{labelTier}),
		dedupTombstoned: factory.NewCounter(prometheus.CounterOpts{
			Name: "memini_dedup_tombstoned_total",
			Help: "Memories tombstoned by the dedup pass (periodic job or one-shot call).",
		}),
		embedDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "memini_embed_duration_seconds",
			Help:    "Embedder call latency, by backend layer (openai, cached, diskcache, batched, disabled).",
			Buckets: prometheus.DefBuckets,
		}, []string{labelBackend}),
		embedTokens: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_embed_tokens_total",
			Help: "Cumulative embedding tokens reported by the API, by backend (only set for the openai backend).",
		}, []string{labelBackend}),
		embedItems: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "memini_embed_items",
			Help:    "Number of input texts per Embed call, by backend.",
			Buckets: []float64{1, 2, 4, 8, 16, 32, 64, 128},
		}, []string{labelBackend}),
		embedErrors: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_embed_errors_total",
			Help: "Embedder failures, by backend.",
		}, []string{labelBackend}),
		embedInFlight: factory.NewGauge(prometheus.GaugeOpts{
			Name: "memini_embed_in_flight",
			Help: "Embeddings calls currently in flight against the backend (post-cap, not cache hits).",
		}),
		rerankInFlight: factory.NewGauge(prometheus.GaugeOpts{
			Name: "memini_rerank_in_flight",
			Help: "Rerank calls currently in flight against the backend.",
		}),
	}
	return m
}

// service.Metrics methods.

func (m *consolidateMetrics) ConsolidateResult(result string) {
	m.results.WithLabelValues(result).Inc()
}

func (m *consolidateMetrics) ConsolidateQueueDepth(depth int) {
	m.queueDepth.Set(float64(depth))
}

func (m *consolidateMetrics) RememberResult(result, tier string) {
	m.rememberResults.WithLabelValues(result, tier).Inc()
}

func (m *consolidateMetrics) RecallResult(result, tierFilter, hitsBucket string) {
	m.recallResults.WithLabelValues(result, tierFilter, hitsBucket).Inc()
}

func (m *consolidateMetrics) ForgetResult(result string) {
	m.forgetResults.WithLabelValues(result).Inc()
}

func (m *consolidateMetrics) SupersedeResult(result string) {
	m.supersedeResults.WithLabelValues(result).Inc()
}

func (m *consolidateMetrics) PromoteResult(result string, facts int) {
	m.promoteResults.WithLabelValues(result).Inc()
	m.promoteFacts.Add(float64(facts))
}

func (m *consolidateMetrics) FsckResult(result string) {
	m.fsckResults.WithLabelValues(result).Inc()
}

func (m *consolidateMetrics) OpDuration(op string, d time.Duration) {
	m.opDuration.WithLabelValues(op).Observe(d.Seconds())
}

func (m *consolidateMetrics) AnswerResult(result string) {
	m.answerResults.WithLabelValues(result).Inc()
}

func (m *consolidateMetrics) RerankResult(backend, result string) {
	m.rerankResults.WithLabelValues(backend, result).Inc()
}

func (m *consolidateMetrics) RecallDegraded(reason string) {
	m.recallDegraded.WithLabelValues(reason).Inc()
}

func (m *consolidateMetrics) RecallFloored(tierFilter string, n int) {
	if n > 0 {
		m.recallFloored.WithLabelValues(tierFilter).Add(float64(n))
	}
}

func (m *consolidateMetrics) RememberDegraded(reason string) {
	m.rememberDegraded.WithLabelValues(reason).Inc()
}

func (m *consolidateMetrics) WriteSanitized(action string) {
	m.writeSanitized.WithLabelValues(action).Inc()
}

func (m *consolidateMetrics) ReinforceResult(result string) {
	m.reinforceResults.WithLabelValues(result).Inc()
}

// store.Metrics methods.

func (m *consolidateMetrics) Upsert(op, tier, memoryType string) {
	m.storeUpsert.WithLabelValues(op, tier, memoryType).Inc()
}

func (m *consolidateMetrics) Delete() {
	m.storeDelete.Inc()
}

func (m *consolidateMetrics) SoftDelete() {
	m.storeSoftDelete.Inc()
}

func (m *consolidateMetrics) SweepExpired(tier string) {
	m.storeSweep.WithLabelValues(tier).Inc()
}

func (m *consolidateMetrics) ActiveByTier(tier string, n int) {
	m.activeByTier.WithLabelValues(tier).Set(float64(n))
}

func (m *consolidateMetrics) CorroborateResult(result string) {
	m.corroborateResults.WithLabelValues(result).Inc()
}

func (m *consolidateMetrics) ContradictResult(result string) {
	m.contradictResults.WithLabelValues(result).Inc()
}

func (m *consolidateMetrics) TierClassified(tier string) {
	m.tierClassified.WithLabelValues(tier).Inc()
}

func (m *consolidateMetrics) EmbedBackfillPending(n int) {
	m.embedBackfillPending.Set(float64(n))
}

func (m *consolidateMetrics) ChunkBackfillPending(n int) {
	m.chunkBackfillPending.Set(float64(n))
}

func (m *consolidateMetrics) DedupTombstoned(n int) {
	if n > 0 {
		m.dedupTombstoned.Add(float64(n))
	}
}

// embed.Metrics methods.

func (m *consolidateMetrics) Observe(backend string, items, tokens int, d time.Duration) {
	// We piggy-back on the store histograms/counters for now: a per-backend
	// latency histogram and a tokens counter are the most actionable signals.
	// Re-use a fixed metric name with a backend label to keep cardinality
	// bounded (4-5 backends).
	m.embedDuration.WithLabelValues(backend).Observe(d.Seconds())
	if tokens > 0 {
		m.embedTokens.WithLabelValues(backend).Add(float64(tokens))
	}
	if items > 0 {
		m.embedItems.WithLabelValues(backend).Observe(float64(items))
	}
}

func (m *consolidateMetrics) Error(backend string) {
	m.embedErrors.WithLabelValues(backend).Inc()
}

// In-flight hooks for the Limited wrappers. The wrapper accepts a func so
// this package doesn't need to depend on the embed/rerank packages for a
// prom gauge; nil is a safe no-op inside the wrappers.

func (m *consolidateMetrics) EmbedInFlight(n int64) {
	m.embedInFlight.Set(float64(n))
}

func (m *consolidateMetrics) RerankInFlight(n int64) {
	m.rerankInFlight.Set(float64(n))
}
