package sqlitevec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	valid_from, valid_to, confidence`

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
			confidence       REAL
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
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("sqlitevec: migrate: %w\nstatement: %s", err, q)
		}
	}
	// Backfill the temporal-validity columns on stores created before they
	// existed (the CREATE TABLE above is a no-op once the table is present).
	for _, col := range []string{"valid_from", "valid_to"} {
		if err := s.addColumnIfMissing(ctx, "memories", col, "INTEGER"); err != nil {
			return err
		}
	}
	if err := s.addColumnIfMissing(ctx, "memories", "confidence", "REAL"); err != nil {
		return err
	}
	return nil
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
func (s *Store) Upsert(ctx context.Context, m *memory.Memory) error {
	if len(m.Embedding) != s.dims {
		return fmt.Errorf("sqlitevec: embedding has %d dims, store expects %d", len(m.Embedding), s.dims)
	}
	metaJSON, err := json.Marshal(orEmptyMap(m.Metadata))
	if err != nil {
		return fmt.Errorf("sqlitevec: marshal metadata: %w", err)
	}
	tagsJSON, err := json.Marshal(orEmptySlice(m.Tags))
	if err != nil {
		return fmt.Errorf("sqlitevec: marshal tags: %w", err)
	}
	vec, err := sqlitevec.SerializeFloat32(m.Embedding)
	if err != nil {
		return fmt.Errorf("sqlitevec: serialize embedding: %w", err)
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
			 valid_from, valid_to, confidence)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			m.ID, m.Namespace, string(m.Tier), m.Content, m.Summary, string(metaJSON), string(tagsJSON),
			m.Importance, ms(m.CreatedAt), ms(m.UpdatedAt), ms(m.LastAccessedAt), m.AccessCount,
			msPtr(m.ExpiresAt), strPtr(m.SupersededBy), msPtr(m.ValidFrom), msPtr(m.ValidTo), f64Ptr(m.Confidence))
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
		if _, uerr := tx.ExecContext(ctx, `UPDATE memories SET
			tier=?, content=?, summary=?, metadata=?, tags=?, importance=?,
			created_at=?, updated_at=?, last_accessed_at=?, access_count=?, expires_at=?, superseded_by=?,
			valid_from=?, valid_to=?, confidence=?
			WHERE rowid=?`,
			string(m.Tier), m.Content, m.Summary, string(metaJSON), string(tagsJSON),
			m.Importance, ms(m.CreatedAt), ms(m.UpdatedAt), ms(m.LastAccessedAt), m.AccessCount,
			msPtr(m.ExpiresAt), strPtr(m.SupersededBy), msPtr(m.ValidFrom), msPtr(m.ValidTo), f64Ptr(m.Confidence), rowID); uerr != nil {
			return fmt.Errorf("sqlitevec: update memory: %w", uerr)
		}
	}

	// Rewrite the vector and FTS rows keyed by the stable rowid.
	if _, err := tx.ExecContext(ctx, `DELETE FROM vec_memories WHERE rowid=?`, rowID); err != nil {
		return fmt.Errorf("sqlitevec: clear vector: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO vec_memories(rowid, namespace, embedding) VALUES (?,?,?)`,
		rowID, m.Namespace, vec); err != nil {
		return fmt.Errorf("sqlitevec: insert vector: %w", err)
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
	s.metrics.Upsert(op, string(m.Tier))
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

// VectorSearch returns the k nearest live memories to vec in the namespace.
func (s *Store) VectorSearch(ctx context.Context, namespace string, vec []float32, f store.Filter, k int) ([]store.Scored, error) {
	if len(vec) != s.dims {
		return nil, fmt.Errorf("sqlitevec: query vector has %d dims, store expects %d", len(vec), s.dims)
	}
	blob, err := sqlitevec.SerializeFloat32(vec)
	if err != nil {
		return nil, err
	}
	where, args := filterClause(f, "m")
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
	where, args := filterClause(f, "m")
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

// List returns memories in a namespace matching f (without embeddings).
func (s *Store) List(ctx context.Context, namespace string, f store.Filter, limit int) ([]*memory.Memory, error) {
	where, args := filterClause(f, "m")
	q := `SELECT ` + memoryColumns + ` FROM memories m WHERE m.namespace = ?` + where
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

// DeleteNamespace removes every memory in a namespace, including vector and FTS
// index entries. Returns the number of memories deleted.
func (s *Store) DeleteNamespace(ctx context.Context, namespace string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	rowIDs, err := collectRowIDs(tx, ctx, namespace)
	if err != nil {
		return 0, err
	}
	if len(rowIDs) == 0 {
		return 0, nil
	}

	for _, q := range []string{
		`DELETE FROM vec_memories WHERE rowid IN (SELECT rowid FROM memories WHERE namespace=?)`,
		`DELETE FROM fts_memories WHERE rowid IN (SELECT rowid FROM memories WHERE namespace=?)`,
		`DELETE FROM memories WHERE namespace=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, namespace); err != nil {
			return 0, err
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
// collides in toNS.
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
			`SELECT m.rowid, v.embedding FROM memories m JOIN vec_memories v ON v.rowid = m.rowid
			 WHERE m.id = ? AND m.namespace = ?`, id, fromNS).Scan(&rowID, &emb)
		if errors.Is(err, sql.ErrNoRows) {
			continue // not in the source namespace; skip
		}
		if err != nil {
			return moved, fmt.Errorf("sqlitevec: reassign lookup %q: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memories SET namespace=? WHERE rowid=?`, toNS, rowID); err != nil {
			return moved, fmt.Errorf("sqlitevec: reassign memory: %w", err)
		}
		// vec_memories.namespace is a partition key; rewrite the row under it.
		if _, err := tx.ExecContext(ctx, `DELETE FROM vec_memories WHERE rowid=?`, rowID); err != nil {
			return moved, fmt.Errorf("sqlitevec: reassign clear vector: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO vec_memories(rowid, namespace, embedding) VALUES (?,?,?)`, rowID, toNS, emb); err != nil {
			return moved, fmt.Errorf("sqlitevec: reassign insert vector: %w", err)
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
func (s *Store) SetConfidence(ctx context.Context, namespace, id string, confidence float64, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE memories SET confidence=?, updated_at=? WHERE id=? AND namespace=?`,
		confidence, ms(now), id, namespace)
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
