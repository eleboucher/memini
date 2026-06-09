//go:build integration

package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/eleboucher/memini/internal/store/postgres"
	"github.com/eleboucher/memini/internal/store/storetest"
)

// TestConformance runs the shared store conformance suite against a real
// Postgres+VectorChord instance. Set MEMINI_TEST_POSTGRES_DSN to enable it
// (CI provides a vchord-postgres service); it skips otherwise.
func TestConformance(t *testing.T) {
	dsn := os.Getenv("MEMINI_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set MEMINI_TEST_POSTGRES_DSN to run Postgres integration tests")
	}
	st, err := postgres.Open(context.Background(), dsn, 8)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	storetest.Run(t, st, 8)
}
