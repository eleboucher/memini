// Package store defines the storage abstraction memini retrieves memories
// through. Drivers (sqlite-vec, Postgres/VectorChord) implement Store; the
// hybrid-search and service layers depend only on this interface.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/eleboucher/memini/internal/memory"
)

// ErrNotFound is returned by Get/Delete when no memory matches.
var ErrNotFound = errors.New("memory not found")

// ErrConflict is returned by Upsert when the given ID already exists in a
// different namespace, preventing cross-tenant hijacking.
var ErrConflict = errors.New("id exists in a different namespace")

// Scored is a memory paired with a relevance score for the query that produced
// it. Results are always returned best-first; Score is higher-is-better and is
// only comparable within a single method's result set.
type Scored struct {
	Memory *memory.Memory
	Score  float64
}

// Filter narrows a search to a subset of memories. The zero value matches all
// live (non-expired, non-superseded) memories in the namespace.
type Filter struct {
	// Tiers restricts results to these tiers; empty means all tiers.
	Tiers []memory.Tier
	// Levels restricts results to memories whose derivation level (explicit or
	// deduced) matches one of the listed values; empty means no level constraint.
	Levels []memory.Level
	// Tags restricts results to memories carrying every listed tag (AND). Empty
	// means no tag constraint.
	Tags []string
	// Metadata restricts results to memories whose top-level metadata contains
	// each listed key with the given string value (AND). Empty means no metadata
	// constraint. Only top-level string-valued entries are matched.
	Metadata map[string]string
	// ExcludeMetadata drops memories whose top-level metadata carries any of the
	// listed key=value pairs (the inverse of Metadata). Empty means no exclusion.
	// Used to keep a caller from recalling its own just-written memories — e.g.
	// the OpenClaw plugin tags each captured turn with its session id and excludes
	// that session on the pre-turn auto-recall, so a turn already in the live
	// transcript is not echoed back as "long-term memory".
	ExcludeMetadata map[string]string
	// IncludeExpired includes memories past their TTL (default excludes them).
	IncludeExpired bool
	// IncludeSuperseded includes contradiction-tombstoned memories.
	IncludeSuperseded bool
	// Now is the instant expiry is evaluated at; the zero value means the wall
	// clock. Callers with an injected clock (service.WithClock) should set it so
	// store-level expiry filtering agrees with their notion of "now".
	Now time.Time
	// AsOf, when non-zero, switches to time-travel recall: results are the
	// memories whose validity window contained AsOf (valid_from <= AsOf < valid_to,
	// treating NULL bounds as open). It overrides the superseded exclusion, so a
	// fact that was true then but has since been replaced is still returned.
	AsOf time.Time
	// MemoryTypes restricts results to memories whose top-level
	// metadata["memory_type"] equals one of the listed values (OR). Empty means
	// no constraint. Unlike Metadata (AND, one value per key), this expresses the
	// multi-select the UI's memory-type filter needs.
	MemoryTypes []string
	// CreatedAfter restricts results to memories created at or after the instant.
	// The zero value means no constraint.
	CreatedAfter time.Time
	// AccessedAfter restricts results to memories last accessed at or after the
	// instant. The zero value means no constraint.
	AccessedAfter time.Time
	// Sort orders the results. It is honored by List only: the search methods
	// return results best-first by relevance and ignore it.
	Sort Sort
}

// SortKey names a column List can order by. Values are the wire-level names the
// REST layer accepts, so an unknown key never reaches SQL — drivers map keys
// through a whitelist switch and fall back to SortCreatedAt.
type SortKey string

const (
	SortCreatedAt      SortKey = "created_at"
	SortUpdatedAt      SortKey = "updated_at"
	SortLastAccessedAt SortKey = "last_accessed_at"
	SortAccessCount    SortKey = "access_count"
	SortImportance     SortKey = "importance"
)

// Sort orders a listing. The zero value means created_at descending (newest
// first) — the order the UI browser wants by default, and the order the
// all-namespaces aggregate has always merged in.
type Sort struct {
	// Key is the column to order by; "" means SortCreatedAt.
	Key SortKey
	// Asc orders ascending; the zero value orders descending.
	Asc bool
}

