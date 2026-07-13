//go:build integration

package postgres_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eleboucher/memini/internal/store/postgres"
)

// TestAPIKeySettingsToleratesUnknownField mirrors sqlitevec's
// TestAPIKeySettingsToleratesUnknownField (clientsettings_internal_test.go):
// it seeds the api_keys.settings column with a raw blob carrying a field
// store.ClientSettings does not know about (as if an older binary were
// reading a newer writer's blob), then reads the key back through the store
// and checks the decode tolerates it, keeping every recognized field.
//
// Unlike sqlitevec (where the equivalent column is plain TEXT), Postgres's
// api_keys.settings is a genuine jsonb column, so this also proves the round
// trip through Postgres's own JSON handling stays tolerant, not just
// encoding/json's default behavior — see scanAPIKey's "Tolerant decode"
// comment in helpers.go.
func TestAPIKeySettingsToleratesUnknownField(t *testing.T) {
	dsn := os.Getenv("MEMINI_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set MEMINI_TEST_POSTGRES_DSN to run Postgres integration tests")
	}
	ctx := context.Background()

	st, err := postgres.Open(ctx, dsn, 8)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// A second, direct connection to poke the stored blob past the store's own
	// (always-valid) JSON marshaling — the same reason sqlitevec's test writes
	// straight to its meta/api_keys tables rather than through PutAPIKey.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	const name = "legacy-reader-pg"
	const hash = "unknown-field-pg-deadbeef"
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE name=$1`, name) })

	rawSettings := `{"auto_save": false, "some_future_client_field": ["a", "b"]}`
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_keys (name, key_hash, home_ns, default_ns, created_at, disabled, settings)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
		 ON CONFLICT (name) DO UPDATE SET key_hash=EXCLUDED.key_hash, settings=EXCLUDED.settings`,
		name, hash, "", "", time.Now().UTC(), false, rawSettings,
	); err != nil {
		t.Fatalf("seed raw api_keys row: %v", err)
	}

	got, err := st.GetAPIKeyByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetAPIKeyByHash with an unknown field in settings: %v", err)
	}
	if got == nil {
		t.Fatalf("GetAPIKeyByHash: got nil")
	}
	if got.Settings.AutoSave == nil || *got.Settings.AutoSave != false {
		t.Fatalf("auto_save = %v, want false", got.Settings.AutoSave)
	}

	all, err := st.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("ListAPIKeys with an unknown field in settings: %v", err)
	}
	var found bool
	for _, k := range all {
		if k.Name != name {
			continue
		}
		found = true
		if k.Settings.AutoSave == nil || *k.Settings.AutoSave != false {
			t.Fatalf("ListAPIKeys entry = %+v, want auto_save=false", k)
		}
	}
	if !found {
		t.Fatalf("ListAPIKeys did not include %q", name)
	}
}
