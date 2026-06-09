package sqlitevec_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/eleboucher/memini/internal/store/sqlitevec"
	"github.com/eleboucher/memini/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), 8)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	storetest.Run(t, st, 8)
}