// Store is the persistence and retrieval contract for memories. Implementations
// must be safe for concurrent use.
type Store interface {
	// Upsert inserts or replaces a memory (matched by ID within its namespace),
	// including its keyword-search index entry. When len(m.Embedding) == 0 the
	// row is stored with no vector-index entry — kept keyword-searchable only,
	// the write path used when embedding generation is unavailable; a stale
	// vector-index entry from a prior upsert of the same ID is removed. Any
	// other embedding length must equal the store's configured dims, or
	// Upsert errors. VectorSearch never returns a vectorless row. Returns
	// ErrConflict when the given ID already exists under a different namespace.
	Upsert(ctx context.Context, m *memory.Memory) error

	// Get returns a memory by ID, or ErrNotFound.
	Get(ctx context.Context, namespace, id string) (*memory.Memory, error)

	// PredecessorIDs returns the IDs of memories in the namespace whose
	// SupersededBy points at id — the versions id replaced. Empty when none.
	// Used to walk a memory's supersession lineage backwards.
	PredecessorIDs(ctx context.Context, namespace, id string) ([]string, error)

	// GetByFingerprint returns a live (non-superseded, non-expired) memory in the
	// namespace and tier whose content fingerprint (memory.Fingerprint) matches,
	// for exact-restatement dedup at write time. Returns ErrNotFound when none
	// matches. Now is the instant expiry is evaluated at (zero means wall clock).
	GetByFingerprint(ctx context.Context, namespace string, tier memory.Tier, fingerprint string, now time.Time) (*memory.Memory, error)

	// Delete removes a memory by ID, or returns ErrNotFound.
	Delete(ctx context.Context, namespace, id string) error

	// SetSuperseded tombstones a memory by recording the ID that replaced it,
	// excluding it from default search results. Returns ErrNotFound if missing.
	SetSuperseded(ctx context.Context, namespace, id, supersededBy string) error

	// Restore clears superseded_by/valid_to so a tombstoned memory is live
	// again. Returns ErrNotFound if missing.
	Restore(ctx context.Context, namespace, id string) error

	// VectorSearch returns the k memories nearest to vec, best-first.
	VectorSearch(ctx context.Context, namespace string, vec []float32, f Filter, k int) ([]Scored, error)

	// KeywordSearch returns the k best full-text matches for query, best-first.
	KeywordSearch(ctx context.Context, namespace, query string, f Filter, k int) ([]Scored, error)

	// Reinforce records that the given memories were just recalled: it bumps
	// access_count and last_accessed_at to accessedAt. When newExpiry is non-nil
	// it also slides the TTL forward for those that already expire (short-term
	// memories), so frequently-recalled memories don't go stale. Missing IDs are
	// ignored.
	Reinforce(ctx context.Context, namespace string, ids []string, accessedAt time.Time, newExpiry *time.Time) error

	// DeleteIfExpiredBefore removes a memory only when its expiry is still at or
	// before cutoff. Returns ErrNotFound when the memory was absent or when its
	// TTL was slid past cutoff by Reinforce, so the caller never over-deletes a
	// memory that was accessed between ListExpired and the delete call.
	DeleteIfExpiredBefore(ctx context.Context, namespace, id string, cutoff time.Time) error

	// ListExpired returns up to limit memories whose TTL has passed, for the
	// decay sweeper.
	ListExpired(ctx context.Context, now time.Time, limit int) ([]*memory.Memory, error)

	// List returns memories in a namespace matching f (without embeddings),
	// for maintenance tasks (short-term capacity, fsck). limit <= 0 means all.
	// Results are ordered by f.Sort — newest-created first by default — with
	// ties broken by id ascending, so a capped listing is deterministic.
	List(ctx context.Context, namespace string, f Filter, limit int) ([]*memory.Memory, error)

	// ListNamespaces returns the distinct namespaces that hold memories.
	ListNamespaces(ctx context.Context) ([]string, error)

	// DeleteNamespace removes every memory in a namespace (including embeddings
	// and keyword-search index entries). Returns the number of memories deleted.
	DeleteNamespace(ctx context.Context, namespace string) (int64, error)

	// Reassign moves the given memories from fromNS to toNS (recovery from a
	// botched import or a shared-pool migration). IDs absent from fromNS are
	// skipped; since IDs are globally unique a move never collides in toNS.
	// Returns the number of memories moved.
	Reassign(ctx context.Context, fromNS string, ids []string, toNS string) (int64, error)

	// Retier changes a memory's tier and expiry in place (used by retro-tiering
	// to demote stale durable memories). Tier lives only in the main row, so no
	// vector/keyword reindex is needed. Returns ErrNotFound if missing.
	Retier(ctx context.Context, namespace, id string, tier memory.Tier, expiresAt *time.Time) error

	// SetConfidence updates a memory's corroboration confidence in place and
	// bumps updated_at to now (resetting the lazy-decay baseline), used when a
	// durable fact is re-observed. Returns ErrNotFound if missing.
	SetConfidence(ctx context.Context, namespace, id string, confidence float64, now time.Time) error

	// MarkContradicted invalidates a durable fact a newer write contradicts: it
	// sets confidence, stamps valid_to=now (unless already set) so the fact
	// drops out of live recall while staying reachable via AsOf time-travel, and
	// records the contradicting id plus the pre-update confidence in metadata for
	// audit and reversal (Restore clears valid_to). Non-destructive: the row and
	// its history are kept. Bumps updated_at to now. Returns ErrNotFound if
	// missing.
	MarkContradicted(ctx context.Context, namespace, id, contradictedBy string, confidence float64, now time.Time) error

	// Ping verifies the backend is reachable, for readiness checks.
	Ping(ctx context.Context) error

	// Close releases backend resources.
	Close() error
}

