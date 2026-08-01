package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"github.com/eleboucher/memini/internal/store"
)

var _ store.RepairStore = (*Store)(nil)

// Deferred-repair queue on postgres (see store.RepairStore).
//
// The state is four columns on `memories`, not a table of its own, so a
// degraded write and its "still owes a vector" marker commit together. Every
// transition below is a targeted UPDATE that never touches `embedding`, which
// structurally removes the Get-then-Upsert vector-loss trap documented on
// store.GetEmbedding.
//
// Unlike sqlite, the lease here is computed by the database (now() in SQL, not
// the caller's Go clock). With multiple replicas — the topology the Helm chart
// documents for this backend — the lease's whole safety property is "no other
// process's clock says this row is due yet", and that has to be a statement
// about the database's clock rather than about how well N machines agree with
// each other. The now parameter is accepted for interface parity and
// deliberately ignored.

// ClaimRepairs implements store.RepairStore.
//
// FOR UPDATE SKIP LOCKED is what lets concurrent claimants pick disjoint rows
// without blocking. Without it READ COMMITTED still yields correct results (the
// loser re-evaluates the WHERE and drops the row) but claimants serialize on
// each other, which defeats the point of running more than one replica.
func (s *Store) ClaimRepairs(ctx context.Context, state store.RepairState, _ time.Time,
	lease time.Duration, limit int) ([]store.RepairRow, error) {
	if !store.ValidRepairState(state) || state == store.RepairNone {
		return nil, fmt.Errorf("postgres: claim repairs: not a claimable state %q", state)
	}
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		WITH claimed AS (
		    SELECT id FROM memories
		     WHERE embed_state = $1
		       AND (embed_next_run_at IS NULL OR embed_next_run_at <= now())
		     ORDER BY embed_next_run_at NULLS FIRST, id
		     LIMIT $2
		     FOR UPDATE SKIP LOCKED)
		UPDATE memories m
		   SET embed_attempts = m.embed_attempts + 1,
		       embed_next_run_at = now() + $3::interval
		  FROM claimed
		 WHERE m.id = claimed.id
		RETURNING m.id, m.namespace, m.tier, m.content, m.fingerprint, m.embed_state, m.embed_attempts`,
		string(state), limit, lease)
	if err != nil {
		return nil, fmt.Errorf("postgres: claim repairs: %w", err)
	}
	defer rows.Close()

	var out []store.RepairRow
	for rows.Next() {
		var r store.RepairRow
		var st string
		if err := rows.Scan(&r.ID, &r.Namespace, &r.Tier, &r.Content, &r.Fingerprint,
			&st, &r.Attempts); err != nil {
			return nil, fmt.Errorf("postgres: claim repairs: scan: %w", err)
		}
		r.State = store.RepairState(st)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetEmbeddingIfUnchanged implements store.RepairStore. Guard and write are one
// statement, so a content edit landing mid-repair cannot slip between them.
// updated_at is deliberately untouched: a system re-embed is index maintenance,
// not a logical edit.
func (s *Store) SetEmbeddingIfUnchanged(ctx context.Context, namespace, id, fingerprint string,
	vec []float32, next store.RepairState) (bool, error) {
	if !store.ValidRepairState(next) {
		return false, fmt.Errorf("postgres: set embedding: invalid repair state %q", next)
	}
	if len(vec) != 0 && len(vec) != s.dims {
		return false, fmt.Errorf("postgres: set embedding: got %d dims, want %d", len(vec), s.dims)
	}
	// A literal Go nil (not a typed nil) so pgx's nil fast path encodes SQL
	// NULL, matching Upsert's handling of a vectorless row.
	var embArg any
	if len(vec) != 0 {
		embArg = pgvector.NewVector(vec)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE memories
		   SET embedding = $1,
		       embed_state = $2,
		       embed_attempts = 0,
		       embed_next_run_at = CASE WHEN $2 = '' THEN NULL ELSE now() END,
		       embed_last_error = '',
		       metadata = CASE WHEN $2 = '' THEN metadata - 'pending_embed' ELSE metadata END
		 WHERE id = $3 AND namespace = $4 AND fingerprint = $5`,
		embArg, string(next), id, namespace, fingerprint)
	if err != nil {
		return false, fmt.Errorf("postgres: set embedding: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetRepairState implements store.RepairStore.
func (s *Store) SetRepairState(ctx context.Context, namespace, id, fingerprint string,
	next store.RepairState) (bool, error) {
	if !store.ValidRepairState(next) {
		return false, fmt.Errorf("postgres: set repair state: invalid state %q", next)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE memories
		   SET embed_state = $1,
		       embed_attempts = 0,
		       embed_next_run_at = CASE WHEN $1 = '' THEN NULL ELSE now() END,
		       embed_last_error = '',
		       -- Reaching the healthy state also strips the legacy metadata
		       -- marker: leaving it would make every consumer of
		       -- Memory.PendingEmbed still report the row as degraded, and the
		       -- sweeper's compat scan would re-adopt it every tick forever.
		       metadata = CASE WHEN $1 = '' THEN metadata - 'pending_embed' ELSE metadata END
		 WHERE id = $2 AND namespace = $3 AND fingerprint = $4`,
		string(next), id, namespace, fingerprint)
	if err != nil {
		return false, fmt.Errorf("postgres: set repair state: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// FailRepair implements store.RepairStore. It does not touch embed_attempts:
// that was charged at claim time, so a crashed run and a failed run cost the
// same, which is what keeps the attempt ceiling honest.
func (s *Store) FailRepair(ctx context.Context, namespace, id, lastErr string, nextRunAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE memories SET embed_next_run_at = $1, embed_last_error = $2
		 WHERE id = $3 AND namespace = $4`,
		nextRunAt.UTC(), truncErr(lastErr), id, namespace)
	if err != nil {
		return fmt.Errorf("postgres: fail repair: %w", err)
	}
	return nil
}

// ParkRepair implements store.RepairStore. The park instant goes in
// embed_next_run_at — a parked row is never claimable (the claim filters on
// state), so the column is free to mean "parked at" and RearmRepairs stays on
// the same partial index. It must not be updated_at: repairs never bump that,
// precisely so a system re-embed cannot read as a content edit.
func (s *Store) ParkRepair(ctx context.Context, namespace, id, lastErr string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE memories SET embed_state = $1, embed_next_run_at = $2, embed_last_error = $3
		 WHERE id = $4 AND namespace = $5`,
		string(store.RepairFailed), now.UTC(), truncErr(lastErr), id, namespace)
	if err != nil {
		return fmt.Errorf("postgres: park repair: %w", err)
	}
	return nil
}

// RearmRepairs implements store.RepairStore.
func (s *Store) RearmRepairs(ctx context.Context, failedBefore, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE memories
		   SET embed_state = $1, embed_attempts = 0, embed_next_run_at = $2
		 WHERE embed_state = $3 AND embed_next_run_at <= $4`,
		string(store.RepairPending), now.UTC(), string(store.RepairFailed), failedBefore.UTC())
	if err != nil {
		return 0, fmt.Errorf("postgres: rearm repairs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// MarkRepairNeeded implements store.RepairStore.
func (s *Store) MarkRepairNeeded(ctx context.Context, namespace string, ids []string,
	state store.RepairState) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if !store.ValidRepairState(state) || state == store.RepairNone {
		return 0, fmt.Errorf("postgres: mark repair: not a claimable state %q", state)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE memories
		   SET embed_state = $1, embed_attempts = 0, embed_next_run_at = now()
		 WHERE embed_state = '' AND id = ANY($2)
		   AND ($3 = '' OR namespace = $3)`,
		string(state), ids, namespace)
	if err != nil {
		return 0, fmt.Errorf("postgres: mark repair: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RepairStats implements store.RepairStore.
func (s *Store) RepairStats(ctx context.Context) ([]store.RepairStat, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT embed_state, COUNT(*), MIN(created_at), COALESCE(MAX(embed_last_error), '')
		  FROM memories
		 WHERE embed_state <> ''
		 GROUP BY embed_state
		 ORDER BY embed_state`)
	if err != nil {
		return nil, fmt.Errorf("postgres: repair stats: %w", err)
	}
	defer rows.Close()
	var out []store.RepairStat
	for rows.Next() {
		var st store.RepairStat
		var state string
		var oldest *time.Time
		if err := rows.Scan(&state, &st.Count, &oldest, &st.LastError); err != nil {
			return nil, fmt.Errorf("postgres: repair stats: scan: %w", err)
		}
		st.State = store.RepairState(state)
		if oldest != nil {
			st.OldestAt = oldest.UTC()
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// RepairStateOf implements store.RepairStore.
func (s *Store) RepairStateOf(ctx context.Context, namespace, id string) (store.RepairState, int, string, error) {
	var state, lastErr string
	var attempts int
	err := s.pool.QueryRow(ctx,
		`SELECT embed_state, embed_attempts, embed_last_error FROM memories WHERE id=$1 AND namespace=$2`,
		id, namespace).Scan(&state, &attempts, &lastErr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, "", store.ErrNotFound
	}
	if err != nil {
		return "", 0, "", fmt.Errorf("postgres: repair state: %w", err)
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
