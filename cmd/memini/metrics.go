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
	rememberResults *prometheus.CounterVec
	recallResults   *prometheus.CounterVec
	forgetResults   *prometheus.CounterVec
	promoteResults  *prometheus.CounterVec
	fsckResults     *prometheus.CounterVec
	answerResults   *prometheus.CounterVec
	rerankResults   *prometheus.CounterVec
	opDuration      *prometheus.HistogramVec

	// store-level
	storeUpsert     *prometheus.CounterVec
	storeDelete     prometheus.Counter
	storeSoftDelete prometheus.Counter
	storeSweep      *prometheus.CounterVec
	activeByTier    *prometheus.GaugeVec

	// embed-level
	embedDuration *prometheus.HistogramVec
	embedTokens   *prometheus.CounterVec
	embedItems    *prometheus.HistogramVec
	embedErrors   *prometheus.CounterVec
}

const (
	labelBackend    = "backend"
	labelHitsBucket = "hits_bucket"
	labelOp         = "op"
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
		opDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "memini_op_duration_seconds",
			Help:    "End-to-end latency of public service operations.",
			Buckets: prometheus.DefBuckets,
		}, []string{labelOp}),
		storeUpsert: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_store_upserts_total",
			Help: "Store upsert outcomes (insert, update) by tier.",
		}, []string{labelOp, labelTier}),
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

func (m *consolidateMetrics) PromoteResult(result string, _ int) {
	m.promoteResults.WithLabelValues(result).Inc()
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

// store.Metrics methods.

func (m *consolidateMetrics) Upsert(op, tier string) {
	m.storeUpsert.WithLabelValues(op, tier).Inc()
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