// EmbedModelStore is implemented by drivers that record which embedding model
// produced their stored vectors. It lets the bootstrap detect a silent model
// swap — a new MEMINI_EMBED_MODEL at the same dimensionality, which the dims
// guard cannot catch — where old and new vectors share a width but live in
// incomparable spaces, quietly degrading recall.
type EmbedModelStore interface {
	// EmbedModel returns the recorded embedding model name, or "" when none has
	// been recorded yet (a fresh store, or one created before this was tracked).
	EmbedModel(ctx context.Context) (string, error)
	// SetEmbedModel records model as the embedding model the stored vectors were
	// produced with, overwriting any previous value.
	SetEmbedModel(ctx context.Context, model string) error
}

// NamespaceLink is a directed cross-namespace read link: recall scoped to Src
// additionally reads durable memories from Dst. Tiers restricts which tiers
// cross the boundary; nil means the service layer applies its durable-tier
// default (only semantic/procedural cross namespace boundaries — episodic and
// working never do, in either direction). Note is a free-text annotation for
// operators explaining why the link exists.
type NamespaceLink struct {
	Src, Dst  string
	Tiers     []memory.Tier // nil = durable default applied by service
	Note      string
	CreatedAt time.Time
}

// LinkStore is implemented by drivers that persist namespace_links, the
// cross-namespace read-linking table (namespace-cascade design). It is an
// optional capability interface — the EmbedModelStore precedent above — so
// callers type-assert and degrade gracefully against a driver that predates
// it.
type LinkStore interface {
	// PutLink inserts or replaces the link keyed by (l.Src, l.Dst). An
	// upsert overwrites created_at — deliberately, unlike memory Put's
	// preserve-on-upsert: links have no recency semantics, and import
	// restore relies on this to replay a link's original CreatedAt rather
	// than stamping "now".
	PutLink(ctx context.Context, l NamespaceLink) error
	// DeleteLink removes the link from src to dst. The bool reports whether a
	// link existed to delete; a missing link is not an error.
	DeleteLink(ctx context.Context, src, dst string) (bool, error)
	// ListLinks returns the links whose Src is src, ordered by Dst. Empty (not
	// an error) when src has no outgoing links.
	ListLinks(ctx context.Context, src string) ([]NamespaceLink, error)
	// ListAllLinks returns every link in the store, for the CLI/UI.
	ListAllLinks(ctx context.Context) ([]NamespaceLink, error)
	// RenameLinkEndpoints rewrites every link whose Src or Dst equals from to
	// to instead, used when a namespace is moved (maintenance.Move). When a
	// rewritten link collides with a pre-existing row at its new key, the
	// pre-existing row wins and the renamed link is dropped: the target
	// namespace's own explicit grant must never be silently widened or
	// narrowed by an inherited one. A no-op when from == to.
	RenameLinkEndpoints(ctx context.Context, from, to string) error
}

// NamespaceActivity summarizes one namespace's live activity: the count of
// live memories (excluding expired, superseded, and closed-validity rows —
// the same liveness a default List filter applies) and the most recent
// created_at among them (the "last write" column Stats.LastWriteAt uses;
// unlike Stats, tombstoned rows do not advance it, since a single aggregate
// query shares one liveness WHERE for both figures).
type NamespaceActivity struct {
	NS        string
	Total     int
	LastWrite time.Time
}

