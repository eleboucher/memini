package sqlitevec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/ncruces"

	"github.com/eleboucher/memini/internal/store"
)

// Deferred-repair queue on sqlite (see store.RepairStore).
//
// The queue is four columns on `memories` rather than a table of its own. That
// is what makes a degraded write atomic: the row and its "still owes a vector"
// state commit together, so there is no second write to lose and no window in
// which a stored-but-unqueued memory can exist. It also means every transition
// below is a targeted UPDATE that never touches `embedding`, which structurally
// removes the Get-then-Upsert vector-loss trap documented on store.GetEmbedding
// — the repair path simply has no way to express "drop the vector".
//
// embed_next_run_at doubles as the claim lease; see store.RepairStore.

// ClaimRepairs implements store.RepairStore.
//
// The single-statement UPDATE ... WHERE id IN (SELECT ... LIMIT n) RETURNING
// form is safe here: sqlite applies every database change during the first
// sqlite3_step and embargoes RETURNING output until they are all complete, and
// writes are serialized, so two claimants cannot take the same row. This is
// pinned by TestClaimRepairsIsExclusiveUnderConcurrency rather than trusted —
// memini builds on ncruces/go-sqlite3, not the drivers the upstream reports
// cover.
//
// RETURNING row order is documented as arbitrary, so rows are re-sorted by due
// time in Go rather than relied on to arrive ordered. The rows must also be
// drained fully: the UPDATE has already applied by the time the first row is
// read, so abandoning the cursor early would strand claimed rows for a whole
// lease.
func (s *Store) ClaimRepairs(ctx context.Context, state store.RepairState, now time.Time,
	lease time.Duration, limit int) ([]store.RepairRow, error) {
	if !store.ValidRepairState(state) || state == store.RepairNone {
		return nil, fmt.Errorf("sqlitevec: claim repairs: not a claimable state %q", state)
	}
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		UPDATE memories
		   SET embed_attempts = embed_attempts + 1,
		       embed_next_run_at = ?
		 WHERE id IN (
		     SELECT id FROM memories
		      WHERE embed_state = ?
		        AND (embed_next_run_at IS NULL OR embed_next_run_at <= ?)
		      ORDER BY embed_next_run_at, id
		      LIMIT ?)
		RETURNING id, namespace, tier, content, fingerprint, embed_state, embed_attempts, embed_next_run_at`,
		ms(now.Add(lease)), string(state), ms(now), limit)
	if err != nil {
		return nil, fmt.Errorf("sqlitevec: claim repairs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type dued struct {
		row store.RepairRow
		due int64
	}
	var claimed []dued
	for rows.Next() {
		var d dued
		var st string
		var due sql.NullInt64
		if err := rows.Scan(&d.row.ID, &d.row.Namespace, &d.row.Tier, &d.row.Content,
			&d.row.Fingerprint, &st, &d.row.Attempts, &due); err != nil {
			return nil, fmt.Errorf("sqlitevec: claim repairs: scan: %w", err)
		}
		d.row.State = store.RepairState(st)
		d.due = due.Int64
		claimed = append(claimed, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlitevec: claim repairs: %w", err)
	}
	out := make([]store.RepairRow, 0, len(claimed))
	for _, d := range claimed {
		out = append(out, d.row)
	}
	return out, nil
}

// SetEmbeddingIfUnchanged implements store.RepairStore.
//
// The fingerprint guard and both writes share one transaction, so a content
// edit landing mid-repair cannot slip between them — the check-then-act race a
// re-read outside the store can never close (the same reasoning as PutChunks).
// updated_at is deliberately left alone: a system re-embed is index
// maintenance, not a logical edit.
func (s *Store) SetEmbeddingIfUnchanged(ctx context.Context, namespace, id, fingerprint string,
	vec []float32, next store.RepairState) (bool, error) {
	if !store.ValidRepairState(next) {
		return false, fmt.Errorf("sqlitevec: set embedding: invalid repair state %q", next)
	}
	if len(vec) != 0 && len(vec) != s.dims {
		return false, fmt.Errorf("sqlitevec: set embedding: got %d dims, want %d", len(vec), s.dims)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var rowID int64
	err = tx.QueryRowContext(ctx,
		`SELECT rowid FROM memories WHERE id=? AND namespace=? AND fingerprint=?`,
		id, namespace, fingerprint).Scan(&rowID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlitevec: set embedding: locate row: %w", err)
	}

	// Clear first so a repair that legitimately stores no vector cannot leave a
	// stale vec_memories row reachable by a later VectorSearch, mirroring
	// Upsert's rule.
	if _, err := tx.ExecContext(ctx, `DELETE FROM vec_memories WHERE rowid=?`, rowID); err != nil {
		return false, fmt.Errorf("sqlitevec: set embedding: clear vector: %w", err)
	}
	if len(vec) != 0 {
		blob, err := sqlitevec.SerializeFloat32(vec)
		if err != nil {
			return false, fmt.Errorf("sqlitevec: set embedding: serialize: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO vec_memories(rowid, namespace, embedding) VALUES (?,?,?)`,
			rowID, namespace, blob); err != nil {
			return false, fmt.Errorf("sqlitevec: set embedding: insert vector: %w", err)
		}
	}
	if err := setRepairColumnsTx(ctx, tx, rowID, next); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// SetRepairState implements store.RepairStore.
