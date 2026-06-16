package sqlitevec_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestEmbedModelRoundTrip verifies the recorded embedding model is empty on a
// fresh store, persists once set, and survives reopen.
func TestEmbedModelRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "model.db")

	st, err := sqlitevec.Open(ctx, path, 8)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	got, err := st.EmbedModel(ctx)
	if err != nil {
		t.Fatalf("EmbedModel on fresh store: %v", err)
	}
	if got != "" {
		t.Fatalf("fresh store should record no model, got %q", got)
	}

	if err := st.SetEmbedModel(ctx, "text-embedding-3-small"); err != nil {
		t.Fatalf("SetEmbedModel: %v", err)
	}
	// Overwrite to confirm upsert semantics.
	if err := st.SetEmbedModel(ctx, "text-embedding-3-large"); err != nil {
		t.Fatalf("SetEmbedModel overwrite: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, err := sqlitevec.Open(ctx, path, 8)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st2.Close() }()
	got, err = st2.EmbedModel(ctx)
	if err != nil {
		t.Fatalf("EmbedModel after reopen: %v", err)
	}
	if got != "text-embedding-3-large" {
		t.Fatalf("recorded model should persist, got %q", got)
	}
}
