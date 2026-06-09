package embed

import (
	"context"
	"time"
)

// Metrics receives embedder events for observability. Methods must be safe for
// concurrent use; a nil Metrics is replaced by a no-op.
type Metrics interface {
	// Observe records one Embed call: backend labels the layer that produced
	// the result ("openai", "cached", "diskcache", "batched", "disabled"),
	// items is the number of input texts, tokens is the API-reported token
	// usage (0 for cache hits and the disabled backend), and d is the wall
	// duration.
	Observe(backend string, items int, tokens int, d time.Duration)
	// Error records a failed Embed call on the given backend.
	Error(backend string)
}

type nopMetrics struct{}

func (nopMetrics) Observe(string, int, int, time.Duration) {}
func (nopMetrics) Error(string)                            {}

// instrumented wraps an Embedder and reports each call. It is the single
// instrumentation point for the embed pipeline's wrappers (cache, batch,
// disk-cache, disabled): every code path passes through here, so callers
// don't need to thread a metrics field into each constructor.
//
// The OpenAIClient is instrumented directly (so it can read the API's
// reported token count) and is *not* wrapped again — see newOpenAIMetrics.
type instrumented struct {
	inner   Embedder
	backend string
	metrics Metrics
}

// Instrument returns e wrapped with metrics. backend labels the outer
// implementation (e.g. "cached", "diskcache", "batched", "disabled") so
// dashboards can split the latency of real network work from cache hits.
func Instrument(e Embedder, backend string, m Metrics) Embedder {
	if m == nil {
		return e
	}
	return &instrumented{inner: e, backend: backend, metrics: m}
}

func (i *instrumented) Dims() int { return i.inner.Dims() }

func (i *instrumented) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	start := time.Now()
	vecs, err := i.inner.Embed(ctx, texts)
	d := time.Since(start)
	if err != nil {
		i.metrics.Error(i.backend)
		return nil, err
	}
	i.metrics.Observe(i.backend, len(texts), 0, d)
	return vecs, nil
}
