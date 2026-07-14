package sqlitevec

import (
	"database/sql"
	"encoding/json"
	"fmt"
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
		level                      string
		created, updated, accessed int64
		expires                    sql.NullInt64
		superseded                 sql.NullString
		validFrom, validTo         sql.NullInt64
		confidence                 sql.NullFloat64
		linkedJSON                 string
	)
	dest := []any{
		&m.ID, &m.Namespace, &tier, &m.Content, &m.Summary, &metaJSON, &tagsJSON,
		&m.Importance, &created, &updated, &accessed, &m.AccessCount, &expires, &superseded,
		&validFrom, &validTo, &confidence, &level, &linkedJSON,
	}
	if metric != nil {
		dest = append(dest, metric)
	}
	if err := s.Scan(dest...); err != nil {
		return nil, err
	}

	m.Tier = memory.Tier(tier)
	m.Level = memory.Level(level)
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
	if err := json.Unmarshal([]byte(linkedJSON), &m.LinkedMemoryIDs); err != nil {
		return nil, err
	}
	return &m, nil
}

// filterClause builds the SQL suffix and args that restrict a search to the
// subset described by f. Every query aliases the memories table as "m", so
// the alias is a constant here rather than a parameter.
func filterClause(f store.Filter) (string, []any) {
	const alias = "m"
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
	// Level: mirror the tiers pattern — bind-safe placeholders for each level.
	if len(f.Levels) > 0 {
		b.WriteString(" AND " + alias + ".level IN (")
		for i, l := range f.Levels {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("?")
			args = append(args, string(l))
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
	// ExcludeMetadata: drop rows carrying any of these key=value pairs (inverse of
	// Metadata), e.g. excluding a session's own captures from its auto-recall.
	for k, v := range f.ExcludeMetadata {
		b.WriteString(" AND NOT EXISTS (SELECT 1 FROM json_each(" + alias + ".metadata) WHERE key = ? AND value = ?)")
		args = append(args, k, v)
	}
	// MemoryTypes: metadata.memory_type matching ANY listed value (OR), unlike
	// Metadata's AND-with-one-value-per-key.
	if len(f.MemoryTypes) > 0 {
		b.WriteString(" AND EXISTS (SELECT 1 FROM json_each(" + alias + ".metadata) WHERE key = 'memory_type' AND value IN (")
		for i, t := range f.MemoryTypes {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("?")
			args = append(args, t)
		}
		b.WriteString("))")
	}
	if !f.CreatedAfter.IsZero() {
		b.WriteString(" AND " + alias + ".created_at >= ?")
		args = append(args, ms(f.CreatedAfter))
	}
	if !f.AccessedAfter.IsZero() {
		b.WriteString(" AND " + alias + ".last_accessed_at >= ?")
		args = append(args, ms(f.AccessedAfter))
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
		// A fact whose validity window has closed (valid_to in the past) is no
		// longer current: drop it from live recall while AsOf and
		// IncludeSuperseded can still reach it. This is how a contradiction
		// invalidates the superseded fact without deleting it.
		b.WriteString(" AND (" + alias + ".valid_to IS NULL OR " + alias + ".valid_to > ?)")
		args = append(args, ref.UnixMilli())
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

// tiersOrEmpty returns tiers, or an empty slice when tiers is nil, so
// PutLink persists a JSON array ('[]') rather than a JSON null for an
// unrestricted (service-default) link.
func tiersOrEmpty(tiers []memory.Tier) []memory.Tier {
	if tiers == nil {
		return []memory.Tier{}
	}
	return tiers
}

// scanLinks reads every remaining row of rows into NamespaceLinks, closing
// rows before returning.
func scanLinks(rows *sql.Rows) ([]store.NamespaceLink, error) {
	defer func() { _ = rows.Close() }()
	var out []store.NamespaceLink
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// scanLink scans a single (src_ns, dst_ns, tiers, note, created_at) row.
func scanLink(s scanner) (store.NamespaceLink, error) {
	var l store.NamespaceLink
	var tiersJSON, created string
	if err := s.Scan(&l.Src, &l.Dst, &tiersJSON, &l.Note, &created); err != nil {
		return l, err
	}
	if err := json.Unmarshal([]byte(tiersJSON), &l.Tiers); err != nil {
		return l, fmt.Errorf("sqlitevec: unmarshal link tiers: %w", err)
	}
	t, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return l, fmt.Errorf("sqlitevec: parse link created_at: %w", err)
	}
	l.CreatedAt = t.UTC()
	return l, nil
}

// scanAPIKeys reads every remaining row of rows into APIKeys, closing rows
// before returning.
func scanAPIKeys(rows *sql.Rows) ([]store.APIKey, error) {
	defer func() { _ = rows.Close() }()
	var out []store.APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// scanAPIKey scans a single (name, key_hash, home_ns, default_ns,
// created_at, disabled, settings, admin) row.
func scanAPIKey(s scanner) (store.APIKey, error) {
	var k store.APIKey
	var created, settingsJSON string
	var disabled, admin int
	if err := s.Scan(&k.Name, &k.Hash, &k.HomeNS, &k.DefaultNS, &created, &disabled, &settingsJSON, &admin); err != nil {
		return k, err
	}
	t, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return k, fmt.Errorf("sqlitevec: parse api key created_at: %w", err)
	}
	k.CreatedAt = t.UTC()
	k.Disabled = disabled != 0
	k.Admin = admin != 0
	// Tolerant decode: unknown fields in an older/newer writer's blob are
	// ignored (json.Unmarshal's default behavior) — strict validation is the
	// REST boundary's job, not the store's.
	if err := json.Unmarshal([]byte(settingsJSON), &k.Settings); err != nil {
		return k, fmt.Errorf("sqlitevec: unmarshal api key settings: %w", err)
	}
	return k, nil
}

// boolToInt converts a Go bool to the 0/1 SQLite stores its INTEGER boolean
// columns as (e.g. api_keys.disabled).
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
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

// escapeLike neutralizes LIKE's wildcards so a user's literal "%" or "_"
// searches for that character instead of matching everything. Pair it with
// ESCAPE '\' on the LIKE.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// orderClause maps a sort onto an ORDER BY over the memories table aliased as
// "m". The column comes from a whitelist switch, never from the caller's
// string, so an unrecognized key degrades to created_at rather than reaching
// SQL. Ties break on id so a capped listing is deterministic.
func orderClause(s store.Sort) string {
	col := "m.created_at"
	switch s.Key {
	case store.SortUpdatedAt:
		col = "m.updated_at"
	case store.SortLastAccessedAt:
		col = "m.last_accessed_at"
	case store.SortAccessCount:
		col = "m.access_count"
	case store.SortImportance:
		col = "m.importance"
	}
	dir := "DESC"
	if s.Asc {
		dir = "ASC"
	}
	return " ORDER BY " + col + " " + dir + ", m.id ASC"
}
