package sqlitevec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/ncruces"
	_ "github.com/ncruces/go-sqlite3/driver" // registers the database/sql "sqlite3" driver

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// overFetch multiplies k for vector search so post-filtering (expiry,
// supersession, tier) still leaves roughly k live results.
const overFetch = 4

// memoryColumns is the canonical column order for scanning a memory row.
const memoryColumns = `id, namespace, tier, content, summary, metadata, tags, importance,
	created_at, updated_at, last_accessed_at, access_count, expires_at, superseded_by,
	valid_from, valid_to, confidence, level, linked_memory_ids`

// Store is a sqlite-vec backed store.Store.
type Store struct {
	db      *sql.DB
	dims    int
	metrics store.Metrics
}

var _ store.Store = (*Store)(nil)

// SetMetrics installs an observability sink. Passing nil disables metrics.
func (s *Store) SetMetrics(m store.Metrics) {
	if m == nil {
		s.metrics = store.NopMetrics()
		return
	}
	s.metrics = m
}

// Open opens (creating if needed) the sqlite database at path and ensures the
// schema exists for the given embedding dimensionality.
func Open(ctx context.Context, path string, dims int) (*Store, error) {
	if dims <= 0 {
		return nil, fmt.Errorf("sqlitevec: dims must be positive, got %d", dims)
	}
	// _txlock=immediate makes BeginTx take the write lock up front. The store's
	// transactions are all writers; a deferred tx that upgraded to a write under
	// concurrency would get SQLITE_BUSY immediately (busy_timeout does not apply
	// to lock upgrades in WAL mode) and fail instead of waiting.
	dsn := fmt.Sprintf("file:%s?_txlock=immediate"+
		"&_pragma=busy_timeout(10000)&_pragma=journal_mode(wal)&_pragma=foreign_keys(on)", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlitevec: open: %w", err)
	}
	s := &Store{db: db, dims: dims, metrics: store.NopMetrics()}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// backfillColumns are columns added after the original memories schema. On an
// existing DB the CREATE TABLE in migrate is a no-op, so each is ALTER-added
// here before any index or query references it.
var backfillColumns = []struct{ name, decl string }{
	{"valid_from", "INTEGER"},
	{"valid_to", "INTEGER"},
	{"confidence", "REAL"},
	{"fingerprint", "TEXT NOT NULL DEFAULT ''"},
	{"level", "TEXT NOT NULL DEFAULT ''"},
	{"linked_memory_ids", "TEXT NOT NULL DEFAULT '[]'"},
}

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memories (
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
			superseded_by    TEXT,
			valid_from       INTEGER,
			valid_to         INTEGER,
			confidence       REAL,
			fingerprint      TEXT NOT NULL DEFAULT '',
			level            TEXT NOT NULL DEFAULT '',
			linked_memory_ids TEXT NOT NULL DEFAULT '[]'
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_namespace ON memories(namespace)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_expires ON memories(expires_at)`,
		// namespace is a partition key so KNN can isolate tenants efficiently.
		fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS vec_memories USING vec0(
			namespace TEXT partition key,
			embedding float[%d]
		)`, s.dims),
		// Porter stemming so queries match morphological variants (move/moved/moving).
		`CREATE VIRTUAL TABLE IF NOT EXISTS fts_memories USING fts5(content, summary, tags, tokenize='porter unicode61')`,
		// Key/value store for store-level metadata (e.g. the embedding model the
		// vectors were produced with — see EmbedModel/SetEmbedModel).
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		// namespace_links records cross-namespace read links (namespace-cascade
		// design, see store.LinkStore); tiers is a JSON array of memory.Tier
		// strings, created_at an RFC3339 string.
		`CREATE TABLE IF NOT EXISTS namespace_links (
			src_ns     TEXT NOT NULL,
			dst_ns     TEXT NOT NULL,
			tiers      TEXT NOT NULL DEFAULT '[]',
			note       TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			PRIMARY KEY (src_ns, dst_ns)
		)`,
		// api_keys records API credentials (see store.APIKeyStore): key_hash is
		// the hex SHA-256 of the secret (the secret itself is never stored) and
		// is UNIQUE so GetAPIKeyByHash's auth-path lookup can rely on an index;
		// created_at is an RFC3339 string, as with namespace_links above.
		// default_ns is also ALTER-added below for databases created before it.
		`CREATE TABLE IF NOT EXISTS api_keys (
			name       TEXT PRIMARY KEY,
			key_hash   TEXT NOT NULL UNIQUE,
			home_ns    TEXT NOT NULL DEFAULT '',
			default_ns TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			disabled   INTEGER NOT NULL DEFAULT 0,
			admin      INTEGER NOT NULL DEFAULT 0
		)`,
		// memory_events is the activity log (see store.EventLogStore): one row
		// per (operation, memory), rows of one operation sharing op_id. The
		// memory_* columns are a snapshot, not a join — they keep the feed a
		// single query and keep a forget event readable after its memory is
		// gone. Timestamps are unix millis, as in memories above.
		`CREATE TABLE IF NOT EXISTS memory_events (
			id             INTEGER PRIMARY KEY,
			op_id          TEXT NOT NULL,
			kind           TEXT NOT NULL,
			namespace      TEXT NOT NULL,
			query          TEXT NOT NULL DEFAULT '',
			memory_id      TEXT NOT NULL DEFAULT '',
			memory_ns      TEXT NOT NULL DEFAULT '',
			memory_tier    TEXT NOT NULL DEFAULT '',
			memory_summary TEXT NOT NULL DEFAULT '',
			rank           INTEGER NOT NULL DEFAULT 0,
			score          REAL,
			detail         TEXT NOT NULL DEFAULT '{}',
			created_at     INTEGER NOT NULL
		)`,
		// The read path is always newest-first, optionally narrowed by namespace;
		// the (created_at DESC, id DESC) tail matches ListEvents' ordering so the
		// keyset cursor walks the index.
		`CREATE INDEX IF NOT EXISTS idx_memory_events_ns_time ON memory_events(namespace, created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_events_time ON memory_events(created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_events_memory ON memory_events(memory_id)`,
		// project_map records project→namespace pins (see store.ProjectMapStore,
		// the config-handshake redesign): key is "remote:<canonical-remote>" or
		// "path:<absolute-toplevel>"; created_at/updated_at are RFC3339 strings,
		// as with api_keys/namespace_links above.
		`CREATE TABLE IF NOT EXISTS project_map (
			key        TEXT PRIMARY KEY,
			namespace  TEXT NOT NULL,
			note       TEXT NOT NULL DEFAULT '',
			created_by TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_project_map_ns ON project_map(namespace)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("sqlitevec: migrate: %w\nstatement: %s", err, q)
		}
	}
	if err := s.verifyVecDims(ctx); err != nil {
		return err
	}
	for _, c := range backfillColumns {
		if err := s.addColumnIfMissing(ctx, "memories", c.name, c.decl); err != nil {
			return err
		}
	}
	// api_keys.default_ns was added after the table first shipped; ALTER-add
	// it so databases created before it migrate in place.
	if err := s.addColumnIfMissing(ctx, "api_keys", "default_ns", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// api_keys.settings holds the per-key store.ClientSettings override
	// (config-handshake redesign) as a JSON object; '{}' means no override.
	if err := s.addColumnIfMissing(ctx, "api_keys", "settings", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}
	// api_keys.admin holds the per-key admin capability (admin-keys redesign);
	// see store.APIKey.Admin's doc.
	if err := s.addColumnIfMissing(ctx, "api_keys", "admin", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// After the backfill: on an old DB the fingerprint column exists only now.
	if _, err := s.db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_memories_fingerprint ON memories(namespace, tier, fingerprint)`); err != nil {
		return fmt.Errorf("sqlitevec: migrate: create idx_memories_fingerprint: %w", err)
	}
	return nil
}

