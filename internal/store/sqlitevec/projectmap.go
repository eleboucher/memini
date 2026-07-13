package sqlitevec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/store"
)

var _ store.ProjectMapStore = (*Store)(nil)

// PutProjectMapEntries upserts entries in a single transaction, keyed by each
// entry's Key. An update preserves the existing row's created_at/created_by
// — looked up inside the same transaction so a concurrent Put for the same
// key cannot race between the read and the write, mirroring PutAPIKey's
// lookup-then-upsert pattern — while namespace/note/updated_at take the
// incoming values.
func (s *Store) PutProjectMapEntries(ctx context.Context, entries []store.ProjectMapEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	for _, e := range entries {
		created := e.CreatedAt
		if created.IsZero() {
			created = now
		}
		updated := e.UpdatedAt
		if updated.IsZero() {
			updated = now
		}
		createdBy := e.CreatedBy

		var existingCreated, existingCreatedBy string
		err := tx.QueryRowContext(ctx,
			`SELECT created_at, created_by FROM project_map WHERE key=?`, e.Key).Scan(&existingCreated, &existingCreatedBy)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// Insert path: use the incoming created/created_by as-is.
		case err != nil:
			return fmt.Errorf("sqlitevec: lookup project map entry %q: %w", e.Key, err)
		default:
			// Update path: the row's provenance is fixed at creation, so the
			// incoming CreatedAt/CreatedBy (whatever the caller passed) is
			// discarded in favor of what is already stored.
			created, err = time.Parse(time.RFC3339Nano, existingCreated)
			if err != nil {
				return fmt.Errorf("sqlitevec: parse existing project map created_at for %q: %w", e.Key, err)
			}
			createdBy = existingCreatedBy
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO project_map (key, namespace, note, created_by, created_at, updated_at)
			 VALUES (?,?,?,?,?,?)
			 ON CONFLICT(key) DO UPDATE SET
				namespace=excluded.namespace, note=excluded.note, updated_at=excluded.updated_at`,
			e.Key, e.Namespace, e.Note, createdBy, created.Format(time.RFC3339Nano), updated.Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("sqlitevec: put project map entry %q: %w", e.Key, err)
		}
	}
	return tx.Commit()
}

// GetProjectMapEntries returns the entries matching the given keys, in no
// particular order; a key with no matching row is simply absent.
func (s *Store) GetProjectMapEntries(ctx context.Context, keys []string) ([]store.ProjectMapEntry, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	q := `SELECT key, namespace, note, created_by, created_at, updated_at FROM project_map WHERE key IN (` +
		placeholders(len(keys)) + `)`
	rows, err := s.db.QueryContext(ctx, q, keysToArgs(keys)...)
	if err != nil {
		return nil, fmt.Errorf("sqlitevec: get project map entries: %w", err)
	}
	return scanProjectMapEntries(rows)
}

// DeleteProjectMapEntries removes the entries with the given keys and returns
// the number of rows actually deleted.
func (s *Store) DeleteProjectMapEntries(ctx context.Context, keys []string) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	q := `DELETE FROM project_map WHERE key IN (` + placeholders(len(keys)) + `)`
	res, err := s.db.ExecContext(ctx, q, keysToArgs(keys)...)
	if err != nil {
		return 0, fmt.Errorf("sqlitevec: delete project map entries: %w", err)
	}
	return res.RowsAffected()
}

// ListProjectMapEntries returns every entry ordered by Key.
func (s *Store) ListProjectMapEntries(ctx context.Context) ([]store.ProjectMapEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, namespace, note, created_by, created_at, updated_at FROM project_map ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("sqlitevec: list project map entries: %w", err)
	}
	return scanProjectMapEntries(rows)
}

// RenameProjectMapNamespaces rewrites every entry whose namespace exactly
// equals from to to instead; a namespace that merely starts with from is
// untouched.
func (s *Store) RenameProjectMapNamespaces(ctx context.Context, from, to string) error {
	if from == to {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE project_map SET namespace=? WHERE namespace=?`, to, from)
	if err != nil {
		return fmt.Errorf("sqlitevec: rename project map namespaces: %w", err)
	}
	return nil
}

// placeholders returns "?,?,...,?" (n placeholders), for an IN clause built
// from a caller-supplied key slice.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// keysToArgs converts keys to []any for QueryContext/ExecContext varargs.
func keysToArgs(keys []string) []any {
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}
	return args
}

// scanProjectMapEntries reads every remaining row of rows into
// ProjectMapEntries, closing rows before returning.
func scanProjectMapEntries(rows *sql.Rows) ([]store.ProjectMapEntry, error) {
	defer func() { _ = rows.Close() }()
	var out []store.ProjectMapEntry
	for rows.Next() {
		var e store.ProjectMapEntry
		var created, updated string
		if err := rows.Scan(&e.Key, &e.Namespace, &e.Note, &e.CreatedBy, &created, &updated); err != nil {
			return nil, err
		}
		ct, err := time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, fmt.Errorf("sqlitevec: parse project map created_at: %w", err)
		}
		ut, err := time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, fmt.Errorf("sqlitevec: parse project map updated_at: %w", err)
		}
		e.CreatedAt = ct.UTC()
		e.UpdatedAt = ut.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}
