package sqlitevec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	vec "github.com/asg017/sqlite-vec-go-bindings/ncruces"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

func hasColumn(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	err := db.QueryRow(`SELECT 1 FROM pragma_table_info('memories') WHERE name=?`, name).Scan(new(int))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("check column %s: %v", name, err)
	}
	return err == nil
}

func hasIndex(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	err := db.QueryRow(`SELECT 1 FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(new(int))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("check index %s: %v", name, err)
	}
	return err == nil
}

func hasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	err := db.QueryRow(`SELECT 1 FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(new(int))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("check table %s: %v", name, err)
	}
	return err == nil
}

// unit returns the i-th standard basis vector for the test embedding dim (8).
func unit(i int) []float32 {
	v := make([]float32, 8)
	v[i] = 1
	return v
}

type legacyRow struct {
	rowID     int64
	id        string
	namespace string
	tier      memory.Tier
	content   string
	embedding []float32
}

// seedLegacyDB writes a DB with the pre-fingerprint schema (memories missing the
// newer columns, plus the unchanged vec0/fts5 tables) and the given rows across
// all three tables — a stand-in for a volume created by an older release.
func seedLegacyDB(t *testing.T, path string, rows []legacyRow) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	defer func() { _ = db.Close() }()

	schema := []string{
		`CREATE TABLE memories (
			rowid            INTEGER PRIMARY KEY,
			id               TEXT NOT NULL UNIQUE,
			namespace        TEXT NOT NULL,
			tier             TEXT NOT NULL,
			content          TEXT NOT NULL,
			summary          TEXT NOT NULL DEFAULT '',
			metadata         TEXT NOT NULL DEFAULT '{}',
			tags             TEXT NOT NULL DEFAULT '[]',
			importance       REAL NOT NULL DEFAULT 0,
			created_at       INTEGER NOT NULL,
			updated_at       INTEGER NOT NULL,
			last_accessed_at INTEGER NOT NULL,
			access_count     INTEGER NOT NULL DEFAULT 0,
			expires_at       INTEGER,
			superseded_by    TEXT
		)`,
		`CREATE INDEX idx_memories_namespace ON memories(namespace)`,
		`CREATE INDEX idx_memories_expires ON memories(expires_at)`,
		`CREATE VIRTUAL TABLE vec_memories USING vec0(namespace TEXT partition key, embedding float[8])`,
		`CREATE VIRTUAL TABLE fts_memories USING fts5(content, summary, tags, tokenize='porter unicode61')`,
	}
	for _, q := range schema {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed schema %q: %v", q, err)
		}
	}

	now := ms(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO memories
			(rowid, id, namespace, tier, content, created_at, updated_at, last_accessed_at)
			VALUES (?,?,?,?,?,?,?,?)`,
			r.rowID, r.id, r.namespace, string(r.tier), r.content, now, now, now); err != nil {
			t.Fatalf("seed memories row %s: %v", r.id, err)
		}
		blob, err := vec.SerializeFloat32(r.embedding)
		if err != nil {
			t.Fatalf("serialize embedding %s: %v", r.id, err)
		}
		if _, err := db.Exec(`INSERT INTO vec_memories(rowid, namespace, embedding) VALUES (?,?,?)`,
			r.rowID, r.namespace, blob); err != nil {
			t.Fatalf("seed vec row %s: %v", r.id, err)
		}
		if _, err := db.Exec(`INSERT INTO fts_memories(rowid, content, summary, tags) VALUES (?,?,'','')`,
			r.rowID, r.content); err != nil {
			t.Fatalf("seed fts row %s: %v", r.id, err)
		}
	}
}

// TestUpgradeFromLegacyDB opens a pre-fingerprint DB and checks it migrates in
// place: existing rows stay readable through every retrieval path and new
// fingerprint-based writes work on top.
func TestUpgradeFromLegacyDB(t *testing.T) {
	const dims = 8
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	seedLegacyDB(t, path, []legacyRow{
		{rowID: 1, id: "m1", namespace: "team", tier: memory.TierWorking,
			content: "the cat sat on the mat", embedding: unit(0)},
		{rowID: 2, id: "m2", namespace: "team", tier: memory.TierSemantic,
			content: "go is a programming language", embedding: unit(1)},
	})

	st, err := Open(ctx, path, dims)
	if err != nil {
		t.Fatalf("upgrade open failed: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	t.Run("PreExistingRowsReadable", func(t *testing.T) {
		for _, id := range []string{"m1", "m2"} {
			if _, err := st.Get(ctx, "team", id); err != nil {
				t.Errorf("Get(%s): %v", id, err)
			}
		}
	})

	t.Run("VectorSearchFindsLegacyRow", func(t *testing.T) {
		got, err := st.VectorSearch(ctx, "team", unit(0), store.Filter{}, 1)
		if err != nil {
			t.Fatalf("VectorSearch: %v", err)
		}
		if len(got) != 1 || got[0].Memory.ID != "m1" {
			t.Fatalf("VectorSearch = %+v, want m1", got)
		}
	})

	t.Run("KeywordSearchFindsLegacyRow", func(t *testing.T) {
		got, err := st.KeywordSearch(ctx, "team", "cat", store.Filter{}, 5)
		if err != nil {
			t.Fatalf("KeywordSearch: %v", err)
		}
		if len(got) != 1 || got[0].Memory.ID != "m1" {
			t.Fatalf("KeywordSearch = %+v, want m1", got)
		}
	})

	t.Run("NewWritesAreFingerprintDiscoverable", func(t *testing.T) {
		now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		m := &memory.Memory{
			ID: "m3", Namespace: "team", Tier: memory.TierSemantic,
			Content: "fresh durable fact", Embedding: unit(2),
			CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
		}
		if err := st.Upsert(ctx, m); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		got, err := st.GetByFingerprint(ctx, "team", memory.TierSemantic,
			memory.Fingerprint("fresh durable fact"), now)
		if err != nil || got.ID != "m3" {
			t.Fatalf("GetByFingerprint = (%v, %v), want m3", got, err)
		}
	})
}

// TestMigrateRenamesProjectMapToPins opens a DB created when the pins table
// was still named project_map and checks migrate renames it in place: the
// seeded row survives (the table is not recreated empty next to a stray),
// the old index is replaced by idx_pins_ns, and no project_map remains.
func TestMigrateRenamesProjectMapToPins(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "prerename.db")

	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	seed := []string{
		`CREATE TABLE project_map (
			key        TEXT PRIMARY KEY,
			namespace  TEXT NOT NULL,
			note       TEXT NOT NULL DEFAULT '',
			created_by TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX idx_project_map_ns ON project_map(namespace)`,
		`INSERT INTO project_map (key, namespace, note, created_by, created_at, updated_at)
		 VALUES ('remote:github.com/acme/phoenix', 'acme/phoenix', 'seeded before rename', 'kit',
			'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	st, err := Open(ctx, path, 8)
	if err != nil {
		t.Fatalf("open pre-rename DB: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	got, err := st.GetPins(ctx, []string{"remote:github.com/acme/phoenix"})
	if err != nil {
		t.Fatalf("GetPins: %v", err)
	}
	if len(got) != 1 || got[0].Namespace != "acme/phoenix" || got[0].CreatedBy != "kit" {
		t.Fatalf("GetPins = %+v, want the seeded pre-rename row", got)
	}
	if hasTable(t, st.db, "project_map") {
		t.Error("project_map still exists after migrate; want it renamed to pins")
	}
	if !hasTable(t, st.db, "pins") {
		t.Error("pins table missing after migrate")
	}
	if hasIndex(t, st.db, "idx_project_map_ns") {
		t.Error("idx_project_map_ns still exists; want it dropped in favor of idx_pins_ns")
	}
	if !hasIndex(t, st.db, "idx_pins_ns") {
		t.Error("idx_pins_ns missing after migrate")
	}
}

// TestMigrateFoldsStrayProjectMapIntoPins covers the rollback window: after
// the rename, an old binary re-creates project_map and writes pins into it.
// The next migrate folds the stray's rows into pins — the stray wins on a key
// conflict (it is the later write, the same last-write-wins rule as PutPins)
// while created_at/created_by keep the pins row's values — and drops the
// stray table, so no pin silently vanishes.
func TestMigrateFoldsStrayProjectMapIntoPins(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stray.db")
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	st, err := Open(ctx, path, 8)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.PutPins(ctx, []store.Pin{{
		Key: "remote:github.com/acme/phoenix", Namespace: "acme/phoenix",
		Note: "pre-rollback", CreatedBy: "kit", CreatedAt: created, UpdatedAt: created,
	}}); err != nil {
		t.Fatalf("PutPins: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("open stray db: %v", err)
	}
	for _, q := range []string{
		`CREATE TABLE project_map (
			key        TEXT PRIMARY KEY,
			namespace  TEXT NOT NULL,
			note       TEXT NOT NULL DEFAULT '',
			created_by TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`INSERT INTO project_map VALUES ('remote:github.com/acme/phoenix', 'acme/phoenix2',
			'rollback re-pin', 'alex', '2026-02-01T00:00:00Z', '2026-02-01T00:00:00Z')`,
		`INSERT INTO project_map VALUES ('path:/home/kit/dev/widgets', 'acme/widgets',
			'rollback new pin', 'alex', '2026-02-01T00:00:00Z', '2026-02-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed stray %q: %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close stray db: %v", err)
	}

	st2, err := Open(ctx, path, 8)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })

	got, err := st2.GetPins(ctx, []string{"remote:github.com/acme/phoenix", "path:/home/kit/dev/widgets"})
	if err != nil {
		t.Fatalf("GetPins: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("GetPins returned %d pins, want 2: %+v", len(got), got)
	}
	byKey := map[string]store.Pin{}
	for _, p := range got {
		byKey[p.Key] = p
	}
	re := byKey["remote:github.com/acme/phoenix"]
	if re.Namespace != "acme/phoenix2" || re.Note != "rollback re-pin" {
		t.Errorf("conflicting key = %+v, want the stray's later write to win", re)
	}
	if re.CreatedBy != "kit" || !re.CreatedAt.Equal(created) {
		t.Errorf("conflicting key created_at/by = %v/%q, want the pins row's provenance preserved", re.CreatedAt, re.CreatedBy)
	}
	if byKey["path:/home/kit/dev/widgets"].Namespace != "acme/widgets" {
		t.Errorf("new stray key = %+v, want folded in", byKey["path:/home/kit/dev/widgets"])
	}
	if hasTable(t, st2.db, "project_map") {
		t.Error("stray project_map still exists after migrate; want it folded and dropped")
	}
}

// TestMigrateBackfillsEveryNewerColumn guards the bug class behind the crashloop:
// a column referenced (by an index or query) before migrate ALTER-adds it on an
// existing DB. Driven by backfillColumns, so adding a column is covered for free:
// strip every backfilled column from a fresh DB, re-migrate, require them back.
func TestMigrateBackfillsEveryNewerColumn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stripped.db")

	st, err := Open(ctx, path, 8)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Drop every index over a backfilled column (so the columns can be dropped),
	// then strip every backfilled column to recreate an older schema.
	for _, idx := range indexesOverBackfilledColumns {
		if _, err := st.db.ExecContext(ctx, "DROP INDEX "+idx); err != nil {
			t.Fatalf("drop index %s: %v", idx, err)
		}
	}
	for _, v := range slices.Backward(backfillColumns) {
		col := v.name
		if _, err := st.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE memories DROP COLUMN %s", col)); err != nil {
			t.Fatalf("drop column %s: %v", col, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, err := Open(ctx, path, 8)
	if err != nil {
		t.Fatalf("re-migrate stripped DB: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })

	for _, c := range backfillColumns {
		if !hasColumn(t, st2.db, c.name) {
			t.Errorf("column %q not restored by migrate", c.name)
		}
	}
	for _, idx := range indexesOverBackfilledColumns {
		if !hasIndex(t, st2.db, idx) {
			t.Errorf("%s not restored by migrate", idx)
		}
	}
}

// indexesOverBackfilledColumns names every index built over a column that
// migrate ALTER-adds rather than creating with the table. They have to be
// dropped before their columns can be, and re-created after — which is exactly
// the ordering bug TestMigrateBackfillsEveryNewerColumn guards. Adding a
// backfilled column with an index means adding it here too.
var indexesOverBackfilledColumns = []string{
	"idx_memories_fingerprint", // fingerprint
	"idx_memories_repair",      // embed_state, embed_next_run_at
}
