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
	// Level: mirror the tiers pattern — bind-safe parameter array for each level.
	if len(f.Levels) > 0 {
		levels := make([]string, len(f.Levels))
		for i, l := range f.Levels {
			levels[i] = string(l)
		}
		clause += " AND level = ANY(" + b.add(levels) + ")"
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
	// ExcludeMetadata: drop rows carrying any of these key=value pairs (inverse of
	// Metadata), e.g. excluding a session's own captures from its auto-recall.
	if len(f.ExcludeMetadata) > 0 {
		var ex strings.Builder
		for k, v := range f.ExcludeMetadata {
			mj, _ := json.Marshal(map[string]string{k: v}) //nolint:errchkjson
			ex.WriteString(" AND NOT (metadata @> " + b.add(string(mj)) + "::jsonb)")
		}
		clause += ex.String()
	}
	// For a time-travel query, "live" means live at AsOf, not at the current
	// wall clock — a memory that has since expired was still valid then.
	ref := f.Now
	if !f.AsOf.IsZero() {
		ref = f.AsOf
	}
	if ref.IsZero() {
		ref = time.Now()
	}
	if !f.IncludeExpired {
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
		// A fact whose validity window has closed (valid_to in the past) is no
		// longer current: drop it from live recall while AsOf and
		// IncludeSuperseded can still reach it. This is how a contradiction
		// invalidates the superseded fact without deleting it.
		clause += " AND (valid_to IS NULL OR valid_to > " + b.add(ref) + ")"
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
		tier, level        string
		metaBytes          []byte
		linkedBytes        []byte
		expires            sql.NullTime
		superseded         sql.NullString
		validFrom, validTo sql.NullTime
		confidence         sql.NullFloat64
	)
	dest := []any{
		&m.ID, &m.Namespace, &tier, &m.Content, &m.Summary, &metaBytes, &m.Tags,
		&m.Importance, &m.CreatedAt, &m.UpdatedAt, &m.LastAccessedAt, &m.AccessCount,
		&expires, &superseded, &validFrom, &validTo, &confidence, &level, &linkedBytes,
	}
	if metric != nil {
		dest = append(dest, metric)
	}
	if err := s.Scan(dest...); err != nil {
		return nil, err
	}

	m.Tier = memory.Tier(tier)
	m.Level = memory.Level(level)
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
	if len(linkedBytes) > 0 {
		if err := json.Unmarshal(linkedBytes, &m.LinkedMemoryIDs); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

// tiersOrEmpty returns tiers, or an empty slice when tiers is nil, so
// PutLink persists a JSON array ('[]') rather than a JSON null for an
// unrestricted (service-default) link.
func tiersOrEmpty(tiers []memory.Tier) []memory.Tier {
	if tiers == nil {
		return []memory.Tier{}
	}
	return tiers
}

// rows is satisfied by pgx.Rows: rowScanner plus iteration.
type rows interface {
	rowScanner
	Next() bool
	Err() error
	Close()
}

// scanLinks reads every remaining row of rs into NamespaceLinks, closing rs
// before returning.
func scanLinks(rs rows) ([]store.NamespaceLink, error) {
	defer rs.Close()
	var out []store.NamespaceLink
	for rs.Next() {
		l, err := scanLink(rs)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rs.Err()
}

// scanLink scans a single (src_ns, dst_ns, tiers, note, created_at) row.
func scanLink(s rowScanner) (store.NamespaceLink, error) {
	var l store.NamespaceLink
	var tiersBytes []byte
	if err := s.Scan(&l.Src, &l.Dst, &tiersBytes, &l.Note, &l.CreatedAt); err != nil {
		return l, err
	}
	if len(tiersBytes) > 0 {
		if err := json.Unmarshal(tiersBytes, &l.Tiers); err != nil {
			return l, fmt.Errorf("postgres: unmarshal link tiers: %w", err)
		}
	}
	l.CreatedAt = l.CreatedAt.UTC()
	return l, nil
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