// ActivityStore is implemented by drivers that can compute per-namespace
// activity in a single aggregate query (SELECT namespace, COUNT(*),
// MAX(created_at) ... GROUP BY namespace). It is an optional capability
// interface — the EmbedModelStore/LinkStore precedent — so callers
// type-assert and degrade gracefully against a driver that predates it: the
// briefing child rollup skips itself entirely rather than falling back to
// per-namespace scans.
type ActivityStore interface {
	// NamespaceActivity returns one row per namespace holding at least one
	// live memory, ordered by namespace. now is the expiry-evaluation instant
	// (mirroring Filter.Now); the zero value means the wall clock.
	NamespaceActivity(ctx context.Context, now time.Time) ([]NamespaceActivity, error)
}

// APIKey is a persisted API credential: a unique human label, the hex
// SHA-256 hash of the secret (the secret itself is never stored), an
// optional home namespace it is bound to, when it was created, and whether
// it is disabled.
type APIKey struct {
	Name   string // unique human label, primary key
	Hash   string // hex SHA-256 of the secret; never the secret itself
	HomeNS string // bound home namespace; "" means unbound
	// DefaultNS is the namespace applied to a request that presents this key
	// but carries no X-Memini-Namespace header; an explicit header always
	// wins. "" means no per-key default (the server-wide default applies).
	DefaultNS string
	CreatedAt time.Time
	Disabled  bool
}

// APIKeyStore is implemented by drivers that persist api_keys, the
// multiple-API-keys-with-optional-home-namespace feature. It is an optional
// capability interface — the EmbedModelStore/LinkStore/ActivityStore
// precedent above — so callers type-assert and degrade gracefully against a
// driver that predates it. The auth middleware consumes GetAPIKeyByHash on
// the request path; the CLI consumes Put/Delete/List.
type APIKeyStore interface {
	// PutAPIKey inserts or replaces the key keyed by k.Name.
	//
	// Unlike PutLink (which deliberately overwrites created_at on every
	// upsert, since links carry no recency semantics — see its doc above),
	// this upsert preserves the existing row's CreatedAt when the incoming
	// k.CreatedAt is the zero value: API keys are long-lived identity, and
	// rotating a key's hash or home namespace must not reset "when was this
	// key first created". Passing a non-zero k.CreatedAt (e.g. import
	// restore replaying an original timestamp) still overwrites it, mirroring
	// PutLink's own conditional-overwrite convention.
	PutAPIKey(ctx context.Context, k APIKey) error
	// DeleteAPIKey removes the key by name. The bool reports whether a key
	// existed to delete; a missing key is not an error.
	DeleteAPIKey(ctx context.Context, name string) (bool, error)
	// ListAPIKeys returns every key ordered by name. Never returns secrets —
	// only the stored hash — since the plaintext secret is never persisted.
	ListAPIKeys(ctx context.Context) ([]APIKey, error)
	// GetAPIKeyByHash returns the key whose Hash matches, or nil, nil when
	// none does. This is the auth-path lookup, so drivers must maintain an
	// index/unique constraint on hash for it to stay fast.
	GetAPIKeyByHash(ctx context.Context, hash string) (*APIKey, error)
	// RenameAPIKeyNamespaces rewrites every key whose HomeNS or DefaultNS
	// equals from to to instead — both columns in one call, since a
	// namespace move (maintenance.Move, which also calls
	// RenameLinkEndpoints) must not leave either binding pointing at the
	// old name. Keys matching in neither column, and the non-matching
	// column of a partially-matching key, are untouched; CreatedAt is never
	// modified. A no-op when from == to.
	RenameAPIKeyNamespaces(ctx context.Context, from, to string) error
}

// EventKind names an operation the activity log records. Reads and writes are
// both logged; the kinds are the wire-level values the REST filter accepts.
type EventKind string

const (
	EventRecall    EventKind = "recall"
	EventGet       EventKind = "get"
	EventBriefing  EventKind = "briefing"
	EventRemember  EventKind = "remember"
	EventUpdate    EventKind = "update"
	EventForget    EventKind = "forget"
	EventSupersede EventKind = "supersede"
)

