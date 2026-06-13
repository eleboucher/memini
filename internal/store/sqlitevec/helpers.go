package sqlitevec

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanMemory(s scanner) (*memory.Memory, error) { return scanRow(s, nil) }

func scanMemoryWith(s scanner, metric *float64) (*memory.Memory, error) { return scanRow(s, metric) }

// scanRow scans the canonical memoryColumns into a Memory, optionally followed
// by a trailing numeric metric column (distance / bm25 rank).
func scanRow(s scanner, metric *float64) (*memory.Memory, error) {
	var (
		m                          memory.Memory
		tier, metaJSON, tagsJSON   string
		created, updated, accessed int64
		expires                    sql.NullInt64
		superseded                 sql.NullString
		validFrom, validTo         sql.NullInt64
		confidence                 sql.NullFloat64
	)
	dest := []any{
		&m.ID, &m.Namespace, &tier, &m.Content, &m.Summary, &metaJSON, &tagsJSON,
		&m.Importance, &created, &updated, &accessed, &m.AccessCount, &expires, &superseded,
		&validFrom, &validTo, &confidence,
	}
	if metric != nil {
		dest = append(dest, metric)
	}
	if err := s.Scan(dest...); err != nil {
		return nil, err
	}

	m.Tier = memory.Tier(tier)
	if err := json.Unmarshal([]byte(metaJSON), &m.Metadata); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(tagsJSON), &m.Tags); err != nil {
		return nil, err
	}
	m.CreatedAt = fromMs(created)
	m.UpdatedAt = fromMs(updated)
	m.LastAccessedAt = fromMs(accessed)
	if expires.Valid {
		t := fromMs(expires.Int64)
		m.ExpiresAt = &t
	}
	if superseded.Valid {
		m.SupersededBy = &superseded.String
	}
	if validFrom.Valid {
		t := fromMs(validFrom.Int64)
		m.ValidFrom = &t
	}
	if validTo.Valid {
		t := fromMs(validTo.Int64)
		m.ValidTo = &t
	}
	if confidence.Valid {
		c := confidence.Float64
		m.Confidence = &c
	}
	return &m, nil
}

// filterClause builds the SQL suffix and args that restrict a search to the
// subset described by f. alias is the table alias of the memories table.
func filterClause(f store.Filter, alias string) (string, []any) {
	var b strings.Builder
	var args []any

	if len(f.Tiers) > 0 {
		b.WriteString(" AND " + alias + ".tier IN (")
		for i, t := range f.Tiers {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("?")
			args = append(args, string(t))
		}
		b.WriteString(")")
	}
	// Tags: the memory must carry every listed tag (AND), matched against the
	// JSON tags array element-wise so values bind safely as parameters.
	for _, tag := range f.Tags {
		b.WriteString(" AND EXISTS (SELECT 1 FROM json_each(" + alias + ".tags) WHERE value = ?)")
		args = append(args, tag)
	}
	// Metadata: each key must be present at the top level with the given string
	// value. json_each over an object yields one (key, value) row per entry.
	for k, v := range f.Metadata {
		b.WriteString(" AND EXISTS (SELECT 1 FROM json_each(" + alias + ".metadata) WHERE key = ? AND value = ?)")
		args = append(args, k, v)
	}
	if !f.IncludeExpired {
		// For a time-travel query, "live" means live at AsOf, not at the current
		// wall clock — a memory that has since expired was still valid then.
		ref := f.Now
		if !f.AsOf.IsZero() {
			ref = f.AsOf
		}
		if ref.IsZero() {
			ref = time.Now()
		}
		b.WriteString(" AND (" + alias + ".expires_at IS NULL OR " + alias + ".expires_at > ?)")
		args = append(args, ref.UnixMilli())
	}
	if !f.AsOf.IsZero() {
		// Time-travel: keep rows whose validity window contained AsOf, regardless
		// of supersession (a then-true fact may since have been replaced).
		b.WriteString(" AND (" + alias + ".valid_from IS NULL OR " + alias + ".valid_from <= ?)")
		b.WriteString(" AND (" + alias + ".valid_to IS NULL OR " + alias + ".valid_to > ?)")
		args = append(args, f.AsOf.UnixMilli(), f.AsOf.UnixMilli())
	} else if !f.IncludeSuperseded {
		b.WriteString(" AND " + alias + ".superseded_by IS NULL")
	}
	return b.String(), args
}

// prefixed turns the memoryColumns list into "alias.col, alias.col, ..." for
// use in a joined SELECT.
func prefixed(columns, alias string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// ftsQuery turns a natural-language query into an FTS5 OR-query of quoted
// terms, so recall matches rows containing ANY term (not all of them, which is
// FTS5's default for a bare string). Returns "" when there are no usable terms.
func ftsQuery(query string) string {
	terms := tokenize(query)
	for i, t := range terms {
		terms[i] = `"` + t + `"`
	}
	return strings.Join(terms, " OR ")
}

// tokenize extracts lowercased alphanumeric terms of length >= 2.
func tokenize(s string) []string {
	var terms []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() >= 2 {
			terms = append(terms, cur.String())
		}
		cur.Reset()
	}
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return terms
}

func distanceToScore(d float64) float64 { return 1 / (1 + d) }

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMs(n int64) time.Time { return time.UnixMilli(n).UTC() }

func msPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func f64Ptr(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func strPtr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func orEmptySlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
