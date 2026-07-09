package sqlitevec

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/eleboucher/memini/internal/store"
)

var _ store.LinkStore = (*Store)(nil)

// PutNamespaceLink creates or updates a read-only namespace link. Idempotent:
// a second call for the same (namespace, target) overwrites tiers.
func (s *Store) PutNamespaceLink(ctx context.Context, l store.NamespaceLink) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO namespace_links (namespace, target, tiers, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(namespace, target) DO UPDATE SET tiers=excluded.tiers`,
		l.Namespace, l.Target, l.Tiers, ms(l.CreatedAt))
	if err != nil {
		return fmt.Errorf("sqlitevec: put namespace link: %w", err)
	}
	return nil
}

// DeleteNamespaceLink removes a link, or returns store.ErrNotFound when absent.
func (s *Store) DeleteNamespaceLink(ctx context.Context, namespace, target string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM namespace_links WHERE namespace=? AND target=?`, namespace, target)
	if err != nil {
		return fmt.Errorf("sqlitevec: delete namespace link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlitevec: delete namespace link: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ListNamespaceLinks returns namespace's outgoing links, ordered by target.
func (s *Store) ListNamespaceLinks(ctx context.Context, namespace string) ([]store.NamespaceLink, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT namespace, target, tiers, created_at FROM namespace_links WHERE namespace=? ORDER BY target`,
		namespace)
	if err != nil {
		return nil, fmt.Errorf("sqlitevec: list namespace links: %w", err)
	}
	return scanNamespaceLinks(rows)
}

// ListAllNamespaceLinks returns every link across every namespace, ordered by
// namespace then target.
func (s *Store) ListAllNamespaceLinks(ctx context.Context) ([]store.NamespaceLink, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT namespace, target, tiers, created_at FROM namespace_links ORDER BY namespace, target`)
	if err != nil {
		return nil, fmt.Errorf("sqlitevec: list all namespace links: %w", err)
	}
	return scanNamespaceLinks(rows)
}

func scanNamespaceLinks(rows *sql.Rows) ([]store.NamespaceLink, error) {
	defer func() { _ = rows.Close() }()
	var out []store.NamespaceLink
	for rows.Next() {
		var l store.NamespaceLink
		var createdAt int64
		if err := rows.Scan(&l.Namespace, &l.Target, &l.Tiers, &createdAt); err != nil {
			return nil, err
		}
		l.CreatedAt = fromMs(createdAt)
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
