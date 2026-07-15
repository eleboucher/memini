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

// prefixed qualifies each column in a comma-separated list with alias, for a
// query that joins another table carrying the same column names. Mirrors
// sqlitevec's helper of the same name.
func prefixed(columns, alias string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// filterClause appends the SQL conditions for f, registering any parameters on
// b, with every column left unqualified. Safe only in a single-table query.
func filterClause(b *args, f store.Filter) string {
	return filterClauseOn(b, f, "")
}

// filterClauseOn is filterClause with every column qualified by `alias` (pass
// "" for none). A join needs this: memory_chunks carries `namespace` and
// `embedding` just as memories does, so an unqualified reference is ambiguous
// and Postgres rejects the query outright rather than picking a table.
// sqlitevec's equivalent has always been aliased (see its `alias` const).
func filterClauseOn(b *args, f store.Filter, alias string) string {
	q := ""
	if alias != "" {
		q = alias + "."
	}
	clause := ""
	if len(f.Tiers) > 0 {
		tiers := make([]string, len(f.Tiers))
		for i, t := range f.Tiers {
			tiers[i] = string(t)
		}
		clause += " AND " + q + "tier = ANY(" + b.add(tiers) + ")"
	}
	// Level: mirror the tiers pattern — bind-safe parameter array for each level.
	if len(f.Levels) > 0 {
		levels := make([]string, len(f.Levels))
		for i, l := range f.Levels {
			levels[i] = string(l)
		}
		clause += " AND " + q + "level = ANY(" + b.add(levels) + ")"
	}
	// Tags: the memory's text[] must contain every listed tag (@> = contains).
	if len(f.Tags) > 0 {
		clause += " AND " + q + "tags @> " + b.add(f.Tags)
	}
	// Metadata: the jsonb must contain each listed key=value pair. json.Marshal
	// of a map[string]string cannot fail, so the error is safe to drop. Pass the
	// JSON as a string (not []byte, which pgx would send as bytea — uncastable to
	// jsonb) so the text::jsonb cast parses it.
	if len(f.Metadata) > 0 {
		mj, _ := json.Marshal(f.Metadata) //nolint:errchkjson
		clause += " AND " + q + "metadata @> " + b.add(string(mj)) + "::jsonb"
	}
	// ExcludeMetadata: drop rows carrying any of these key=value pairs (inverse of
	// Metadata), e.g. excluding a session's own captures from its auto-recall.
	if len(f.ExcludeMetadata) > 0 {
		var ex strings.Builder
		for k, v := range f.ExcludeMetadata {
			mj, _ := json.Marshal(map[string]string{k: v}) //nolint:errchkjson
			ex.WriteString(" AND NOT (" + q + "metadata @> " + b.add(string(mj)) + "::jsonb)")
		}
		clause += ex.String()
	}
	// ExcludeIDs: drop the listed ids before ranking and the caller's limit, so
	// an excluded hit never consumes a result slot.
	if len(f.ExcludeIDs) > 0 {
		clause += " AND NOT (" + q + "id = ANY(" + b.add(f.ExcludeIDs) + "))"
	}
	// MemoryTypes: metadata.memory_type matching ANY listed value (OR), unlike
	// Metadata's AND-with-one-value-per-key.
	if len(f.MemoryTypes) > 0 {
		clause += " AND " + q + "metadata->>'memory_type' = ANY(" + b.add(f.MemoryTypes) + ")"
	}
	if !f.CreatedAfter.IsZero() {
		clause += " AND " + q + "created_at >= " + b.add(f.CreatedAfter)
	}
	if !f.AccessedAfter.IsZero() {
		clause += " AND " + q + "last_accessed_at >= " + b.add(f.AccessedAfter)
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
		clause += " AND (" + q + "expires_at IS NULL OR " + q + "expires_at > " + b.add(ref) + ")"
	}
	if !f.AsOf.IsZero() {
		// Time-travel: rows whose validity window contained AsOf, regardless of
		// supersession (a then-true fact may since have been replaced).
		p := b.add(f.AsOf)
		clause += " AND (" + q + "valid_from IS NULL OR " + q + "valid_from <= " + p + ")"
		clause += " AND (" + q + "valid_to IS NULL OR " + q + "valid_to > " + p + ")"
	} else if !f.IncludeSuperseded {
		clause += " AND " + q + "superseded_by IS NULL"
		// A fact whose validity window has closed (valid_to in the past) is no
		// longer current: drop it from live recall while AsOf and
		// IncludeSuperseded can still reach it. This is how a contradiction
		// invalidates the superseded fact without deleting it.
		clause += " AND (" + q + "valid_to IS NULL OR " + q + "valid_to > " + b.add(ref) + ")"
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

// scanAPIKeys reads every remaining row of rs into APIKeys, closing rs
// before returning.
func scanAPIKeys(rs rows) ([]store.APIKey, error) {
	defer rs.Close()
	var out []store.APIKey
	for rs.Next() {
		k, err := scanAPIKey(rs)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rs.Err()
}

// scanAPIKey scans a single (name, key_hash, home_ns, default_ns,
// created_at, disabled, settings, admin) row.
func scanAPIKey(s rowScanner) (store.APIKey, error) {
	var k store.APIKey
	var settingsBytes []byte
	if err := s.Scan(&k.Name, &k.Hash, &k.HomeNS, &k.DefaultNS, &k.CreatedAt, &k.Disabled, &settingsBytes, &k.Admin); err != nil {
		return k, err
	}
	k.CreatedAt = k.CreatedAt.UTC()
	if len(settingsBytes) > 0 {
		// Tolerant decode: unknown fields in an older/newer writer's blob are
		// ignored (json.Unmarshal's default behavior) — strict validation is
		// the REST boundary's job, not the store's.
		if err := json.Unmarshal(settingsBytes, &k.Settings); err != nil {
			return k, fmt.Errorf("postgres: unmarshal api key settings: %w", err)
		}
	}
	return k, nil
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

// orderClause maps a sort onto an ORDER BY over the memories table. The column
// comes from a whitelist switch, never from the caller's string, so an
// unrecognized key degrades to created_at rather than reaching SQL. Ties break
// on id so a capped listing is deterministic — and so the ordering matches the
// sqlite driver's byte for byte, which the all-namespaces merge relies on.
func orderClause(s store.Sort) string {
	col := "created_at"
	switch s.Key {
	case store.SortUpdatedAt:
		col = "updated_at"
	case store.SortLastAccessedAt:
		col = "last_accessed_at"
	case store.SortAccessCount:
		col = "access_count"
	case store.SortImportance:
		col = "importance"
	}
	dir := "DESC"
	if s.Asc {
		dir = "ASC"
	}
	return " ORDER BY " + col + " " + dir + ", id ASC"
}

// escapeLike neutralizes LIKE/ILIKE's wildcards so a user's literal "%" or "_"
// searches for that character instead of matching everything. Pair it with
// ESCAPE '\' on the LIKE.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