func (s *Store) SetRepairState(ctx context.Context, namespace, id, fingerprint string,
	next store.RepairState) (bool, error) {
	if !store.ValidRepairState(next) {
		return false, fmt.Errorf("sqlitevec: set repair state: invalid state %q", next)
	}
	// Moving to a new stage makes the row due immediately and starts that
	// stage's budget fresh. Preserving the existing embed_next_run_at instead
	// would leave the row invisible for the rest of the claim lease the stage
	// it just finished was holding — so a repair would stall for ten minutes
	// between embedding and enriching.
	res, err := s.db.ExecContext(ctx, `
		UPDATE memories
		   SET embed_state = ?,
		       embed_attempts = 0,
		       embed_next_run_at = CASE WHEN ? = '' THEN NULL ELSE 0 END,
		       embed_last_error = '',
		       -- Reaching the healthy state also strips the legacy metadata
		       -- marker; see setRepairColumnsTx for why leaving it would loop.
		       metadata = CASE WHEN ? = '' THEN json_remove(metadata, '$.pending_embed') ELSE metadata END
		 WHERE id=? AND namespace=? AND fingerprint=?`,
		string(next), string(next), string(next), id, namespace, fingerprint)
	if err != nil {
		return false, fmt.Errorf("sqlitevec: set repair state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// setRepairColumnsTx advances a row's repair columns inside an open
// transaction. Reaching store.RepairNone resets the attempt count, due time and
// last error together, so a row that later degrades again starts from a clean
// slate rather than inheriting a stale failure record.
func setRepairColumnsTx(ctx context.Context, tx *sql.Tx, rowID int64, next store.RepairState) error {
	if next == store.RepairNone {
		// json_remove strips the legacy metadata marker in the same statement.
		// Without it a repaired row still reads as pending to every consumer of
		// Memory.PendingEmbed (stats, doctor, the UI badge) AND the sweeper's
		// compat scan re-adopts it from that marker on the next tick — an
		// infinite repair loop costing one embedder call a minute, forever.
		_, err := tx.ExecContext(ctx, `
			UPDATE memories
			   SET embed_state='', embed_attempts=0, embed_next_run_at=NULL, embed_last_error='',
			       metadata=json_remove(metadata, '$.pending_embed')
			 WHERE rowid=?`, rowID)
		if err != nil {
			return fmt.Errorf("sqlitevec: clear repair state: %w", err)
		}
		return nil
	}
	// A stage advance (pending -> enrich) starts the next stage's budget fresh:
	// the attempts spent getting a vector say nothing about whether enrichment
	// will succeed, and carrying them over would park the new stage early.
	_, err := tx.ExecContext(ctx, `
		UPDATE memories
		   SET embed_state=?, embed_attempts=0, embed_next_run_at=0, embed_last_error=''
		 WHERE rowid=?`, string(next), rowID)
	if err != nil {
		return fmt.Errorf("sqlitevec: set repair state: %w", err)
	}
	return nil
}

// FailRepair implements store.RepairStore.
func (s *Store) FailRepair(ctx context.Context, namespace, id, lastErr string, nextRunAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE memories SET embed_next_run_at=?, embed_last_error=?
		 WHERE id=? AND namespace=?`,
		ms(nextRunAt), truncErr(lastErr), id, namespace)
	if err != nil {
		return fmt.Errorf("sqlitevec: fail repair: %w", err)
	}
	return nil
}

// ParkRepair implements store.RepairStore.
//
// The park instant is recorded in embed_next_run_at. That column means "not
// before" for a claimable row, and a parked row is never claimable (the claim
// filters on state), so reusing it as "parked at" costs no extra column and
// keeps RearmRepairs on the same partial index. It must NOT be updated_at:
// repairs deliberately never bump that, precisely so a system re-embed cannot
// be mistaken for a content edit.
func (s *Store) ParkRepair(ctx context.Context, namespace, id, lastErr string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE memories SET embed_state=?, embed_next_run_at=?, embed_last_error=?
		 WHERE id=? AND namespace=?`,
		string(store.RepairFailed), ms(now), truncErr(lastErr), id, namespace)
	if err != nil {
		return fmt.Errorf("sqlitevec: park repair: %w", err)
	}
	return nil
}

// RearmRepairs implements store.RepairStore. Parked rows are identified by the
// park instant ParkRepair left in embed_next_run_at.
func (s *Store) RearmRepairs(ctx context.Context, failedBefore, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE memories
		   SET embed_state=?, embed_attempts=0, embed_next_run_at=?
		 WHERE embed_state=? AND embed_next_run_at <= ?`,
		string(store.RepairPending), ms(now), string(store.RepairFailed), ms(failedBefore))
	if err != nil {
		return 0, fmt.Errorf("sqlitevec: rearm repairs: %w", err)
	}
	return res.RowsAffected()
}

// MarkRepairNeeded implements store.RepairStore.
func (s *Store) MarkRepairNeeded(ctx context.Context, namespace string, ids []string,
	state store.RepairState) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if !store.ValidRepairState(state) || state == store.RepairNone {
		return 0, fmt.Errorf("sqlitevec: mark repair: not a claimable state %q", state)
	}
	args := make([]any, 0, len(ids)+3)
	args = append(args, string(state))
	ph := make([]string, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	q := `UPDATE memories SET embed_state=?, embed_attempts=0, embed_next_run_at=0
	       WHERE embed_state='' AND id IN (` + strings.Join(ph, ",") + `)`
	if namespace != "" {
		q += ` AND namespace=?`
		args = append(args, namespace)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("sqlitevec: mark repair: %w", err)
	}
	return res.RowsAffected()
}

// RepairStats implements store.RepairStore.
func (s *Store) RepairStats(ctx context.Context) ([]store.RepairStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT embed_state, COUNT(*), MIN(created_at),
		       COALESCE(MAX(embed_last_error), '')
		  FROM memories
		 WHERE embed_state <> ''
		 GROUP BY embed_state
		 ORDER BY embed_state`)
	if err != nil {
		return nil, fmt.Errorf("sqlitevec: repair stats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.RepairStat
	for rows.Next() {
		var st store.RepairStat
		var state string
		var oldest sql.NullInt64
		if err := rows.Scan(&state, &st.Count, &oldest, &st.LastError); err != nil {
			return nil, fmt.Errorf("sqlitevec: repair stats: scan: %w", err)
		}
		st.State = store.RepairState(state)
		if oldest.Valid {
			st.OldestAt = fromMs(oldest.Int64)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// RepairStateOf implements store.RepairStore.
func (s *Store) RepairStateOf(ctx context.Context, namespace, id string) (store.RepairState, int, string, error) {
	var state, lastErr string
	var attempts int
	err := s.db.QueryRowContext(ctx,
		`SELECT embed_state, embed_attempts, embed_last_error FROM memories WHERE id=? AND namespace=?`,
		id, namespace).Scan(&state, &attempts, &lastErr)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, "", store.ErrNotFound
	}
	if err != nil {
		return "", 0, "", fmt.Errorf("sqlitevec: repair state: %w", err)
	}
	return store.RepairState(state), attempts, lastErr, nil
}

// repairErrMaxLen bounds a stored provider error so one enormous response body
// cannot bloat a memory row that is read on every repair tick.
const repairErrMaxLen = 1000

func truncErr(s string) string {
	if len(s) <= repairErrMaxLen {
		return s
	}
	return s[:repairErrMaxLen] + "…"
}

// compile-time proof the driver satisfies the optional capability.
var _ store.RepairStore = (*Store)(nil)

// repairDueAt is the embed_next_run_at an Upsert should write for a given
// repair state: 0 (immediately due) when the write owes a repair, NULL when it
// does not.
//
// It is what makes a degraded write self-queueing. The state and its due time
// land in the same statement as the memory, so a write that could not reach the
// embedder is claimable by the repair worker the moment it commits, with no
// enqueue that could be lost in between.
func repairDueAt(state string) any {
	if state == "" {
		return nil
	}
	return int64(0)
}
