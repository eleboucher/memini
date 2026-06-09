package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/eleboucher/memini/internal/service"
)

// consolidateMetrics is the prometheus-backed service.Metrics implementation.
type consolidateMetrics struct {
	results    *prometheus.CounterVec
	queueDepth prometheus.Gauge
}

var _ service.Metrics = (*consolidateMetrics)(nil)

func newConsolidateMetrics(reg prometheus.Registerer) *consolidateMetrics {
	factory := promauto.With(reg)
	return &consolidateMetrics{
		results: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_consolidate_results_total",
			Help: "Consolidation pipeline outcomes by result (gated, new, update, supersede, noop, error, dropped).",
		}, []string{"result"}),
		queueDepth: factory.NewGauge(prometheus.GaugeOpts{
			Name: "memini_consolidate_queue_depth",
			Help: "Current depth of the async consolidation queue.",
		}),
	}
}

func (m *consolidateMetrics) ConsolidateResult(result string) {
	m.results.WithLabelValues(result).Inc()
}

func (m *consolidateMetrics) ConsolidateQueueDepth(depth int) {
	m.queueDepth.Set(float64(depth))
}
