package sqlitevec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/ncruces"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// Chunked embedding on sqlite-vec.
//
// vec0's only key is its rowid, and it offers exactly three query plans —
// point, KNN, and full scan. There is no "filter by metadata column" plan, so a
// memory_rowid stored as a vec0 metadata column could only be deleted by
// scanning every chunk in the database. The vectors therefore live in
// vec_chunks keyed by nothing but rowid, and a plain table maps that rowid to
// its memory. That mirrors how vec_memories already leans on `memories`, except
// the mapping is 1:many so it cannot share the rowid.
//
// Deletes go through the rowid-subquery form already used for namespace
// deletion, which keeps vec0 on its point plan.

// chunkVecTable is the vec0 virtual table holding chunk vectors. Its DDL lives
// with every other table's in migrate (store.go); this names it for the queries
// below so a rename cannot miss one.
const chunkVecTable = "vec_chunks"

// writeChunks rewrites a memory's chunk rows inside the Upsert transaction.
// The DELETE always runs, so an upsert carrying no chunks clears whatever the
// row had — Memory.Chunks' documented contract, and the reason a content change
// that forgets to recompute leaves nothing stale behind.
func (s *Store) writeChunks(ctx context.Context, tx *sql.Tx, memRowID int64, namespace string, chunks []memory.Chunk) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM `+chunkVecTable+` WHERE rowid IN (SELECT rowid FROM memory_chunks WHERE memory_rowid=?)`,
		memRowID); err != nil {
		return fmt.Errorf("sqlitevec: clear chunk vectors: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_chunks WHERE memory_rowid=?`, memRowID); err != nil {
		return fmt.Errorf("sqlitevec: clear chunk rows: %w", err)
	}
	for _, c := range chunks {
		if len(c.Embedding) != s.dims {
			return fmt.Errorf("sqlitevec: chunk %d has %d dims, store expects %d", c.Idx, len(c.Embedding), s.dims)
		}
		blob, err := sqlitevec.SerializeFloat32(c.Embedding)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO memory_chunks(memory_rowid, chunk_idx, text) VALUES (?,?,?)`,
			memRowID, c.Idx, c.Text)
		if err != nil {
			return fmt.Errorf("sqlitevec: insert chunk row: %w", err)
		}
		chunkRowID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+chunkVecTable+`(rowid, namespace, embedding) VALUES (?,?,?)`,
			chunkRowID, namespace, blob); err != nil {
			return fmt.Errorf("sqlitevec: insert chunk vector: %w", err)
		}
	}
	return nil
}

// deleteChunksFor removes the chunk rows and vectors of the given memory
// rowids. Called from every path that removes or moves a memory.
func deleteChunksFor(ctx context.Context, tx *sql.Tx, memRowIDs []int64) error {
	for _, id := range memRowIDs {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM `+chunkVecTable+` WHERE rowid IN (SELECT rowid FROM memory_chunks WHERE memory_rowid=?)`,
			id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_chunks WHERE memory_rowid=?`, id); err != nil {
			return err
		}
	}
	return nil
}

// reassignChunks moves a memory's chunk vectors to a new namespace partition,
// preserving each chunk's rowid so its memory_chunks mapping stays valid.
func reassignChunks(ctx context.Context, tx *sql.Tx, memRowID int64, toNS string) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT c.rowid, v.embedding FROM memory_chunks c JOIN `+chunkVecTable+` v ON v.rowid = c.rowid
		 WHERE c.memory_rowid = ?`, memRowID)
	if err != nil {
		return fmt.Errorf("sqlitevec: reassign read chunks: %w", err)
	}
	type row struct {
		id  int64
		emb []byte
	}
	var found []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.emb); err != nil {
			_ = rows.Close()
			return err
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	for _, r := range found {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+chunkVecTable+` WHERE rowid=?`, r.id); err != nil {
			return fmt.Errorf("sqlitevec: reassign clear chunk vector: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+chunkVecTable+`(rowid, namespace, embedding) VALUES (?,?,?)`,
			r.id, toNS, r.emb); err != nil {
			return fmt.Errorf("sqlitevec: reassign insert chunk vector: %w", err)
		}
	}
	return nil
}