// ValidEventKind reports whether k is one of the recorded kinds, so the REST
// layer can reject an unknown filter value before it reaches SQL.
func ValidEventKind(k EventKind) bool {
	switch k {
	case EventRecall, EventGet, EventBriefing, EventRemember, EventUpdate, EventForget, EventSupersede:
		return true
	}
	return false
}

// Event is one (operation, memory) row of the activity log: what was served or
// written, when, and — for a recall — the query that pulled it and where it
// ranked. Memories served by one operation share an OpID, so a recall that
// returned five memories is five rows the reader regroups into one event.
//
// The memory fields are a snapshot taken at event time, not a live join: they
// keep the feed renderable in one query (no N+1 fetch per row) and keep a
// forget event readable after its memory is gone.
type Event struct {
	// ID is assigned by the store, monotonic within a driver.
	ID int64
	// OpID groups the rows of one logical operation.
	OpID string
	Kind EventKind
	// Namespace is the namespace the request was made against, which for a
	// cascading recall may differ from the served memory's own MemoryNS.
	Namespace string
	// Query is the recall query; "" for every other kind.
	Query string
	// MemoryID is "" for the sentinel row of a recall that returned nothing —
	// "the query found nothing" is itself worth recording.
	MemoryID      string
	MemoryNS      string
	MemoryTier    memory.Tier
	MemorySummary string
	// Rank is the 1-based position the memory was served at; 0 when not applicable.
	Rank int
	// Score is the composite relevance score the memory was served with; nil
	// when not applicable (every non-recall kind).
	Score *float64
	// Detail carries kind-specific context — a recall's degraded mode, a
	// briefing row's section, a supersession's replacement id.
	Detail    map[string]any
	CreatedAt time.Time
}

// EventFilter narrows an activity-log read. The zero value returns the newest
// events across every namespace and kind.
//
// Tiers and Text select whole operations, not individual rows: an event is a
// group of rows, so dropping some of a recall's rows would misreport what that
// recall actually served ("served 2 memories" when it served five). A matching
// operation is therefore returned intact, with every memory it touched.
type EventFilter struct {
	// Namespace restricts to events recorded against this namespace; "" means all.
	Namespace string
	// Namespaces restricts to events recorded against any of these namespaces
	// (OR); empty means no constraint. Used to narrow an all-namespaces feed;
	// ignored when Namespace is set.
	Namespaces []string
	// Kinds restricts to these kinds; empty means all.
	Kinds []EventKind
	// Tiers restricts to operations that touched a memory of one of these tiers
	// (OR); empty means no constraint.
	Tiers []memory.Tier
	// Text restricts to operations whose query or any served memory's summary
	// contains it, case-insensitively. Empty means no constraint.
	Text string
	// Since restricts to events recorded at or after the instant; zero means no
	// constraint.
	Since time.Time
	// Before and BeforeID are a keyset cursor: only rows strictly older than
	// (Before, BeforeID) in the (created_at DESC, id DESC) ordering are
	// returned. A zero Before starts from the newest row.
	Before   time.Time
	BeforeID int64
	// Limit caps the returned rows (not operations); <= 0 means no cap.
	Limit int
}

// EventLogStore is implemented by drivers that persist the activity log. It is
// an optional capability interface — the EmbedModelStore/LinkStore/APIKeyStore
// precedent — so callers type-assert and degrade gracefully: the service skips
// logging and the REST layer answers 501 against a driver that lacks it.
type EventLogStore interface {
	// AppendEvents inserts the rows of one operation as a single batch, so they
	// land contiguously. ListEvents' ordering then keeps an operation's rows
	// adjacent, which is what lets the reader regroup a flat row page into
	// whole events without a join table.
	AppendEvents(ctx context.Context, events []Event) error
	// ListEvents returns rows matching f, newest first (created_at DESC, id DESC).
	ListEvents(ctx context.Context, f EventFilter) ([]Event, error)
	// PruneEvents deletes rows older than olderThan (a zero olderThan prunes
	// none by age) and, when keepMax > 0, the oldest rows beyond the newest
	// keepMax. Returns the number of rows deleted.
	PruneEvents(ctx context.Context, olderThan time.Time, keepMax int) (int64, error)
}

// OrEmptyMap returns m, or an empty map when m is nil, so drivers persist an
// empty JSON object rather than null for absent metadata.
func OrEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// OrEmptySlice returns s, or an empty slice when s is nil, so drivers persist an
// empty JSON array rather than null for absent tags.
func OrEmptySlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
