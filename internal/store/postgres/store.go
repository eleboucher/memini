package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

const memoryColumns = `id, namespace, tier, content, summary, metadata, tags, importance,
	created_at, updated_at, last_accessed_at, access_count, expires_at, superseded_by`

// Store is a Postgres/VectorChord backed store.Store.
type Store struct {
	pool *pgxpool.Pool
	dims int
}

var _ store.Store = (*Store)(nil)

// Open connects to Postgres, ensures the schema exists for the given embedding
// dimensionality, and returns a ready Store.
func Open(ctx context.Context, dsn string, dims int) (*Store, error) {
	if dims <= 0 {
		return nil, fmt.Errorf("postgres: dims must be positive, got %d", dims)
	}

	// Migrate on a single connection first: CREATE EXTENSION must run before the
	// pgvector type can be registered on pooled connections.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := migrate(ctx, conn, dims); err != nil {
		_ = conn.Close(ctx)
		return nil, err
	}
	_ = conn.Close(ctx)

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, c)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: pool: %w", err)
	}
	return &Store{pool: pool, dims: dims}, nil
}

func migrate(ctx context.Context, conn *pgx.Conn, dims int) error {
	stmts := []string{
		`CREATE EXTENSION IF NOT EXISTS vchord CASCADE`,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS memories (
			id               text PRIMARY KEY,
			namespace        text NOT NULL,
			tier             text NOT NULL,
			content          text NOT NULL,
			summary          text NOT NULL DEFAULT '',
			metadata         jsonb NOT NULL DEFAULT '{}',
			tags             text[] NOT NULL DEFAULT '{}',
			importance       double precision NOT NULL DEFAULT 0,
			created_at       timestamptz NOT NULL,
			updated_at       timestamptz NOT NULL,
			last_accessed_at timestamptz NOT NULL,
			access_count     integer NOT NULL DEFAULT 0,
			expires_at       timestamptz,
			superseded_by    text,
			embedding        vector(%d) NOT NULL,
			fts              tsvector GENERATED ALWAYS AS (
				to_tsvector('english',
					content || ' ' || summary || ' ' || array_to_string(tags, ' '))
			) STORED
		)`, dims),
		`CREATE INDEX IF NOT EXISTS idx_memories_namespace ON memories(namespace)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_expires ON memories(expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_fts ON memories USING gin(fts)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_vec ON memories USING vchordrq (embedding vector_l2_ops)`,
	}
	for _, q := range stmts {
		if _, err := conn.Exec(ctx, q); err != nil {
			return fmt.Errorf("postgres: migrate: %w\nstatement: %s", err, q)
		}
	}
	return nil
}

// Upsert inserts or replaces a memory.
func (s *Store) Upsert(ctx context.Context, m *memory.Memory) error {
	if len(m.Embedding) != s.dims {
		return fmt.Errorf("postgres: embedding has %d dims, store expects %d", len(m.Embedding), s.dims)
	}
	metaJSON, err := json.Marshal(orEmptyMap(m.Metadata))
	if err != nil {
		return fmt.Errorf("postgres: marshal metadata: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO memories
			(id, namespace, tier, content, summary, metadata, tags, importance,
			 created_at, updated_at, last_accessed_at, access_count, expires_at, superseded_by, embedding)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (id) DO UPDATE SET
			namespace=EXCLUDED.namespace, tier=EXCLUDED.tier, content=EXCLUDED.content,
			summary=EXCLUDED.summary, metadata=EXCLUDED.metadata, tags=EXCLUDED.tags,
			importance=EXCLUDED.importance, created_at=EXCLUDED.created_at, updated_at=EXCLUDED.updated_at,
			last_accessed_at=EXCLUDED.last_accessed_at, access_count=EXCLUDED.access_count,
			expires_at=EXCLUDED.expires_at, superseded_by=EXCLUDED.superseded_by, embedding=EXCLUDED.embedding`,
		m.ID, m.Namespace, string(m.Tier), m.Content, m.Summary, metaJSON, orEmptySlice(m.Tags),
		m.Importance, m.CreatedAt, m.UpdatedAt, m.LastAccessedAt, m.AccessCount,
		m.ExpiresAt, m.SupersededBy, pgvector.NewVector(m.Embedding))
	if err != nil {
		return fmt.Errorf("postgres: upsert: %w", err)
	}
	return nil
}

// Get returns a memory by ID.
func (s *Store) Get(ctx context.Context, namespace, id string) (*memory.Memory, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+memoryColumns+` FROM memories WHERE id=$1 AND namespace=$2`, id, namespace)
	m, err := scanMemory(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return m, err
}

// Delete removes a memory by ID.
func (s *Store) Delete(ctx context.Context, namespace, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM memories WHERE id=$1 AND namespace=$2`, id, namespace)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// SetSuperseded records that a memory was replaced by supersededBy.
func (s *Store) SetSuperseded(ctx context.Context, namespace, id, supersededBy string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE memories SET superseded_by=$1 WHERE id=$2 AND namespace=$3`, supersededBy, id, namespace)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// Reinforce bumps access_count/last_accessed_at and optionally slides the TTL.
func (s *Store) Reinforce(ctx context.Context, namespace string, ids []string, accessedAt time.Time, newExpiry *time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	b := &args{}
	set := "access_count = access_count + 1, last_accessed_at = " + b.add(accessedAt)
	if newExpiry != nil {
		// Only slide rows that already expire (short-term); never add a TTL to durable rows.
		set += ", expires_at = CASE WHEN expires_at IS NOT NULL THEN " + b.add(*newExpiry) + " ELSE expires_at END"
	}
	ns := b.add(namespace)
	idsParam := b.add(ids)
	q := fmt.Sprintf("UPDATE memories SET %s WHERE namespace = %s AND id = ANY(%s)", set, ns, idsParam)
	_, err := s.pool.Exec(ctx, q, b.vals...)
	return err
}

// VectorSearch returns the k nearest live memories to vec in the namespace.
func (s *Store) VectorSearch(ctx context.Context, namespace string, vec []float32, f store.Filter, k int) ([]store.Scored, error) {
	if len(vec) != s.dims {
		return nil, fmt.Errorf("postgres: query vector has %d dims, store expects %d", len(vec), s.dims)
	}
	b := &args{}
	qv := b.add(pgvector.NewVector(vec))
	ns := b.add(namespace)
	where := filterClause(b, f)
	q := fmt.Sprintf(`
		SELECT %s, embedding <-> %s AS distance
		FROM memories
		WHERE namespace = %s%s
		ORDER BY embedding <-> %s
		LIMIT %s`,
		memoryColumns, qv, ns, where, qv, b.add(k))
	return s.queryScored(ctx, q, b.vals, func(d float64) float64 { return 1 / (1 + d) })
}

// KeywordSearch returns the k best full-text matches in the namespace.
func (s *Store) KeywordSearch(ctx context.Context, namespace, query string, f store.Filter, k int) ([]store.Scored, error) {
	// OR the terms (term1 | term2 | ...) so recall matches rows containing ANY
	// term, rather than plainto_tsquery's all-terms AND.
	tsq := tsQuery(query)
	if tsq == "" {
		return nil, nil
	}
	b := &args{}
	qq := b.add(tsq)
	ns := b.add(namespace)
	where := filterClause(b, f)
	q := fmt.Sprintf(`
		SELECT %s, ts_rank(fts, to_tsquery('english', %s)) AS rank
		FROM memories
		WHERE fts @@ to_tsquery('english', %s) AND namespace = %s%s
		ORDER BY rank DESC
		LIMIT %s`,
		memoryColumns, qq, qq, ns, where, b.add(k))
	return s.queryScored(ctx, q, b.vals, func(r float64) float64 { return r })
}

// ListExpired returns up to limit memories whose TTL has passed.
func (s *Store) ListExpired(ctx context.Context, now time.Time, limit int) ([]*memory.Memory, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+memoryColumns+` FROM memories WHERE expires_at IS NOT NULL AND expires_at <= $1 LIMIT $2`,
		now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
	b := &args{}
	ns := b.add(namespace)
	where := filterClause(b, f)
	q := fmt.Sprintf(`SELECT %s FROM memories WHERE namespace = %s%s`, memoryColumns, ns, where)
	if limit > 0 {
		q += " LIMIT " + b.add(limit)
	}
	rows, err := s.pool.Query(ctx, q, b.vals...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT namespace FROM memories ORDER BY namespace`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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

// Ping verifies the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Close releases the connection pool.
func (s *Store) Close() error { s.pool.Close(); return nil }

func (s *Store) queryScored(ctx context.Context, q string, vals []any, score func(float64) float64) ([]store.Scored, error) {
	rows, err := s.pool.Query(ctx, q, vals...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
