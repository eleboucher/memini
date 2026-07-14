//go:build integration

package postgres_test

import (
	"context"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

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

// TestMigrateRenamesProjectMapToPins seeds a scratch database with the
// pre-rename project_map table and checks Open renames it in place: the
// seeded row is readable through GetPins (the table is not recreated empty
// next to a stray), the index is renamed, and no project_map remains. It
// runs in its own database because the shared test database has already
// been migrated, which would mask the rename path.
func TestMigrateRenamesProjectMapToPins(t *testing.T) {
	dsn := os.Getenv("MEMINI_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set MEMINI_TEST_POSTGRES_DSN to run Postgres integration tests")
	}
	ctx := context.Background()

	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		t.Fatalf("this test needs a URL-form DSN to derive a scratch database, got %q", dsn)
	}
	const scratch = "memini_test_pins_rename"

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+scratch+` WITH (FORCE)`); err != nil {
		t.Fatalf("drop stale scratch database: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+scratch); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+scratch+` WITH (FORCE)`)
		_ = admin.Close(ctx)
	})

	u.Path = "/" + scratch
	scratchDSN := u.String()

	seedConn, err := pgx.Connect(ctx, scratchDSN)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	seed := []string{
		`CREATE TABLE project_map (
			key        text PRIMARY KEY,
			namespace  text NOT NULL,
			note       text NOT NULL DEFAULT '',
			created_by text NOT NULL DEFAULT '',
			created_at timestamptz NOT NULL,
			updated_at timestamptz NOT NULL
		)`,
		`CREATE INDEX idx_project_map_ns ON project_map(namespace)`,
		`INSERT INTO project_map (key, namespace, note, created_by, created_at, updated_at)
		 VALUES ('remote:github.com/acme/phoenix', 'acme/phoenix', 'seeded before rename', 'kit',
			'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	}
	for _, q := range seed {
		if _, err := seedConn.Exec(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if err := seedConn.Close(ctx); err != nil {
		t.Fatalf("close seed conn: %v", err)
	}

	st, err := postgres.Open(ctx, scratchDSN, 8)
	if err != nil {
		t.Fatalf("open pre-rename database: %v", err)
	}
	defer func() { _ = st.Close() }()

	got, err := st.GetPins(ctx, []string{"remote:github.com/acme/phoenix"})
	if err != nil {
		t.Fatalf("GetPins: %v", err)
	}
	if len(got) != 1 || got[0].Namespace != "acme/phoenix" || got[0].CreatedBy != "kit" {
		t.Fatalf("GetPins = %+v, want the seeded pre-rename row", got)
	}

	check, err := pgx.Connect(ctx, scratchDSN)
	if err != nil {
		t.Fatalf("connect for schema checks: %v", err)
	}
	defer func() { _ = check.Close(ctx) }()
	for name, wantPresent := range map[string]bool{
		"project_map": false, "pins": true,
		"idx_project_map_ns": false, "idx_pins_ns": true,
	} {
		var reg *string
		if err := check.QueryRow(ctx, `SELECT to_regclass($1)::text`, name).Scan(&reg); err != nil {
			t.Fatalf("to_regclass(%s): %v", name, err)
		}
		if present := reg != nil; present != wantPresent {
			t.Errorf("relation %q present = %v, want %v", name, present, wantPresent)
		}
	}
}
