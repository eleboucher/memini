package service_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/service"
)

// opMetrics records OpDuration calls; everything else is a no-op via the
// embedded interface.
type opMetrics struct {
	service.Metrics
	mu  sync.Mutex
	ops map[string]int
}

func (m *opMetrics) OpDuration(op string, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ops == nil {
		m.ops = map[string]int{}
	}
	m.ops[op]++
}

// TestBriefingRecordsOpDuration pins that Briefing is observable server-side:
// it must record an op_duration sample (op="briefing") like recall/remember
// do, so briefing rate and latency show up in metrics without relying on
// client injection beacons.
func TestBriefingRecordsOpDuration(t *testing.T) {
	m := &opMetrics{Metrics: service.NopMetrics()}
	svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce(), service.WithMetrics(m))
	if _, err := svc.Remember(context.Background(), service.RememberInput{
		Namespace: "acme/api", Content: "the deploy pipeline publishes multi-arch images to the internal registry",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := svc.Briefing(context.Background(), "acme/api", service.BriefingOpts{}); err != nil {
		t.Fatalf("briefing: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ops["briefing"] != 1 {
		t.Fatalf("OpDuration(briefing) calls = %d, want 1 (ops seen: %v)", m.ops["briefing"], m.ops)
	}
}
