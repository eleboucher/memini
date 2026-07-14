package sqlitevec

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

var _ store.EventLogStore = (*Store)(nil)

const eventColumns = `id, op_id, kind, namespace, query, memory_id, memory_ns,
	memory_tier, memory_summary, rank, score, detail, actor, actor_kind, created_at`

// AppendEvents inserts one operation's rows in a single transaction, so they
// land contiguously and share a created_at — the adjacency ListEvents' ordering
// relies on to let the reader regroup flat rows back into whole events.
func (s *Store) AppendEvents(ctx context.Context, events []store.Event) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, e := range events {
		detail, err := json.Marshal(store.OrEmptyMap(e.Detail))
		if err != nil {
			return fmt.Errorf("sqlitevec: marshal event detail: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO memory_events
				(op_id, kind, namespace, query, memory_id, memory_ns, memory_tier,
				 memory_summary, rank, score, detail, actor, actor_kind, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			e.OpID, string(e.Kind), e.Namespace, e.Query, e.MemoryID, e.MemoryNS,
			string(e.MemoryTier), e.MemorySummary, e.Rank, f64Ptr(e.Score),
			string(detail), e.Actor, e.ActorKind, ms(e.CreatedAt),
		); err != nil {
			return fmt.Errorf("sqlitevec: insert event: %w", err)
		}
	}
	return tx.Commit()
}

// ListEvents returns rows matching f, newest first.
func (s *Store) ListEvents(ctx context.Context, f store.EventFilter) ([]store.Event, error) {
	var b strings.Builder
	var args []any
	b.WriteString(`SELECT ` + eventColumns + ` FROM memory_events WHERE 1=1`)

	if f.Namespace != "" {
		b.WriteString(" AND namespace = ?")
		args = append(args, f.Namespace)
	} else if len(f.Namespaces) > 0 {
		b.WriteString(" AND namespace IN (")
		for i, ns := range f.Namespaces {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("?")
			args = append(args, ns)
		}
		b.WriteString(")")
	}
	if len(f.Kinds) > 0 {
		b.WriteString(" AND kind IN (")
		for i, k := range f.Kinds {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("?")
			args = append(args, string(k))
		}
		b.WriteString(")")
	}
	if f.Actor != "" {
		b.WriteString(" AND actor = ?")
		args = append(args, f.Actor)
	}
	if !f.Since.IsZero() {
		b.WriteString(" AND created_at >= ?")
		args = append(args, ms(f.Since))
	}
	// Tiers and Text match the operation, not the row: a recall that served a
	// semantic memory is returned whole — all of its rows — so the event still
	// reports what it actually served. Filtering rows here instead would make a
	// 5-memory recall render as a 2-memory one.
	if len(f.Tiers) > 0 {
		b.WriteString(" AND op_id IN (SELECT op_id FROM memory_events WHERE memory_tier IN (")
		for i, t := range f.Tiers {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("?")
			args = append(args, string(t))
		}
		b.WriteString("))")
	}
	if f.Text != "" {
		pat := "%" + escapeLike(strings.ToLower(f.Text)) + "%"
		b.WriteString(` AND op_id IN (SELECT op_id FROM memory_events
			WHERE lower(query) LIKE ? ESCAPE '\' OR lower(memory_summary) LIKE ? ESCAPE '\')`)
		args = append(args, pat, pat)
	}
	// Keyset cursor: strictly older than (Before, BeforeID) in the
	// (created_at DESC, id DESC) ordering, so a page never repeats or skips a
	// row when new events land mid-pagination (an OFFSET would do both).
	if !f.Before.IsZero() {
		b.WriteString(" AND (created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, ms(f.Before), ms(f.Before), f.BeforeID)
	}
	b.WriteString(" ORDER BY created_at DESC, id DESC")
	if f.Limit > 0 {
		b.WriteString(" LIMIT ?")
		args = append(args, f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []store.Event
	for rows.Next() {
		var (
			e         store.Event
			kind      string
			tier      string
			detail    string
			score     *float64
			createdAt int64
		)
		if err := rows.Scan(&e.ID, &e.OpID, &kind, &e.Namespace, &e.Query, &e.MemoryID,
			&e.MemoryNS, &tier, &e.MemorySummary, &e.Rank, &score, &detail,
			&e.Actor, &e.ActorKind, &createdAt); err != nil {
			return nil, err
		}
		e.Kind = store.EventKind(kind)
		e.MemoryTier = memory.Tier(tier)
		e.Score = score
		e.CreatedAt = fromMs(createdAt)
		if detail != "" {
			if err := json.Unmarshal([]byte(detail), &e.Detail); err != nil {
				return nil, fmt.Errorf("sqlitevec: unmarshal event detail: %w", err)
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneEvents trims the log by age and by row cap.
func (s *Store) PruneEvents(ctx context.Context, olderThan time.Time, keepMax int) (int64, error) {
	var deleted int64
	if !olderThan.IsZero() {
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM memory_events WHERE created_at < ?`, ms(olderThan))
		if err != nil {
			return deleted, fmt.Errorf("sqlitevec: prune events by age: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return deleted, err
		}
		deleted += n
	}
	if keepMax > 0 {
		// Delete everything outside the newest keepMax rows. LIMIT -1 means "no
		// limit" in SQLite, which is how an OFFSET-only subselect is spelled.
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM memory_events WHERE id IN (
				SELECT id FROM memory_events
				ORDER BY created_at DESC, id DESC
				LIMIT -1 OFFSET ?
			)`, keepMax)
		if err != nil {
			return deleted, fmt.Errorf("sqlitevec: prune events by cap: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return deleted, err
		}
		deleted += n
	}
	return deleted, nil
}
