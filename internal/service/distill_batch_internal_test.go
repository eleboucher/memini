package service

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

const batchTestDims = 64

// recordingDistiller records each Distill call's episode batch.
type recordingDistiller struct {
	mu      sync.Mutex
	batches [][]llm.Episode
}

func (d *recordingDistiller) Distill(_ context.Context, in llm.DistillInput) ([]llm.Fact, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.batches = append(d.batches, in.Episodes)
	return nil, nil
}

func (d *recordingDistiller) calls() [][]llm.Episode {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]llm.Episode(nil), d.batches...)
}

func newBatchService(t *testing.T, d llm.Distiller, maxTokens int, maxAge time.Duration, clock func() time.Time) *Service {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "batch.db"), batchTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st, embedtest.New(batchTestDims), WithSyncReinforce(), WithClock(clock),
		WithDistiller(d), WithDistillOnWrite(true), WithDistillBatch(maxTokens, maxAge))
}

func rememberTurn(t *testing.T, svc *Service, session, content string) {
	t.Helper()
	_, err := svc.Remember(context.Background(), RememberInput{
		Namespace: "alice", Content: content, Tier: memory.TierEpisodic,
		Metadata: map[string]any{"session_id": session},
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
}

// TestDistillBatchTokenFlush pins the token trigger: turns accumulate below the
// threshold (no LLM call), and crossing it flushes the whole session as one
// Distill call carrying every buffered turn.
func TestDistillBatchTokenFlush(t *testing.T) {
	d := &recordingDistiller{}
	now := time.Unix(1_700_000_000, 0).UTC()
	// ~15 estimated tokens per 60-char turn; threshold 40 → third turn flushes.
	svc := newBatchService(t, d, 40, time.Hour, func() time.Time { return now })

	turn := "User: how are the deploys wired?\nAssistant: staging first"
	rememberTurn(t, svc, "s1", turn)
	rememberTurn(t, svc, "s1", turn+" then prod")
	svc.WaitBackground()
	if got := d.calls(); len(got) != 0 {
		t.Fatalf("below threshold: want 0 distill calls, got %d", len(got))
	}

	rememberTurn(t, svc, "s1", turn+" with a canary in between")
	svc.WaitBackground()
	got := d.calls()
	if len(got) != 1 {
		t.Fatalf("want 1 batched distill call, got %d", len(got))
	}
	if len(got[0]) != 3 {
		t.Fatalf("batched call should carry all 3 turns, got %d", len(got[0]))
	}
}

// TestDistillBatchAgeFlush pins the age trigger, driving the check directly
// against the fake clock (the production ticker just calls the same check).
func TestDistillBatchAgeFlush(t *testing.T) {
	d := &recordingDistiller{}
	now := time.Unix(1_700_000_000, 0).UTC()
	clock := func() time.Time { return now }
	svc := newBatchService(t, d, 1_000_000, 10*time.Minute, clock)

	rememberTurn(t, svc, "s1", "User: remember the registry moved to harbor.internal")
	svc.flushDistillBatches(func(b *distillBuffer) bool {
		return svc.now().Sub(b.oldest) >= svc.distillBatch.maxAge
	})
	svc.WaitBackground()
	if got := d.calls(); len(got) != 0 {
		t.Fatalf("fresh buffer must not age-flush, got %d calls", len(got))
	}

	now = now.Add(10 * time.Minute)
	svc.flushDistillBatches(func(b *distillBuffer) bool {
		return svc.now().Sub(b.oldest) >= svc.distillBatch.maxAge
	})
	svc.WaitBackground()
	if got := d.calls(); len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("want 1 age-flushed distill call with 1 turn, got %v", got)
	}
}

// TestDistillBatchNoSessionFallsBack pins that a capture without a session_id
// skips the batcher and distills per-capture immediately.
func TestDistillBatchNoSessionFallsBack(t *testing.T) {
	d := &recordingDistiller{}
	now := time.Unix(1_700_000_000, 0).UTC()
	svc := newBatchService(t, d, 1_000_000, time.Hour, func() time.Time { return now })

	if _, err := svc.Remember(context.Background(), RememberInput{
		Namespace: "alice", Content: "User: the auth service uses jose", Tier: memory.TierEpisodic,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	svc.WaitBackground()
	if got := d.calls(); len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("no session_id: want an immediate per-capture distill, got %v", got)
	}
}

// TestDistillBatchBufferCapEvictsStalest pins the memory bound: past
// distillBatchMaxBuffers live sessions, the stalest buffer is flushed early
// instead of the map growing without bound.
func TestDistillBatchBufferCapEvictsStalest(t *testing.T) {
	d := &recordingDistiller{}
	now := time.Unix(1_700_000_000, 0).UTC()
	svc := newBatchService(t, d, 1_000_000, time.Hour, func() time.Time { return now })

	for i := range distillBatchMaxBuffers + 1 {
		now = now.Add(time.Second) // distinct ages so "stalest" is well-defined
		// Unique content per turn — an exact repeat would be absorbed by
		// fingerprint dedup before ever reaching the batcher.
		rememberTurn(t, svc, fmt.Sprintf("s%d", i), fmt.Sprintf("User: note for topic %d", i))
	}
	svc.WaitBackground()
	if got := d.calls(); len(got) != 1 {
		t.Fatalf("want exactly the stalest buffer flushed, got %d calls", len(got))
	}
	svc.distillBatch.mu.Lock()
	live := len(svc.distillBatch.buffers)
	svc.distillBatch.mu.Unlock()
	if live != distillBatchMaxBuffers {
		t.Fatalf("live buffers = %d, want %d", live, distillBatchMaxBuffers)
	}
}

// TestDistillBatcherShutdownFlush pins that cancelling StartDistillBatcher
// flushes every remaining buffer.
func TestDistillBatcherShutdownFlush(t *testing.T) {
	d := &recordingDistiller{}
	now := time.Unix(1_700_000_000, 0).UTC()
	svc := newBatchService(t, d, 1_000_000, time.Hour, func() time.Time { return now })

	rememberTurn(t, svc, "s1", "User: the ingress uses traefik")
	rememberTurn(t, svc, "s2", "User: metrics land in victoria-metrics")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.StartDistillBatcher(ctx); close(done) }()
	cancel()
	<-done
	svc.WaitBackground()
	if got := d.calls(); len(got) != 2 {
		t.Fatalf("shutdown should flush both session buffers, got %d calls", len(got))
	}
}
