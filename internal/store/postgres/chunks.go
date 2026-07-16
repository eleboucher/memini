package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// Chunked embedding on Postgres. See store.ChunkStore for why this is a
// separate capability rather than a change to VectorSearch.
//
// memory_chunks is a plain child table with ON DELETE CASCADE, which is what
// makes Delete, DeleteIfExpiredBefore, and DeleteNamespace chunk-aware for
// free. Reassign is the one write path the FK does not cover — it changes a
// memory's namespace rather than its existence — so it is handled explicitly
// below.

// writeChunks rewrites a memory's chunk rows inside the Upsert transaction. The
// DELETE always runs, so an upsert carrying no chunks clears them — see
// Memory.Chunks for why stale chunks are worse than missing ones.
func (s *Store) writeChunks(ctx context.Context, tx pgx.Tx, m *memory.Memory) error {
	if _, err := tx.Exec(ctx, `DELETE FROM memory_chunks WHERE memory_id = $1`, m.ID); err != nil {
		return fmt.Errorf("postgres: clear chunks: %w", err)
	}
	for _, c := range m.Chunks {
		if len(c.Embedding) != s.dims {
			return fmt.Errorf("postgres: chunk %d has %d dims, store expects %d", c.Idx, len(c.Embedding), s.dims)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO memory_chunks (memory_id, chunk_idx, namespace, text, embedding) VALUES ($1,$2,$3,$4,$5)`,
			m.ID, c.Idx, m.Namespace, c.Text, pgvector.NewVector(c.Embedding)); err != nil {
			return fmt.Errorf("postgres: insert chunk: %w", err)
		}
	}
	return nil
}

// chunkOverFetch multiplies k before the max-pool. The LIMIT inside the CTE
// applies to chunks, so one long memory whose every chunk matches could take
// the whole budget and starve other memories; over-fetching leaves the pool
// something to collapse.
const chunkOverFetch = 8

// chunkFilterOverFetch funds the filters. VectorSearch needs no such thing on
// this backend — its WHERE runs inside the index scan — but the chunk CTE can
// filter only on namespace (tier, expiry, and supersession live on memories,
// joined after the LIMIT has already spent the budget), so filtered-out
// candidates consume slots here exactly as they do on sqlite, and the same 4x
// covers them. The two multipliers compose: collapse and filtering both bite
// out of one fixed budget.
const chunkFilterOverFetch = 4

// ChunkVectorSearch implements store.ChunkStore.
//
// MIN(distance) per memory is the max-pool (nearer = smaller distance). Pooling
// happens before the join so the distance-to-score conversion is the same one
// VectorSearch uses, which keeps both legs' scores in one space — recall's gates
// are absolute thresholds, so a chunk score that meant something different from
// a document score would silently mis-gate.
func (s *Store) ChunkVectorSearch(ctx context.Context, namespace string, vec []float32, f store.Filter, k int) ([]store.Scored, error) {
	if len(vec) != s.dims {
		return nil, fmt.Errorf("postgres: query vector has %d dims, store expects %d", len(vec), s.dims)
	}
	if k <= 0 {
		return nil, nil
	}
	b := &args{}
	qv := b.add(pgvector.NewVector(vec))
	ns := b.add(namespace)
	poolK := b.add(k * chunkOverFetch * chunkFilterOverFetch)
	// Aliased: memory_chunks carries namespace and embedding under the same
	// names as memories, so an unqualified filter column would be ambiguous.
	where := filterClauseOn(b, f, "m")
	q := fmt.Sprintf(`
		WITH cand AS (
			SELECT c.memory_id, c.text, c.embedding <-> %s AS distance
			FROM memory_chunks c
			WHERE c.namespace = %s
			ORDER BY c.embedding <-> %s
			LIMIT %s
		), pooled AS (
			-- DISTINCT ON keeps the row that produced the minimum, so the text
			-- carried out is the chunk that actually won rather than an arbitrary
			-- one of the memory's chunks. That is the point: the reranker judges it.
			SELECT DISTINCT ON (memory_id) memory_id, text, distance
			FROM cand ORDER BY memory_id, distance
		)
		SELECT %s, p.distance, p.text
		FROM pooled p JOIN memories m ON m.id = p.memory_id
		WHERE m.namespace = %s%s
		ORDER BY p.distance
		LIMIT %s`,
		qv, ns, qv, poolK,
		prefixed(memoryColumns, "m"), ns, where, b.add(k))

	// Identical to VectorSearch's conversion, deliberately: the two legs are
	// merged by score, and recall gates on absolute values.
	return s.queryScoredChunk(ctx, q, b.vals, func(d float64) float64 { return 1 / (1 + d) })
}

// ListUnchunked implements store.ChunkStore. char_length counts characters, not
// bytes, matching what internal/chunk bounds on.
func (s *Store) ListUnchunked(ctx context.Context, namespace string, minRunes int, afterID string, limit int) ([]*memory.Memory, error) {
	if limit <= 0 {
		return nil, nil
	}
	b := &args{}
	q := `SELECT ` + prefixed(memoryColumns, "m") + ` FROM memories m
		WHERE char_length(m.content) > ` + b.add(minRunes) + `
		  AND NOT EXISTS (SELECT 1 FROM memory_chunks c WHERE c.memory_id = m.id)`
	if namespace != "" {
		q += ` AND m.namespace = ` + b.add(namespace)
	}
	if afterID != "" {
		q += ` AND m.id > ` + b.add(afterID)
	}
	q += ` ORDER BY m.id LIMIT ` + b.add(limit)

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

// CountUnchunked implements store.ChunkStore: ListUnchunked's queue in full,
// where the list shows one batch.
func (s *Store) CountUnchunked(ctx context.Context, namespace string, minRunes int) (int, error) {
	b := &args{}
	q := `SELECT count(*) FROM memories m
		WHERE char_length(m.content) > ` + b.add(minRunes) + `
		  AND NOT EXISTS (SELECT 1 FROM memory_chunks c WHERE c.memory_id = m.id)`
	if namespace != "" {
		q += ` AND m.namespace = ` + b.add(namespace)
	}
	var n int
	err := s.pool.QueryRow(ctx, q, b.vals...).Scan(&n)
	return n, err
}

// PutChunks implements store.ChunkStore. FOR UPDATE holds the memories row
// while the chunks are written, so the updated_at guard and the write are
// atomic — the check-then-act window a service-level re-read cannot close.
func (s *Store) PutChunks(ctx context.Context, namespace, id string, updatedAt time.Time, chunks []memory.Chunk) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var stored time.Time
	err = tx.QueryRow(ctx,
		`SELECT updated_at FROM memories WHERE namespace=$1 AND id=$2 FOR UPDATE`,
		namespace, id).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !stored.Equal(updatedAt) {
		return false, nil
	}
	if err := s.writeChunks(ctx, tx, &memory.Memory{ID: id, Namespace: namespace, Chunks: chunks}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// CountChunks implements store.ChunkStore. The FK cascade means orphans cannot
// exist here, so a plain count is the whole truth.
func (s *Store) CountChunks(ctx context.Context, namespace string) (int, error) {
	b := &args{}
	q := `SELECT count(*) FROM memory_chunks`
	if namespace != "" {
		q += ` WHERE namespace = ` + b.add(namespace)
	}
	var n int
	err := s.pool.QueryRow(ctx, q, b.vals...).Scan(&n)
	return n, err
}