// chunkOverFetch multiplies k for the chunk KNN. vec0's KNN is a fixed-k
// operator with no notion of "k distinct memories", so a single long memory
// whose chunks all match can otherwise take every slot and starve the rest.
// Over-fetching gives the max-pool something to collapse. It stacks on the
// overFetch that already covers post-filtering.
const chunkOverFetch = 8

// ChunkVectorSearch implements store.ChunkStore.
//
// The vec0 KNN is wrapped in a subquery so its MATCH/k constraints stay isolated
// on its own plan and cannot be perturbed by the outer GROUP BY. MIN(distance)
// per memory is the max-pool: the same distance-to-score function then applies,
// so these scores land in the same space as VectorSearch's — which recall
// depends on, since its gates are absolute thresholds rather than ranks.
func (s *Store) ChunkVectorSearch(ctx context.Context, namespace string, vec []float32, f store.Filter, k int) ([]store.Scored, error) {
	if len(vec) != s.dims {
		return nil, fmt.Errorf("sqlitevec: query vector has %d dims, store expects %d", len(vec), s.dims)
	}
	if k <= 0 {
		return nil, nil
	}
	blob, err := sqlitevec.SerializeFloat32(vec)
	if err != nil {
		return nil, err
	}
	where, args := filterClause(f)
	// SQLite's bare-column rule inside an aggregate query: with MIN(v.distance),
	// the non-aggregated columns come from the row that produced the minimum. So
	// c.text is the winning chunk's text, which is exactly what the reranker
	// needs to judge. That is a documented SQLite guarantee for MIN/MAX, not an
	// accident of the query plan.
	q := fmt.Sprintf(`
		SELECT %s, MIN(v.distance) AS distance, c.text
		FROM (SELECT rowid, distance FROM %s
		      WHERE namespace = ? AND embedding MATCH ? AND k = ?) v
		JOIN memory_chunks c ON c.rowid = v.rowid
		JOIN memories m ON m.rowid = c.memory_rowid
		WHERE 1=1%s
		GROUP BY m.rowid
		ORDER BY distance
		LIMIT ?`, prefixed(memoryColumns, "m"), chunkVecTable, where)

	callArgs := append([]any{namespace, blob, k * chunkOverFetch}, args...)
	callArgs = append(callArgs, k)
	return s.queryScoredChunk(ctx, q, callArgs, distanceToScore)
}

// ListUnchunked implements store.ChunkStore. length() counts characters in
// SQLite (not bytes, unlike its BLOB overload), which is what internal/chunk
// bounds on, so the two agree.
func (s *Store) ListUnchunked(ctx context.Context, namespace string, minRunes, limit int) ([]*memory.Memory, error) {
	if limit <= 0 {
		return nil, nil
	}
	q := `SELECT ` + memoryColumns + ` FROM memories m
		WHERE length(m.content) > ?
		  AND NOT EXISTS (SELECT 1 FROM memory_chunks c WHERE c.memory_rowid = m.rowid)`
	args := []any{minRunes}
	if namespace != "" {
		q += ` AND m.namespace = ?`
		args = append(args, namespace)
	}
	q += ` ORDER BY m.rowid LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
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

// verifyChunkVecDims mirrors verifyVecDims for the chunk table: a store whose
// chunk vectors were built at another width would return garbage rather than
// erroring, exactly as the document vectors would.
func (s *Store) verifyChunkVecDims(ctx context.Context) error {
	var ddl string
	err := s.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, chunkVecTable).Scan(&ddl)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // table absent (a store from before chunking; migrate creates it)
	}
	if err != nil {
		return fmt.Errorf("sqlitevec: inspect %s: %w", chunkVecTable, err)
	}
	got, err := parseVecDims(ddl)
	if err != nil {
		return err
	}
	if got != s.dims {
		return fmt.Errorf("sqlitevec: chunk vectors were created with %d dims but the store is configured for %d; "+
			"set MEMINI_EMBED_DIMS=%d to match, or migrate to a new database", got, s.dims, got)
	}
	return nil
}
