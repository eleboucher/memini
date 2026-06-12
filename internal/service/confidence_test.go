package service_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

func TestConfidenceSeededAndCorroborated(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "svc.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithWriteDedup(0.95))

	// A durable write seeds confidence; a short-term write does not track it.
	fact, err := svc.Remember(ctx, service.RememberInput{Namespace: "n", Content: "the db is postgres", Tier: memory.TierSemantic})
	if err != nil {
		t.Fatalf("remember durable: %v", err)
	}
	if fact.Confidence == nil || *fact.Confidence != memory.ConfidenceSeedFresh {
		t.Fatalf("durable seed confidence = %v, want %v", fact.Confidence, memory.ConfidenceSeedFresh)
	}
	scratch, err := svc.Remember(ctx, service.RememberInput{Namespace: "n", Content: "ran the tests", Tier: memory.TierWorking})
	if err != nil {
		t.Fatalf("remember working: %v", err)
	}
	if scratch.Confidence != nil {
		t.Errorf("short-term memory should not track confidence, got %v", *scratch.Confidence)
	}

	// A near-identical repeat corroborates the durable fact: confidence rises.
	if _, err := svc.Remember(ctx, service.RememberInput{Namespace: "n", Content: "the db is postgres", Tier: memory.TierSemantic}); err != nil {
		t.Fatalf("remember repeat: %v", err)
	}
	got, err := svc.Get(ctx, "n", fact.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Confidence == nil || *got.Confidence <= memory.ConfidenceSeedFresh {
		t.Fatalf("corroborated confidence = %v, want > seed %v", got.Confidence, memory.ConfidenceSeedFresh)
	}
}
