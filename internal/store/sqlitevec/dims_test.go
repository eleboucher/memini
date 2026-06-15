package sqlitevec_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestOpenRejectsDimsMismatch verifies that reopening a database with a
// different embedding dimensionality fails fast at Open with a clear error,
// rather than silently accepting it (CREATE VIRTUAL TABLE IF NOT EXISTS is a
// no-op) and erroring on every later Upsert.
func TestOpenRejectsDimsMismatch(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dims.db")

	st, err := sqlitevec.Open(ctx, path, 8)
	if err != nil {
		t.Fatalf("open with dims=8: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopening with a different width must fail with a descriptive error.
	_, err = sqlitevec.Open(ctx, path, 16)
	if err == nil {
		t.Fatal("reopen with dims=16: want error, got nil")
	}
	if !strings.Contains(err.Error(), "8") || !strings.Contains(err.Error(), "16") {
		t.Errorf("error should name both the existing (8) and configured (16) dims: %v", err)
	}

	// Reopening with the original width still works.
	st2, err := sqlitevec.Open(ctx, path, 8)
	if err != nil {
		t.Fatalf("reopen with matching dims=8: %v", err)
	}
	_ = st2.Close()
}
