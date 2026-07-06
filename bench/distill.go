package bench

import (
	"context"
	"encoding/json"
	"sync/atomic"

	"github.com/eleboucher/memini/internal/llm"
)

// CountingDistiller wraps a Distiller with cost/compression counters so a
// write-mode run can report what distill-on-write spent at ingest. Token
// counts use the same ~4 bytes/token estimate as the retrieval metrics and
// cover the JSON payloads only; the fixed distill prompt template is per-call
// overhead on top.
type CountingDistiller struct {
	inner llm.Distiller

	calls     atomic.Int64
	errs      atomic.Int64
	episodes  atomic.Int64
	facts     atomic.Int64
	inTokens  atomic.Int64
	outTokens atomic.Int64
}

// NewCountingDistiller wraps inner with usage counters.
func NewCountingDistiller(inner llm.Distiller) *CountingDistiller {
	return &CountingDistiller{inner: inner}
}

// Distill forwards to the wrapped distiller, counting episodes consumed,
// facts produced, and estimated payload tokens in each direction.
func (c *CountingDistiller) Distill(ctx context.Context, in llm.DistillInput) ([]llm.Fact, error) {
	c.calls.Add(1)
	c.episodes.Add(int64(len(in.Episodes)))
	if buf, err := json.Marshal(in); err == nil {
		c.inTokens.Add(int64(estimateTokens(string(buf))))
	}
	facts, err := c.inner.Distill(ctx, in)
	if err != nil {
		c.errs.Add(1)
		return nil, err
	}
	c.facts.Add(int64(len(facts)))
	if buf, err := json.Marshal(facts); err == nil {
		c.outTokens.Add(int64(estimateTokens(string(buf))))
	}
	return facts, nil
}

// DistillStats is a snapshot of a CountingDistiller's counters.
type DistillStats struct {
	Calls     int64 `json:"calls"`
	Errors    int64 `json:"errors"`
	Episodes  int64 `json:"episodes"`
	Facts     int64 `json:"facts"`
	InTokens  int64 `json:"in_tokens_est"`
	OutTokens int64 `json:"out_tokens_est"`
}

// Stats returns the current counter values.
func (c *CountingDistiller) Stats() DistillStats {
	return DistillStats{
		Calls:     c.calls.Load(),
		Errors:    c.errs.Load(),
		Episodes:  c.episodes.Load(),
		Facts:     c.facts.Load(),
		InTokens:  c.inTokens.Load(),
		OutTokens: c.outTokens.Load(),
	}
}
