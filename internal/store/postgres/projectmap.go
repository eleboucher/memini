package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

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

		var existingCreated time.Time
		var existingCreatedBy string
		err := tx.QueryRow(ctx,
			`SELECT created_at, created_by FROM project_map WHERE key=$1`, e.Key).Scan(&existingCreated, &existingCreatedBy)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// Insert path: use the incoming created/created_by as-is.
		case err != nil:
			return fmt.Errorf("postgres: lookup project map entry %q: %w", e.Key, err)
		default:
			// Update path: the row's provenance is fixed at creation, so the
			// incoming CreatedAt/CreatedBy (whatever the caller passed) is
			// discarded in favor of what is already stored.
			created = existingCreated
			createdBy = existingCreatedBy
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO project_map (key, namespace, note, created_by, created_at, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6)
			 ON CONFLICT (key) DO UPDATE SET
				namespace=EXCLUDED.namespace, note=EXCLUDED.note, updated_at=EXCLUDED.updated_at`,
			e.Key, e.Namespace, e.Note, createdBy, created, updated,
		); err != nil {
			return fmt.Errorf("postgres: put project map entry %q: %w", e.Key, err)
		}
	}
	return tx.Commit(ctx)
}

// GetProjectMapEntries returns the entries matching the given keys, in no
// particular order; a key with no matching row is simply absent.
func (s *Store) GetProjectMapEntries(ctx context.Context, keys []string) ([]store.ProjectMapEntry, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT key, namespace, note, created_by, created_at, updated_at FROM project_map WHERE key = ANY($1)`, keys)
	if err != nil {
		return nil, fmt.Errorf("postgres: get project map entries: %w", err)
	}
	return scanProjectMapEntries(rows)
}

// DeleteProjectMapEntries removes the entries with the given keys and returns
// the number of rows actually deleted.
func (s *Store) DeleteProjectMapEntries(ctx context.Context, keys []string) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM project_map WHERE key = ANY($1)`, keys)
	if err != nil {
		return 0, fmt.Errorf("postgres: delete project map entries: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ListProjectMapEntries returns every entry ordered by Key.
func (s *Store) ListProjectMapEntries(ctx context.Context) ([]store.ProjectMapEntry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT key, namespace, note, created_by, created_at, updated_at FROM project_map ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list project map entries: %w", err)
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
	_, err := s.pool.Exec(ctx, `UPDATE project_map SET namespace=$2 WHERE namespace=$1`, from, to)
	if err != nil {
		return fmt.Errorf("postgres: rename project map namespaces: %w", err)
	}
	return nil
}

// scanProjectMapEntries reads every remaining row of rs into
// ProjectMapEntries, closing rs before returning.
func scanProjectMapEntries(rs rows) ([]store.ProjectMapEntry, error) {
	defer rs.Close()
	var out []store.ProjectMapEntry
	for rs.Next() {
		var e store.ProjectMapEntry
		if err := rs.Scan(&e.Key, &e.Namespace, &e.Note, &e.CreatedBy, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		e.CreatedAt = e.CreatedAt.UTC()
		e.UpdatedAt = e.UpdatedAt.UTC()
		out = append(out, e)
	}
	return out, rs.Err()
}
