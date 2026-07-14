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
	// Drop the only index over a backfilled column (so the column can be dropped),
	// then strip every backfilled column to recreate an older schema.
	if _, err := st.db.ExecContext(ctx, "DROP INDEX idx_memories_fingerprint"); err != nil {
		t.Fatalf("drop index: %v", err)
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
	if !hasIndex(t, st2.db, "idx_memories_fingerprint") {
		t.Error("idx_memories_fingerprint not restored by migrate")
	}
}
