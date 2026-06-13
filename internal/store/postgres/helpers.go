package postgres

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// args accumulates query parameters and hands out their $N placeholders.
type args struct{ vals []any }

func (a *args) add(v any) string {
	a.vals = append(a.vals, v)
	return fmt.Sprintf("$%d", len(a.vals))
}

// filterClause appends the SQL conditions for f, registering any parameters on b.
func filterClause(b *args, f store.Filter) string {
	clause := ""
	if len(f.Tiers) > 0 {
		tiers := make([]string, len(f.Tiers))
		for i, t := range f.Tiers {
			tiers[i] = string(t)
		}
		clause += " AND tier = ANY(" + b.add(tiers) + ")"
	}
	// Tags: the memory's text[] must contain every listed tag (@> = contains).
	if len(f.Tags) > 0 {
		clause += " AND tags @> " + b.add(f.Tags)
	}
	// Metadata: the jsonb must contain each listed key=value pair. json.Marshal
	// of a map[string]string cannot fail, so the error is safe to drop. Pass the
	// JSON as a string (not []byte, which pgx would send as bytea — uncastable to
	// jsonb) so the text::jsonb cast parses it.
	if len(f.Metadata) > 0 {
		mj, _ := json.Marshal(f.Metadata) //nolint:errchkjson
		clause += " AND metadata @> " + b.add(string(mj)) + "::jsonb"
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
		clause += " AND (expires_at IS NULL OR expires_at > " + b.add(ref) + ")"
	}
	if !f.AsOf.IsZero() {
		// Time-travel: rows whose validity window contained AsOf, regardless of
		// supersession (a then-true fact may since have been replaced).
		p := b.add(f.AsOf)
		clause += " AND (valid_from IS NULL OR valid_from <= " + p + ")"
		clause += " AND (valid_to IS NULL OR valid_to > " + p + ")"
	} else if !f.IncludeSuperseded {
		clause += " AND superseded_by IS NULL"
	}
	return clause
}

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanMemory(s rowScanner) (*memory.Memory, error) { return scanRow(s, nil) }

func scanMemoryWith(s rowScanner, metric *float64) (*memory.Memory, error) { return scanRow(s, metric) }

func scanRow(s rowScanner, metric *float64) (*memory.Memory, error) {
	var (
		m                  memory.Memory
		tier               string
		metaBytes          []byte
		expires            sql.NullTime
		superseded         sql.NullString
		validFrom, validTo sql.NullTime
		confidence         sql.NullFloat64
	)
	dest := []any{
		&m.ID, &m.Namespace, &tier, &m.Content, &m.Summary, &metaBytes, &m.Tags,
		&m.Importance, &m.CreatedAt, &m.UpdatedAt, &m.LastAccessedAt, &m.AccessCount,
		&expires, &superseded, &validFrom, &validTo, &confidence,
	}
	if metric != nil {
		dest = append(dest, metric)
	}
	if err := s.Scan(dest...); err != nil {
		return nil, err
	}

	m.Tier = memory.Tier(tier)
	if len(metaBytes) > 0 {
		if err := json.Unmarshal(metaBytes, &m.Metadata); err != nil {
			return nil, err
		}
	}
	m.CreatedAt = m.CreatedAt.UTC()
	m.UpdatedAt = m.UpdatedAt.UTC()
	m.LastAccessedAt = m.LastAccessedAt.UTC()
	if expires.Valid {
		t := expires.Time.UTC()
		m.ExpiresAt = &t
	}
	if superseded.Valid {
		m.SupersededBy = &superseded.String
	}
	if validFrom.Valid {
		t := validFrom.Time.UTC()
		m.ValidFrom = &t
	}
	if validTo.Valid {
		t := validTo.Time.UTC()
		m.ValidTo = &t
	}
	if confidence.Valid {
		c := confidence.Float64
		m.Confidence = &c
	}
	return &m, nil
}

// tsQuery turns a natural-language query into a Postgres to_tsquery OR-string
// ("term1 | term2 | ..."), so recall matches rows containing ANY term. Returns
// "" when there are no usable terms.
func tsQuery(query string) string {
	var terms []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() >= 2 {
			terms = append(terms, cur.String())
		}
		cur.Reset()
	}
	for _, r := range strings.ToLower(query) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return strings.Join(terms, " | ")
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
