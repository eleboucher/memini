package embed_test

import (
	"context"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/embed/embedtest"
)

type slowEmbedder struct {
	embedtest.Fake
	d time.Duration
}

func (s *slowEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	select {
	case <-time.After(s.d):
		return s.Fake.Embed(ctx, texts)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func BenchmarkLimitedBurst(b *testing.B) {
	const (
		sleep = 5 * time.Millisecond
		cap   = 2
		fan   = 16
	)
	inner := &slowEmbedder{Fake: *embedtest.New(4), d: sleep}
	limited := embed.NewLimited(inner, cap, nil)

	b.ResetTimer()
	for range b.N {
		runParallel(limited, fan)
	}
}

func BenchmarkUnlimitedBurst(b *testing.B) {
	const (
		sleep = 5 * time.Millisecond
		fan   = 16
	)
	inner := &slowEmbedder{Fake: *embedtest.New(4), d: sleep}

	b.ResetTimer()
	for range b.N {
		runParallel(inner, fan)
	}
}

func runParallel(e embed.Embedder, fan int) {
	done := make(chan struct{}, fan)
	for range fan {
		go func() {
			_, _ = e.Embed(context.Background(), []string{"x"})
			done <- struct{}{}
		}()
	}
	for range fan {
		<-done
	}
}
