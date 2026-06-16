package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

const reembedTestDims = 64

func openModelStore(t *testing.T) *sqlitevec.Store {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "m.db"), reembedTestDims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestReconcileEmbedModelAdoptsOnFreshStore records the configured model when
// the store has none, leaving subsequent same-model starts as no-ops.
func TestReconcileEmbedModelAdoptsOnFreshStore(t *testing.T) {
	ctx := context.Background()
	st := openModelStore(t)
	cfg := &config.Config{EmbedModel: "model-a", EmbedBaseURL: "http://x"}

	if err := reconcileEmbedModel(ctx, st, embedtest.New(reembedTestDims), cfg, quietLog()); err != nil {
		t.Fatalf("reconcile fresh: %v", err)
	}
	got, _ := st.EmbedModel(ctx)
	if got != "model-a" {
		t.Fatalf("recorded model = %q, want model-a", got)
	}
	// Same model again is a no-op and must not error.
	if err := reconcileEmbedModel(ctx, st, embedtest.New(reembedTestDims), cfg, quietLog()); err != nil {
		t.Fatalf("reconcile same model: %v", err)
	}
}

// TestReconcileEmbedModelRefusesByDefault fails a model change when the opt-in
// flag is off, naming both models and the escape hatches.
func TestReconcileEmbedModelRefusesByDefault(t *testing.T) {
	ctx := context.Background()
	st := openModelStore(t)
	if err := st.SetEmbedModel(ctx, "model-a"); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	cfg := &config.Config{EmbedModel: "model-b", EmbedBaseURL: "http://x"}

	err := reconcileEmbedModel(ctx, st, embedtest.New(reembedTestDims), cfg, quietLog())
	if err == nil {
		t.Fatal("want error on model change with flag off, got nil")
	}
	for _, want := range []string{"model-a", "model-b", "MEMINI_REEMBED_ON_MODEL_CHANGE", "memini reembed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	// The recorded model must be unchanged after a refusal.
	if got, _ := st.EmbedModel(ctx); got != "model-a" {
		t.Errorf("recorded model changed to %q after refusal", got)
	}
}

// TestReconcileEmbedModelAutoReembeds re-embeds and adopts the new model when
// the opt-in flag is set.
func TestReconcileEmbedModelAutoReembeds(t *testing.T) {
	ctx := context.Background()
	st := openModelStore(t)
	emb := embedtest.New(reembedTestDims)
	if err := st.SetEmbedModel(ctx, "model-a"); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	// A memory carrying a deliberately wrong (zero) vector.
	zero := make([]float32, reembedTestDims)
	ts := time.Now().UTC()
	m := &memory.Memory{
		ID: "x", Namespace: "ns", Tier: memory.TierSemantic, Content: "the sky is blue",
		CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts, Embedding: zero,
	}
	if err := st.Upsert(ctx, m); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	cfg := &config.Config{EmbedModel: "model-b", EmbedBaseURL: "http://x", ReembedOnModelChange: true}
	if err := reconcileEmbedModel(ctx, st, emb, cfg, quietLog()); err != nil {
		t.Fatalf("reconcile auto-reembed: %v", err)
	}

	if got, _ := st.EmbedModel(ctx); got != "model-b" {
		t.Fatalf("recorded model = %q, want model-b after auto-reembed", got)
	}
	// The vector was rewritten: a content-matching search now finds the memory.
	vec, _ := emb.Embed(ctx, []string{"the sky is blue"})
	hits, err := st.VectorSearch(ctx, "ns", vec[0], store.Filter{}, 1)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 || hits[0].Memory.ID != "x" {
		t.Errorf("memory not re-embedded: hits=%v", hits)
	}
}

// TestReconcileEmbedModelAutoReembedNeedsEndpoint refuses auto-reembed when no
// embeddings endpoint is configured, rather than calling a disabled embedder.
func TestReconcileEmbedModelAutoReembedNeedsEndpoint(t *testing.T) {
	ctx := context.Background()
	st := openModelStore(t)
	if err := st.SetEmbedModel(ctx, "model-a"); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	cfg := &config.Config{EmbedModel: "model-b", ReembedOnModelChange: true} // EmbedBaseURL empty

	err := reconcileEmbedModel(ctx, st, embedtest.New(reembedTestDims), cfg, quietLog())
	if err == nil || !strings.Contains(err.Error(), "MEMINI_EMBED_BASE_URL") {
		t.Fatalf("want endpoint error, got %v", err)
	}
}
