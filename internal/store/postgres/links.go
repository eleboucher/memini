package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/eleboucher/memini/internal/store"
)

var _ store.LinkStore = (*Store)(nil)

// PutNamespaceLink creates or updates a read-only namespace link. Idempotent:
// a second call for the same (namespace, target) overwrites tiers.
func (s *Store) PutNamespaceLink(ctx context.Context, l store.NamespaceLink) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO namespace_links (namespace, target, tiers, created_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (namespace, target) DO UPDATE SET tiers=excluded.tiers`,
		l.Namespace, l.Target, l.Tiers, l.CreatedAt)
	if err != nil {
		return fmt.Errorf("postgres: put namespace link: %w", err)
	}
	return nil
}

// DeleteNamespaceLink removes a link, or returns store.ErrNotFound when absent.
func (s *Store) DeleteNamespaceLink(ctx context.Context, namespace, target string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM namespace_links WHERE namespace=$1 AND target=$2`, namespace, target)
	if err != nil {
		return fmt.Errorf("postgres: delete namespace link: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ListNamespaceLinks returns namespace's outgoing links, ordered by target.
func (s *Store) ListNamespaceLinks(ctx context.Context, namespace string) ([]store.NamespaceLink, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT namespace, target, tiers, created_at FROM namespace_links WHERE namespace=$1 ORDER BY target`,
		namespace)
	if err != nil {
		return nil, fmt.Errorf("postgres: list namespace links: %w", err)
	}
	return scanNamespaceLinks(rows)
}

// ListAllNamespaceLinks returns every link across every namespace, ordered by
// namespace then target.
func (s *Store) ListAllNamespaceLinks(ctx context.Context) ([]store.NamespaceLink, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT namespace, target, tiers, created_at FROM namespace_links ORDER BY namespace, target`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list all namespace links: %w", err)
	}
	return scanNamespaceLinks(rows)
}

func scanNamespaceLinks(rows pgx.Rows) ([]store.NamespaceLink, error) {
	defer rows.Close()
	var out []store.NamespaceLink
	for rows.Next() {
		var l store.NamespaceLink
		if err := rows.Scan(&l.Namespace, &l.Target, &l.Tiers, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
