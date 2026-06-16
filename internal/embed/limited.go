package embed

import (
	"context"
	"sync/atomic"

	"golang.org/x/sync/semaphore"
)

// Limited caps in-flight Embed calls. max <= 0 returns inner unchanged.
// onInFlight, if non-nil, receives the absolute in-flight count on every
// acquire and release.
type Limited struct {
	inner      Embedder
	sem        *semaphore.Weighted
	max        int64
	inFlight   atomic.Int64
	onInFlight func(n int64)
}

func NewLimited(inner Embedder, max int, onInFlight func(n int64)) Embedder {
	if max <= 0 || inner == nil {
		return inner
	}
	return &Limited{
		inner:      inner,
		sem:        semaphore.NewWeighted(int64(max)),
		max:        int64(max),
		onInFlight: onInFlight,
	}
}

func (l *Limited) Dims() int { return l.inner.Dims() }

func (l *Limited) Max() int { return int(l.max) }

func (l *Limited) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := l.sem.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	defer l.sem.Release(1)
	defer l.bump(-1)
	l.bump(1)
	return l.inner.Embed(ctx, texts)
}

func (l *Limited) bump(delta int64) {
	n := l.inFlight.Add(delta)
	if l.onInFlight != nil {
		l.onInFlight(n)
	}
}
