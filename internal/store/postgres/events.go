package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

var _ store.EventLogStore = (*Store)(nil)

const eventColumns = `id, op_id, kind, namespace, query, memory_id, memory_ns,
	memory_tier, memory_summary, rank, score, detail, actor, actor_kind, created_at`

// AppendEvents inserts one operation's rows in a single batch, so they land
// contiguously and share a created_at — the adjacency ListEvents' ordering
// relies on to let the reader regroup flat rows back into whole events.
func (s *Store) AppendEvents(ctx context.Context, events []store.Event) error {
	if len(events) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, e := range events {
		detail, err := json.Marshal(store.OrEmptyMap(e.Detail))
		if err != nil {
			return fmt.Errorf("postgres: marshal event detail: %w", err)
		}
		batch.Queue(
			`INSERT INTO memory_events
				(op_id, kind, namespace, query, memory_id, memory_ns, memory_tier,
				 memory_summary, rank, score, detail, actor, actor_kind, created_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12,$13,$14)`,
			e.OpID, string(e.Kind), e.Namespace, e.Query, e.MemoryID, e.MemoryNS,
			string(e.MemoryTier), e.MemorySummary, e.Rank, e.Score,
			string(detail), e.Actor, e.ActorKind, e.CreatedAt,
		)
	}
	if err := s.pool.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("postgres: insert events: %w", err)
	}
	return nil
}

// ListEvents returns rows matching f, newest first.
func (s *Store) ListEvents(ctx context.Context, f store.EventFilter) ([]store.Event, error) {
	b := &args{}
	var q strings.Builder
	q.WriteString(`SELECT ` + eventColumns + ` FROM memory_events WHERE true`)

	if f.Namespace != "" {
		q.WriteString(" AND namespace = " + b.add(f.Namespace))
	} else if len(f.Namespaces) > 0 {
		q.WriteString(" AND namespace = ANY(" + b.add(f.Namespaces) + ")")
	}
	if len(f.Kinds) > 0 {
		kinds := make([]string, len(f.Kinds))
		for i, k := range f.Kinds {
			kinds[i] = string(k)
		}
		q.WriteString(" AND kind = ANY(" + b.add(kinds) + ")")
	}
	if f.Actor != "" {
		q.WriteString(" AND actor = " + b.add(f.Actor))
	}
	if !f.Since.IsZero() {
		q.WriteString(" AND created_at >= " + b.add(f.Since))
	}
	// Tiers and Text match the operation, not the row: a recall that served a
	// semantic memory is returned whole — all of its rows — so the event still
	// reports what it actually served. Filtering rows here instead would make a
	// 5-memory recall render as a 2-memory one.
	if len(f.Tiers) > 0 {
		tiers := make([]string, len(f.Tiers))
		for i, t := range f.Tiers {
			tiers[i] = string(t)
		}
		q.WriteString(" AND op_id IN (SELECT op_id FROM memory_events WHERE memory_tier = ANY(" +
			b.add(tiers) + "))")
	}
	if f.Text != "" {
		pat := b.add("%" + escapeLike(f.Text) + "%")
		q.WriteString(" AND op_id IN (SELECT op_id FROM memory_events WHERE query ILIKE " + pat +
			" ESCAPE '\\' OR memory_summary ILIKE " + pat + " ESCAPE '\\')")
	}
	// Keyset cursor: strictly older than (Before, BeforeID) in the
	// (created_at DESC, id DESC) ordering, so a page never repeats or skips a
	// row when new events land mid-pagination (an OFFSET would do both).
	if !f.Before.IsZero() {
		before := b.add(f.Before)
		q.WriteString(" AND (created_at < " + before +
			" OR (created_at = " + before + " AND id < " + b.add(f.BeforeID) + "))")
	}
	q.WriteString(" ORDER BY created_at DESC, id DESC")
	if f.Limit > 0 {
		q.WriteString(" LIMIT " + b.add(f.Limit))
	}

	rs, err := s.pool.Query(ctx, q.String(), b.vals...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()

	var out []store.Event
	for rs.Next() {
		var (
			e      store.Event
			kind   string
			tier   string
			detail []byte
		)
		if err := rs.Scan(&e.ID, &e.OpID, &kind, &e.Namespace, &e.Query, &e.MemoryID,
			&e.MemoryNS, &tier, &e.MemorySummary, &e.Rank, &e.Score, &detail,
			&e.Actor, &e.ActorKind, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Kind = store.EventKind(kind)
		e.MemoryTier = memory.Tier(tier)
		e.CreatedAt = e.CreatedAt.UTC()
		if len(detail) > 0 {
			if err := json.Unmarshal(detail, &e.Detail); err != nil {
				return nil, fmt.Errorf("postgres: unmarshal event detail: %w", err)
			}
		}
		out = append(out, e)
	}
	return out, rs.Err()
}

// PruneEvents trims the log by age and by row cap.
func (s *Store) PruneEvents(ctx context.Context, olderThan time.Time, keepMax int) (int64, error) {
	var deleted int64
	if !olderThan.IsZero() {
		tag, err := s.pool.Exec(ctx,
			`DELETE FROM memory_events WHERE created_at < $1`, olderThan)
		if err != nil {
			return deleted, fmt.Errorf("postgres: prune events by age: %w", err)
		}
		deleted += tag.RowsAffected()
	}
	if keepMax > 0 {
		// Delete everything outside the newest keepMax rows.
		tag, err := s.pool.Exec(ctx,
			`DELETE FROM memory_events WHERE id IN (
				SELECT id FROM memory_events
				ORDER BY created_at DESC, id DESC
				OFFSET $1
			)`, keepMax)
		if err != nil {
			return deleted, fmt.Errorf("postgres: prune events by cap: %w", err)
		}
		deleted += tag.RowsAffected()
	}
	return deleted, nil
}