// verifyVecDims fails Open when an existing vec_memories table has a different
// embedding width than the configured dims. CREATE VIRTUAL TABLE IF NOT EXISTS
// is a no-op on an existing table, so otherwise a dims change would only
// surface as opaque sqlite-vec mismatch errors on every Upsert.
func (s *Store) verifyVecDims(ctx context.Context) error {
	var ddl string
	err := s.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='vec_memories'`).Scan(&ddl)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // table absent (should not happen post-migrate)
	}
	if err != nil {
		return fmt.Errorf("sqlitevec: inspect vec_memories: %w", err)
	}
	got, err := parseVecDims(ddl)
	if err != nil {
		return err
	}
	if got != s.dims {
		return fmt.Errorf("sqlitevec: store was created with %d embedding dims but is configured for %d; "+
			"set MEMINI_EMBED_DIMS=%d to match the existing data, or migrate to a new database", got, s.dims, got)
	}
	return nil
}

// parseVecDims extracts N from a vec0 "embedding float[N]" column declaration.
func parseVecDims(ddl string) (int, error) {
	_, after, ok := strings.Cut(ddl, "float[")
	if !ok {
		return 0, fmt.Errorf("sqlitevec: cannot find embedding dimension in vec_memories schema")
	}
	inside, _, ok := strings.Cut(after, "]")
	if !ok {
		return 0, fmt.Errorf("sqlitevec: malformed vec_memories schema (no closing ] for float[)")
	}
	n, err := strconv.Atoi(strings.TrimSpace(inside))
	if err != nil {
		return 0, fmt.Errorf("sqlitevec: parse vec_memories dimension: %w", err)
	}
	return n, nil
}

// addColumnIfMissing adds column to table if it is not already present, so
// schema additions are safe to run against an existing database.
func (s *Store) addColumnIfMissing(ctx context.Context, table, column, decl string) error {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("sqlitevec: inspect %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl))
	if err != nil {
		return fmt.Errorf("sqlitevec: add column %s.%s: %w", table, column, err)
	}
	return nil
}

// Upsert inserts or replaces a memory and its vector/keyword index entries.
// When m.Embedding is empty the row is stored with no vec_memories entry
// (keyword index still written) — the write path used when embedding
// generation is unavailable; any other length must equal the store's dims.
func (s *Store) Upsert(ctx context.Context, m *memory.Memory) error {
	if len(m.Embedding) != 0 && len(m.Embedding) != s.dims {
		return fmt.Errorf("sqlitevec: embedding has %d dims, store expects %d", len(m.Embedding), s.dims)
	}
	metaJSON, err := json.Marshal(store.OrEmptyMap(m.Metadata))
	if err != nil {
		return fmt.Errorf("sqlitevec: marshal metadata: %w", err)
	}
	tagsJSON, err := json.Marshal(store.OrEmptySlice(m.Tags))
	if err != nil {
		return fmt.Errorf("sqlitevec: marshal tags: %w", err)
	}
	linkedIDs := m.LinkedMemoryIDs
	if linkedIDs == nil {
		linkedIDs = []string{}
	}
	linkedJSON, err := json.Marshal(linkedIDs)
	if err != nil {
		return fmt.Errorf("sqlitevec: marshal linked memory ids: %w", err)
	}
	var vec []byte
	if len(m.Embedding) != 0 {
		vec, err = sqlitevec.SerializeFloat32(m.Embedding)
		if err != nil {
			return fmt.Errorf("sqlitevec: serialize embedding: %w", err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var rowID int64
	var existingNS string
	var op string
	err = tx.QueryRowContext(ctx, `SELECT rowid, namespace FROM memories WHERE id = ?`, m.ID).Scan(&rowID, &existingNS)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		op = "insert"
		res, ierr := tx.ExecContext(ctx, `INSERT INTO memories
			(id, namespace, tier, content, summary, metadata, tags, importance,
			 created_at, updated_at, last_accessed_at, access_count, expires_at, superseded_by,
			 valid_from, valid_to, confidence, fingerprint, level, linked_memory_ids)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			m.ID, m.Namespace, string(m.Tier), m.Content, m.Summary, string(metaJSON), string(tagsJSON),
			m.Importance, ms(m.CreatedAt), ms(m.UpdatedAt), ms(m.LastAccessedAt), m.AccessCount,
			msPtr(m.ExpiresAt), strPtr(m.SupersededBy), msPtr(m.ValidFrom), msPtr(m.ValidTo), f64Ptr(m.Confidence),
			memory.Fingerprint(m.Content), string(m.Level), string(linkedJSON))
		if ierr != nil {
			return fmt.Errorf("sqlitevec: insert memory: %w", ierr)
		}
		if rowID, ierr = res.LastInsertId(); ierr != nil {
			return ierr
		}
	case err != nil:
		return fmt.Errorf("sqlitevec: lookup memory: %w", err)
	default:
		op = "update"
		if existingNS != m.Namespace {
			return fmt.Errorf("sqlitevec: id %q exists in namespace %q: %w", m.ID, existingNS, store.ErrConflict)
		}
		// created_at is intentionally absent: it is immutable after insert.
		// Including it let an update-by-ID (CreatedAt=now) corrupt recency
		// ranking and GetByFingerprint's created_at DESC ordering.
		if _, uerr := tx.ExecContext(ctx, `UPDATE memories SET
			tier=?, content=?, summary=?, metadata=?, tags=?, importance=?,
			updated_at=?, last_accessed_at=?, access_count=?, expires_at=?, superseded_by=?,
			valid_from=?, valid_to=?, confidence=?, fingerprint=?, level=?, linked_memory_ids=?
			WHERE rowid=?`,
			string(m.Tier), m.Content, m.Summary, string(metaJSON), string(tagsJSON),
			m.Importance, ms(m.UpdatedAt), ms(m.LastAccessedAt), m.AccessCount,
			msPtr(m.ExpiresAt), strPtr(m.SupersededBy), msPtr(m.ValidFrom), msPtr(m.ValidTo), f64Ptr(m.Confidence),
			memory.Fingerprint(m.Content), string(m.Level), string(linkedJSON), rowID); uerr != nil {
			return fmt.Errorf("sqlitevec: update memory: %w", uerr)
		}
	}

	// Rewrite the vector and FTS rows keyed by the stable rowid. The DELETE
	// always runs (even when the new embedding is empty) so a re-upsert that
	// drops the vector removes the stale vec_memories row instead of leaving
	// it orphaned and reachable by an unrelated future VectorSearch.
	if _, err := tx.ExecContext(ctx, `DELETE FROM vec_memories WHERE rowid=?`, rowID); err != nil {
		return fmt.Errorf("sqlitevec: clear vector: %w", err)
	}
	if len(m.Embedding) != 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO vec_memories(rowid, namespace, embedding) VALUES (?,?,?)`,
			rowID, m.Namespace, vec); err != nil {
			return fmt.Errorf("sqlitevec: insert vector: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM fts_memories WHERE rowid=?`, rowID); err != nil {
		return fmt.Errorf("sqlitevec: clear fts: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO fts_memories(rowid, content, summary, tags) VALUES (?,?,?,?)`,
		rowID, m.Content, m.Summary, strings.Join(m.Tags, " ")); err != nil {
		return fmt.Errorf("sqlitevec: insert fts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.metrics.Upsert(op, string(m.Tier), store.MemoryTypeLabel(m))
	return nil
}

// Reinforce bumps access_count/last_accessed_at and optionally slides the TTL.
func (s *Store) Reinforce(ctx context.Context, namespace string, ids []string, accessedAt time.Time, newExpiry *time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	set := "access_count = access_count + 1, last_accessed_at = ?"
	args := []any{ms(accessedAt)}
	if newExpiry != nil {
		// Only slide rows that already expire (short-term); never add a TTL to durable rows.
		set += ", expires_at = CASE WHEN expires_at IS NOT NULL THEN ? ELSE expires_at END"
		args = append(args, ms(*newExpiry))
	}
	args = append(args, namespace)
	placeholders := make([]string, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	q := fmt.Sprintf("UPDATE memories SET %s WHERE namespace = ? AND id IN (%s)",
		set, strings.Join(placeholders, ","))
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// Get returns a memory by ID.
func (s *Store) Get(ctx context.Context, namespace, id string) (*memory.Memory, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+memoryColumns+` FROM memories WHERE id=? AND namespace=?`, id, namespace)
	m, err := scanMemory(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return m, err
}

// PredecessorIDs returns the IDs of memories in the namespace superseded by id.
func (s *Store) PredecessorIDs(ctx context.Context, namespace, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM memories WHERE namespace=? AND superseded_by=?`, namespace, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var x string
		if err := rows.Scan(&x); err != nil {
			return nil, err
		}
		ids = append(ids, x)
	}
	return ids, rows.Err()
}

// GetByFingerprint returns the most recent live memory in namespace+tier whose
// content fingerprint matches. Superseded, expired, and validity-closed
// (contradicted) rows are excluded so a dead duplicate never absorbs a fresh
// write — re-asserting a contradicted fact must store a live row, not
// corroborate the invalidated one.
func (s *Store) GetByFingerprint(
	ctx context.Context, namespace string, tier memory.Tier, fingerprint string, now time.Time,
) (*memory.Memory, error) {
	if fingerprint == "" {
		return nil, store.ErrNotFound
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+memoryColumns+` FROM memories
		 WHERE namespace=? AND tier=? AND fingerprint=? AND superseded_by IS NULL
		   AND (expires_at IS NULL OR expires_at > ?)
		   AND (valid_to IS NULL OR valid_to > ?)
		 ORDER BY created_at DESC LIMIT 1`,
		namespace, string(tier), fingerprint, ms(now), ms(now))
	m, err := scanMemory(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return m, err
}

// Delete removes a memory and its index entries.
func (s *Store) Delete(ctx context.Context, namespace, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var rowID int64
	err = tx.QueryRowContext(ctx, `SELECT rowid FROM memories WHERE id=? AND namespace=?`, id, namespace).Scan(&rowID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	} else if err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM memories WHERE rowid=?`,
		`DELETE FROM vec_memories WHERE rowid=?`,
		`DELETE FROM fts_memories WHERE rowid=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, rowID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.metrics.Delete()
	return nil
}

// DeleteIfExpiredBefore removes a memory only if its expiry is still at or
// before cutoff. Returns ErrNotFound when the memory is absent or its TTL was
// slid past cutoff by Reinforce since the last ListExpired call.
func (s *Store) DeleteIfExpiredBefore(ctx context.Context, namespace, id string, cutoff time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var rowID int64
	var tier string
	err = tx.QueryRowContext(ctx,
		`SELECT rowid, tier FROM memories WHERE id=? AND namespace=? AND expires_at IS NOT NULL AND expires_at <= ?`,
		id, namespace, ms(cutoff)).Scan(&rowID, &tier)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	} else if err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM memories WHERE rowid=?`,
		`DELETE FROM vec_memories WHERE rowid=?`,
		`DELETE FROM fts_memories WHERE rowid=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, rowID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.metrics.SweepExpired(tier)
	return nil
}

// SetSuperseded records that a memory was replaced by supersededBy.
func (s *Store) SetSuperseded(ctx context.Context, namespace, id, supersededBy string) error {
	// Stamp valid_to at the moment of supersession (unless already set), so a
	// time-filtered recall can still surface the fact for the window it held.
	res, err := s.db.ExecContext(ctx,
		`UPDATE memories SET superseded_by=?, valid_to=COALESCE(valid_to, ?) WHERE id=? AND namespace=?`,
		supersededBy, ms(time.Now().UTC()), id, namespace)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	s.metrics.SoftDelete()
	return nil
}

// Restore clears superseded_by/valid_to so a tombstoned memory is live again.
func (s *Store) Restore(ctx context.Context, namespace, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE memories SET superseded_by=NULL, valid_to=NULL WHERE id=? AND namespace=?`,
		id, namespace)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// VectorSearch returns the k nearest live memories to vec in the namespace.
func (s *Store) VectorSearch(ctx context.Context, namespace string, vec []float32, f store.Filter, k int) ([]store.Scored, error) {
	if len(vec) != s.dims {
		return nil, fmt.Errorf("sqlitevec: query vector has %d dims, store expects %d", len(vec), s.dims)
	}
	blob, err := sqlitevec.SerializeFloat32(vec)
	if err != nil {
		return nil, err
	}
	where, args := filterClause(f)
	q := fmt.Sprintf(`
		SELECT %s, v.distance
		FROM vec_memories v
		JOIN memories m ON m.rowid = v.rowid
		WHERE v.namespace = ? AND v.embedding MATCH ? AND k = ?%s
		ORDER BY v.distance
		LIMIT ?`, prefixed(memoryColumns, "m"), where)

	callArgs := append([]any{namespace, blob, k * overFetch}, args...)
	callArgs = append(callArgs, k)
	return s.queryScored(ctx, q, callArgs, distanceToScore)
}

// KeywordSearch returns the k best BM25 full-text matches in the namespace.
func (s *Store) KeywordSearch(ctx context.Context, namespace, query string, f store.Filter, k int) ([]store.Scored, error) {
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	where, args := filterClause(f)
	q := fmt.Sprintf(`
		SELECT %s, bm25(fts_memories) AS rank
		FROM fts_memories
		JOIN memories m ON m.rowid = fts_memories.rowid
		WHERE fts_memories MATCH ? AND m.namespace = ?%s
		ORDER BY rank
		LIMIT ?`, prefixed(memoryColumns, "m"), where)

	callArgs := append([]any{match, namespace}, args...)
	callArgs = append(callArgs, k)
	// bm25 is lower-is-better, so negate it for a higher-is-better score.
	return s.queryScored(ctx, q, callArgs, func(rank float64) float64 { return -rank })
}

// ListExpired returns up to limit memories whose TTL has passed.
func (s *Store) ListExpired(ctx context.Context, now time.Time, limit int) ([]*memory.Memory, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+memoryColumns+` FROM memories WHERE expires_at IS NOT NULL AND expires_at <= ? LIMIT ?`,
		ms(now), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// List returns memories in a namespace matching f (without embeddings),
// ordered by f.Sort (newest-created first by default).
func (s *Store) List(ctx context.Context, namespace string, f store.Filter, limit int) ([]*memory.Memory, error) {
	where, args := filterClause(f)
	q := `SELECT ` + memoryColumns + ` FROM memories m WHERE m.namespace = ?` + where + orderClause(f.Sort)
	callArgs := append([]any{namespace}, args...)
	if limit > 0 {
		q += " LIMIT ?"
		callArgs = append(callArgs, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, callArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListNamespaces returns the distinct namespaces holding memories.
func (s *Store) ListNamespaces(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT namespace FROM memories ORDER BY namespace`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return nil, err
		}
		out = append(out, ns)
	}
	return out, rows.Err()
}

// NamespaceActivity implements store.ActivityStore: one aggregate query for
// per-namespace live count and most recent created_at. Liveness reuses
// filterClause with an empty Filter so it stays byte-identical to what a
// default List applies (not expired at now, not superseded, validity window
// not closed).
func (s *Store) NamespaceActivity(ctx context.Context, now time.Time) ([]store.NamespaceActivity, error) {
	where, args := filterClause(store.Filter{Now: now})
	q := `SELECT m.namespace, COUNT(*), MAX(m.created_at) FROM memories m WHERE 1=1` +
		where + ` GROUP BY m.namespace ORDER BY m.namespace`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []store.NamespaceActivity
	for rows.Next() {
		var a store.NamespaceActivity
		var total, last int64
		if err := rows.Scan(&a.NS, &total, &last); err != nil {
			return nil, err
		}
		a.Total = int(total)
		a.LastWrite = fromMs(last)
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteNamespace removes every memory in a namespace, including vector and
// FTS index entries, plus any namespace_links row that references the
// namespace on either side (gap G5: a deleted namespace must not leave a
// dangling link). Returns the number of memories deleted.
func (s *Store) DeleteNamespace(ctx context.Context, namespace string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Cascade the link table first: this must happen even when the namespace
	// holds no memories (a namespace can exist purely as a link endpoint).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM namespace_links WHERE src_ns=? OR dst_ns=?`, namespace, namespace); err != nil {
		return 0, fmt.Errorf("sqlitevec: delete namespace: cascade links: %w", err)
	}

	rowIDs, err := collectRowIDs(tx, ctx, namespace)
	if err != nil {
		return 0, err
	}
	if len(rowIDs) > 0 {
		for _, q := range []string{
			`DELETE FROM vec_memories WHERE rowid IN (SELECT rowid FROM memories WHERE namespace=?)`,
			`DELETE FROM fts_memories WHERE rowid IN (SELECT rowid FROM memories WHERE namespace=?)`,
			`DELETE FROM memories WHERE namespace=?`,
		} {
			if _, err := tx.ExecContext(ctx, q, namespace); err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	for range rowIDs {
		s.metrics.Delete()
	}
	return int64(len(rowIDs)), nil
}

// Reassign moves memories from fromNS to toNS, updating the namespace column
// and rewriting the vec0 partition row (the FTS row carries no namespace). IDs
// absent from fromNS are skipped; IDs are globally unique so a move never
// collides in toNS. The lookup is a LEFT JOIN (not an inner join) because a
// vectorless memory (see Upsert) has no vec_memories row at all — an inner
// join would silently skip it as "not found" instead of moving it.
func (s *Store) Reassign(ctx context.Context, fromNS string, ids []string, toNS string) (int64, error) {
	if len(ids) == 0 || fromNS == toNS {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var moved int64
	for _, id := range ids {
		var rowID int64
		var emb []byte
		err := tx.QueryRowContext(ctx,
			`SELECT m.rowid, v.embedding FROM memories m LEFT JOIN vec_memories v ON v.rowid = m.rowid
			 WHERE m.id = ? AND m.namespace = ?`, id, fromNS).Scan(&rowID, &emb)
		if errors.Is(err, sql.ErrNoRows) {
			continue // not in the source namespace; skip
		}
		if err != nil {
			return 0, fmt.Errorf("sqlitevec: reassign lookup %q: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memories SET namespace=? WHERE rowid=?`, toNS, rowID); err != nil {
			return 0, fmt.Errorf("sqlitevec: reassign memory: %w", err)
		}
		// vec_memories.namespace is a partition key; rewrite the row under it.
		if _, err := tx.ExecContext(ctx, `DELETE FROM vec_memories WHERE rowid=?`, rowID); err != nil {
			return 0, fmt.Errorf("sqlitevec: reassign clear vector: %w", err)
		}
		if emb != nil {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO vec_memories(rowid, namespace, embedding) VALUES (?,?,?)`, rowID, toNS, emb); err != nil {
				return 0, fmt.Errorf("sqlitevec: reassign insert vector: %w", err)
			}
		}
		moved++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return moved, nil
}

// Retier changes a memory's tier and expiry in place. Tier and expiry live only
// in the memories row, so no vector/FTS reindex is required.
func (s *Store) Retier(ctx context.Context, namespace, id string, tier memory.Tier, expiresAt *time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE memories SET tier=?, expires_at=? WHERE id=? AND namespace=?`,
		string(tier), msPtr(expiresAt), id, namespace)
	if err != nil {
		return fmt.Errorf("sqlitevec: retier: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// SetConfidence updates a memory's confidence and bumps updated_at to now.
// Confidence lives only in the memories row, so no vector/FTS reindex is needed.
// Validity-closed rows are skipped (ErrNotFound): corroboration must never
// regrow an invalidated fact, even when MarkContradicted lands between the
// caller's read and this write.
func (s *Store) SetConfidence(ctx context.Context, namespace, id string, confidence float64, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE memories SET confidence=?, updated_at=?
		 WHERE id=? AND namespace=? AND (valid_to IS NULL OR valid_to > ?)`,
		confidence, ms(now), id, namespace, ms(now))
	if err != nil {
		return fmt.Errorf("sqlitevec: set confidence: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// MarkContradicted invalidates a durable fact a newer write contradicts. The
// SET expressions read the pre-update confidence column (SQLite evaluates the
// right-hand side against the old row), snapshotting it into metadata for
// audit and reversal before overwriting it.
func (s *Store) MarkContradicted(ctx context.Context, namespace, id, contradictedBy string, confidence float64, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE memories SET
			metadata=json_set(json_set(metadata, '$.contradicted_by', ?), '$.contradicted_prev_confidence', confidence),
			confidence=?,
			valid_to=COALESCE(valid_to, ?),
			updated_at=?
		 WHERE id=? AND namespace=?`,
		contradictedBy, confidence, ms(now), ms(now), id, namespace)
	if err != nil {
		return fmt.Errorf("sqlitevec: mark contradicted: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func collectRowIDs(tx *sql.Tx, ctx context.Context, namespace string) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT rowid FROM memories WHERE namespace=?`, namespace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

const metaEmbedModel = "embed_model"

// EmbedModel returns the recorded embedding model name, or "" if none was set.
func (s *Store) EmbedModel(ctx context.Context) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, metaEmbedModel).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("sqlitevec: read embed model: %w", err)
	}
	return v, nil
}

// SetEmbedModel records the embedding model the stored vectors were produced with.
func (s *Store) SetEmbedModel(ctx context.Context, model string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, metaEmbedModel, model)
	if err != nil {
		return fmt.Errorf("sqlitevec: set embed model: %w", err)
	}
	return nil
}

var _ store.ClientSettingsStore = (*Store)(nil)

const metaClientSettingsDefaults = "client_settings_defaults"

// GlobalClientSettings returns the stored global default ClientSettings, or
// the zero value (every field nil) if none has been set yet.
func (s *Store) GlobalClientSettings(ctx context.Context) (store.ClientSettings, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, metaClientSettingsDefaults).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ClientSettings{}, nil
	}
	if err != nil {
		return store.ClientSettings{}, fmt.Errorf("sqlitevec: read global client settings: %w", err)
	}
	var cs store.ClientSettings
	// Tolerant decode: an unknown field from a newer writer is ignored
	// (json.Unmarshal's default behavior) — strict validation is the REST
	// boundary's job, not the store's.
	if err := json.Unmarshal([]byte(v), &cs); err != nil {
		return store.ClientSettings{}, fmt.Errorf("sqlitevec: unmarshal global client settings: %w", err)
	}
	return cs, nil
}

// SetGlobalClientSettings replaces the stored global default ClientSettings
// wholesale (not a merge): only fields set on s are persisted, since nil
// pointer fields with `omitempty` marshal to nothing.
func (s *Store) SetGlobalClientSettings(ctx context.Context, cs store.ClientSettings) error {
	b, err := json.Marshal(cs)
	if err != nil {
		return fmt.Errorf("sqlitevec: marshal global client settings: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, metaClientSettingsDefaults, string(b))
	if err != nil {
		return fmt.Errorf("sqlitevec: set global client settings: %w", err)
	}
	return nil
}

var _ store.LinkStore = (*Store)(nil)

// PutLink inserts or replaces the link keyed by (l.Src, l.Dst).
//
// Unlike memory Put (created_at is immutable after insert, see the comment
// on its INSERT above), an upsert here overwrites created_at. This is
// intentional, not an oversight: links carry no recency semantics that a
// stable created_at would protect, and import restore relies on the
// overwrite being conditional on l.CreatedAt being non-zero (below) so it
// can replay a link's original creation time instead of stamping "now".
func (s *Store) PutLink(ctx context.Context, l store.NamespaceLink) error {
	tiersJSON, err := json.Marshal(tiersOrEmpty(l.Tiers))
	if err != nil {
		return fmt.Errorf("sqlitevec: marshal link tiers: %w", err)
	}
	created := l.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO namespace_links (src_ns, dst_ns, tiers, note, created_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(src_ns,dst_ns) DO UPDATE SET
			tiers=excluded.tiers, note=excluded.note, created_at=excluded.created_at`,
		l.Src, l.Dst, string(tiersJSON), l.Note, created.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("sqlitevec: put link: %w", err)
	}
	return nil
}

// DeleteLink removes the link from src to dst. The bool reports whether a
// link existed to delete.
func (s *Store) DeleteLink(ctx context.Context, src, dst string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM namespace_links WHERE src_ns=? AND dst_ns=?`, src, dst)
	if err != nil {
		return false, fmt.Errorf("sqlitevec: delete link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListLinks returns the links whose Src is src, ordered by Dst.
func (s *Store) ListLinks(ctx context.Context, src string) ([]store.NamespaceLink, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT src_ns, dst_ns, tiers, note, created_at FROM namespace_links WHERE src_ns=? ORDER BY dst_ns`, src)
	if err != nil {
		return nil, fmt.Errorf("sqlitevec: list links: %w", err)
	}
	return scanLinks(rows)
}

// ListAllLinks returns every link in the store, ordered by Src then Dst.
func (s *Store) ListAllLinks(ctx context.Context) ([]store.NamespaceLink, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT src_ns, dst_ns, tiers, note, created_at FROM namespace_links ORDER BY src_ns, dst_ns`)
	if err != nil {
		return nil, fmt.Errorf("sqlitevec: list all links: %w", err)
	}
	return scanLinks(rows)
}

// RenameLinkEndpoints rewrites every link whose src_ns or dst_ns equals from
// to to instead. When a rewritten link collides with a pre-existing row at
// its new key, the pre-existing row is kept and the renamed link dropped
// (ON CONFLICT DO NOTHING): the target namespace's own explicit grant wins
// over an inherited one, so a rename can never silently widen or narrow tier
// access the target had already configured. The SELECT is ordered by
// (src_ns, dst_ns) so which renamed link survives a multi-way collision
// (e.g. the reciprocal pair link(from,to)+link(to,from) collapsing onto
// (to,to)) is deterministic: the first row in key order wins. A no-op when
// from == to.
func (s *Store) RenameLinkEndpoints(ctx context.Context, from, to string) error {
	if from == to {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT src_ns, dst_ns, tiers, note, created_at FROM namespace_links
		 WHERE src_ns=? OR dst_ns=? ORDER BY src_ns, dst_ns`, from, from)
	if err != nil {
		return fmt.Errorf("sqlitevec: rename link endpoints: select: %w", err)
	}
	type linkRow struct{ src, dst, tiers, note, created string }
	var toRename []linkRow
	for rows.Next() {
		var r linkRow
		if err := rows.Scan(&r.src, &r.dst, &r.tiers, &r.note, &r.created); err != nil {
			_ = rows.Close()
			return err
		}
		toRename = append(toRename, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	for _, r := range toRename {
		newSrc, newDst := r.src, r.dst
		if newSrc == from {
			newSrc = to
		}
		if newDst == from {
			newDst = to
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM namespace_links WHERE src_ns=? AND dst_ns=?`, r.src, r.dst); err != nil {
			return fmt.Errorf("sqlitevec: rename link endpoints: delete: %w", err)
		}
		// DO NOTHING, not DO UPDATE: a pre-existing link at the new key is the
		// target namespace's own configuration and must survive untouched.
		if _, err := tx.ExecContext(ctx, `INSERT INTO namespace_links (src_ns, dst_ns, tiers, note, created_at)
			VALUES (?,?,?,?,?)
			ON CONFLICT(src_ns,dst_ns) DO NOTHING`,
			newSrc, newDst, r.tiers, r.note, r.created); err != nil {
			return fmt.Errorf("sqlitevec: rename link endpoints: insert: %w", err)
		}
	}
	return tx.Commit()
}

var _ store.APIKeyStore = (*Store)(nil)

// PutAPIKey inserts or replaces the key keyed by k.Name.
//
// Unlike PutLink (which deliberately overwrites created_at on every upsert,
// since links carry no recency semantics — see its doc above), this upsert
// preserves the existing row's created_at when k.CreatedAt is the zero
// value: API keys are long-lived identity, and rotating a key's hash or
// home namespace must not reset "when was this key first created". A
// non-zero k.CreatedAt (e.g. import restore replaying an original
// timestamp) still overwrites it. The lookup-then-upsert runs in a
// transaction so a concurrent PutAPIKey for the same name cannot race
// between the read of the existing created_at and the write.
func (s *Store) PutAPIKey(ctx context.Context, k store.APIKey) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	created := k.CreatedAt
	if created.IsZero() {
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT created_at FROM api_keys WHERE name=?`, k.Name).Scan(&existing)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			created = time.Now().UTC()
		case err != nil:
			return fmt.Errorf("sqlitevec: lookup api key: %w", err)
		default:
			created, err = time.Parse(time.RFC3339Nano, existing)
			if err != nil {
				return fmt.Errorf("sqlitevec: parse existing api key created_at: %w", err)
			}
		}
	}
	settingsJSON, err := json.Marshal(k.Settings)
	if err != nil {
		return fmt.Errorf("sqlitevec: marshal api key settings: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO api_keys (name, key_hash, home_ns, default_ns, created_at, disabled, settings, admin)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET
			key_hash=excluded.key_hash, home_ns=excluded.home_ns, default_ns=excluded.default_ns,
			created_at=excluded.created_at, disabled=excluded.disabled, settings=excluded.settings, admin=excluded.admin`,
		k.Name, k.Hash, k.HomeNS, k.DefaultNS, created.Format(time.RFC3339Nano), boolToInt(k.Disabled), string(settingsJSON), boolToInt(k.Admin))
	if err != nil {
		return fmt.Errorf("sqlitevec: put api key: %w", err)
	}
	return tx.Commit()
}

// DeleteAPIKey removes the key by name. The bool reports whether a key
// existed to delete.
func (s *Store) DeleteAPIKey(ctx context.Context, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE name=?`, name)
	if err != nil {
		return false, fmt.Errorf("sqlitevec: delete api key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListAPIKeys returns every key ordered by name.
func (s *Store) ListAPIKeys(ctx context.Context) ([]store.APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, key_hash, home_ns, default_ns, created_at, disabled, settings, admin FROM api_keys ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("sqlitevec: list api keys: %w", err)
	}
	return scanAPIKeys(rows)
}

// GetAPIKeyByHash returns the key whose hash matches, or nil, nil when none
// does.
func (s *Store) GetAPIKeyByHash(ctx context.Context, hash string) (*store.APIKey, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT name, key_hash, home_ns, default_ns, created_at, disabled, settings, admin FROM api_keys WHERE key_hash=?`, hash)
	k, err := scanAPIKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlitevec: get api key by hash: %w", err)
	}
	return &k, nil
}

// RenameAPIKeyNamespaces rewrites every key whose home_ns or default_ns
// equals from to to instead — both columns in one statement, so a namespace
// move (maintenance.Move, alongside RenameLinkEndpoints) leaves neither
// binding dangling. Unlike RenameLinkEndpoints there is no collision
// handling: neither column is part of a key's identity, so a plain UPDATE
// suffices. A no-op when from == to.
func (s *Store) RenameAPIKeyNamespaces(ctx context.Context, from, to string) error {
	if from == to {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET
			home_ns    = CASE WHEN home_ns    = ? THEN ? ELSE home_ns END,
			default_ns = CASE WHEN default_ns = ? THEN ? ELSE default_ns END
		WHERE home_ns = ? OR default_ns = ?`,
		from, to, from, to, from, from)
	if err != nil {
		return fmt.Errorf("sqlitevec: rename api key namespaces: %w", err)
	}
	return nil
}

// Ping verifies the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// queryScored runs a query whose final selected column is a numeric metric and
// returns scored memories, best-first. score converts the raw metric to a
// higher-is-better score.
func (s *Store) queryScored(ctx context.Context, q string, args []any, score func(float64) float64) ([]store.Scored, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []store.Scored
	for rows.Next() {
		var metric float64
		m, err := scanMemoryWith(rows, &metric)
		if err != nil {
			return nil, err
		}
		out = append(out, store.Scored{Memory: m, Score: score(metric)})
	}
	return out, rows.Err()
}
