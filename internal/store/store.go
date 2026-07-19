// Package store defines the storage abstraction memini retrieves memories
// through. Drivers (sqlite-vec, Postgres/VectorChord) implement Store; the
// hybrid-search and service layers depend only on this interface.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/memory"
)

// ErrNotFound is returned by Get/Delete when no memory matches.
var ErrNotFound = errors.New("memory not found")

// ErrConflict is returned by Upsert when the given ID already exists in a
// different namespace, preventing cross-namespace hijacking.
var ErrConflict = errors.New("id exists in a different namespace")

// Scored is a memory paired with a relevance score for the query that produced
// it. Results are always returned best-first; Score is higher-is-better and is
// only comparable within a single method's result set.
type Scored struct {
	Memory *memory.Memory
	Score  float64
	// MatchedChunk is the text of the chunk that produced this hit, set only by
	// ChunkVectorSearch and empty for every other search. Callers that rerank
	// should judge this rather than the whole memory when it is set: it is the
	// passage that actually matched, and a reranker handed the whole memory sees
	// only a prefix that need not contain it.
	MatchedChunk string
}

// WithScore returns the hit with only its score changed. Score adjusters must
// go through here rather than rebuilding the struct, so fields they do not
// know about (MatchedChunk today) survive them.
func (s Scored) WithScore(v float64) Scored {
	s.Score = v
	return s
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
	// ExcludeIDs drops memories with the listed ids. Applied in SQL, before
	// ranking and the caller's limit, so an excluded hit never consumes a
	// result slot — a client that filters already-injected ids after the fact
	// would instead lose those slots. Empty means no exclusion.
	ExcludeIDs []string
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

	// IDsByPrefix returns the IDs in the namespace that begin with prefix,
	// ordered ascending and bounded at limit rows — an indexed prefix scan
	// backing short-id resolution (service.Get accepts an 8+ hex-char id
	// prefix). LIKE/glob metacharacters in prefix match literally, never as
	// wildcards. An empty prefix or non-positive limit returns no rows: the
	// caller is resolving a specific handle, never enumerating a namespace.
	IDsByPrefix(ctx context.Context, namespace, prefix string, limit int) ([]string, error)

	// GetEmbedding returns the stored vector for a memory, for a write that must
	// preserve a vector it is not recomputing (see service.Remember's reuse of
	// the stored vector when an update leaves content unchanged). Returns
	// (nil, nil) when the row exists but is vectorless — a degraded write
	// awaiting backfill — and ErrNotFound when the memory is absent.
	//
	// Kept off Get deliberately: a vector is dims*4 bytes and Get/List/search run
	// on every read path, so Memory.Embedding is left empty there (see
	// memory.Memory.Embedding). That makes a Get-then-Upsert round trip lossy for
	// the vector, which is why a write that skips embedding must read it back
	// through here rather than off the Memory it just loaded.
	GetEmbedding(ctx context.Context, namespace, id string) ([]float32, error)

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

// ChunkStore is the optional chunked-embedding capability: per-segment vectors
// for content that runs past the per-item embed budget, so recall can match
// text the document vector does not cover.
//
// It is a SEPARATE capability rather than a change to VectorSearch, and that
// separation is load-bearing. VectorSearch has six callers and three of them
// destroy data: write-dedup (coalesce/supersede tombstones the loser),
// contradiction routing (closes a fact's validity window), and the maintenance
// dedup sweep (tombstones). Chunk similarity is max-pooled, which makes a long
// memory a near-duplicate of anything matching any one of its paragraphs — so
// pooling inside VectorSearch would have those three tombstone unrelated
// memories. VectorSearch's semantics are therefore frozen, and recall unions
// the two legs instead. Chunks are purely additive: they can only add hits,
// never remove or rewrite a memory.
//
// Because the document vector stays authoritative, a store that implements this
// needs no migration to be correct, and dropping back to a build without it
// loses only the extra recall.
type ChunkStore interface {
	// ChunkVectorSearch returns the k memories whose best-matching chunk is
	// nearest to vec, best-first, one row per memory (max-pooled over its
	// chunks). Scores are in the same space as VectorSearch's, so a caller can
	// compare the two legs directly — which matters because the recall gates
	// (semantic floor, min-score) are absolute thresholds, not ranks.
	//
	// Filter applies to the memories, not the chunks. Rows whose memory is
	// filtered out never appear.
	ChunkVectorSearch(ctx context.Context, namespace string, vec []float32, f Filter, k int) ([]Scored, error)

	// ListUnchunked returns up to limit memories in the namespace whose
	// content exceeds minRunes but which have no chunk rows — the backfill's
	// work queue — ordered by id, starting after afterID ("" starts from the
	// beginning). The cursor lets the backfill page past rows it cannot
	// process this tick (declined by the splitter, rejected by the embedder)
	// instead of re-listing them into every batch until they starve it.
	// Namespace "" means every namespace.
	//
	// Expired and superseded rows are returned too, deliberately: recall's
	// AsOf and IncludeSuperseded modes flow through the chunk leg, and
	// tombstones are retained precisely so time-travel queries can reach them.
	//
	// This is a query rather than a metadata flag because rows that predate
	// chunking carry no flag to find them by, and adding one would mean
	// rewriting every row before the feature could do anything.
	ListUnchunked(ctx context.Context, namespace string, minRunes int, afterID string, limit int) ([]*memory.Memory, error)

	// CountUnchunked reports the total size of ListUnchunked's queue — the
	// real backlog, where ListUnchunked shows at most one batch of it.
	CountUnchunked(ctx context.Context, namespace string, minRunes int) (int, error)

	// PutChunks replaces the chunk rows of the memory identified by
	// (namespace, id), guarded by updatedAt: the write happens only when the
	// row still exists with exactly that updated_at, and reports false
	// otherwise. Guard and write are one transaction, so a concurrent content
	// change cannot slip between them — the check-then-act race a re-read
	// outside the store can never close. Nothing else on the row is touched:
	// not the document vector, not the FTS index, and not the columns a
	// concurrent Reinforce may be bumping (Reinforce deliberately does not
	// advance updated_at, and this guard must not punish it).
	PutChunks(ctx context.Context, namespace, id string, updatedAt time.Time, chunks []memory.Chunk) (bool, error)

	// CountChunks reports how many chunk rows exist in the namespace (""
	// counts every namespace). It exists so tests and operators can see
	// orphaned rows that ChunkVectorSearch's join back to memories
	// structurally hides; on backends without referential cascades, orphans
	// (which belong to no namespace) are included in every count.
	CountChunks(ctx context.Context, namespace string) (int, error)
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

// ClientSettings is the behavioral/injection configuration surface a caller's
// effective settings resolve to: a layered merge of the built-in defaults
// (DefaultClientSettings), the server's global defaults (ClientSettingsStore),
// and a per-API-key override (APIKey.Settings, below). Every field is a
// pointer; nil means "unset — inherit from the next layer down". Only
// MergeClientSettings' result is guaranteed to have every field non-nil.
//
// The field set, JSON tags (the wire-level snake_case keys), and defaults are
// exactly the ClientSettings schema in api/openapi.yaml (config-handshake
// redesign) — keep the two in sync; the schema wins on any disagreement.
type ClientSettings struct {
	// CaptureTurns enables capturing each user→assistant turn as episodic memory.
	CaptureTurns *bool `json:"capture_turns,omitempty"`
	// SessionDigest enables recording a session-end/stop/pre-compact digest memory.
	SessionDigest *bool `json:"session_digest,omitempty"`
	// InlineExtract enables injecting the directive asking the agent to save
	// durable facts via memory_remember.
	InlineExtract *bool `json:"inline_extract,omitempty"`
	// AutoSave enables periodically nudging the agent to persist durable memories.
	AutoSave *bool `json:"auto_save,omitempty"`
	// AutoSaveInterval is the user-message interval between auto-save nudges;
	// must be >= 1.
	AutoSaveInterval *int `json:"auto_save_interval,omitempty"`
	// AutoSaveMinEvents is the minimum number of buffered state-changing tool
	// events since the last auto-save baseline for the interval nudge to fire;
	// 0 disables the activity gate (interval-only cadence). Must be >= 0.
	AutoSaveMinEvents *int `json:"auto_save_min_events,omitempty"`

	// InjectBriefingPinned caps pinned memories in the session-start briefing.
	InjectBriefingPinned *int `json:"inject_briefing_pinned,omitempty"`
	// InjectBriefingFacts caps durable semantic facts in the session-start briefing.
	InjectBriefingFacts *int `json:"inject_briefing_facts,omitempty"`
	// InjectBriefingProcedures caps procedural how-tos in the session-start briefing.
	InjectBriefingProcedures *int `json:"inject_briefing_procedures,omitempty"`
	// InjectBriefingRecent caps recent episodic entries in the session-start briefing.
	InjectBriefingRecent *int `json:"inject_briefing_recent,omitempty"`
	// InjectBriefingMaxTok is a hard ceiling on briefing injection tokens; 0 is uncapped.
	InjectBriefingMaxTok *int `json:"inject_briefing_max_tok,omitempty"`

	// InjectPretoolItems caps recalled items injected per file on PreToolUse.
	InjectPretoolItems *int `json:"inject_pretool_items,omitempty"`
	// InjectPretoolMaxTok is a hard ceiling on per-tool injection tokens; 0 is uncapped.
	InjectPretoolMaxTok *int `json:"inject_pretool_max_tok,omitempty"`
	// InjectPretoolMinScore floors the final ranked (composite) score for a
	// PreToolUse injection — the score shown in the activity feed. All bundled
	// integrations enforce it server-side via min_rank_score (floored hits
	// appear in the feed marked as filtered); a custom or older caller may
	// still apply it as a pre-rank fused-score floor.
	InjectPretoolMinScore *float64 `json:"inject_pretool_min_score,omitempty"`
	// InjectPretoolTools is the tool-name allowlist that triggers a PreToolUse injection.
	InjectPretoolTools *[]string `json:"inject_pretool_tools,omitempty"`
	// InjectPretoolGateMs skips the PreToolUse recall server call entirely for
	// a file whose last call was younger than this many milliseconds; 0 always
	// calls. Must be >= 0.
	InjectPretoolGateMs *int `json:"inject_pretool_gate_ms,omitempty"`

	// InjectDedupe suppresses re-injecting a recalled memory still within its
	// injection cooldown window (InjectCooldownMs / InjectCooldownPrompts);
	// with both windows 0 an unchanged memory stays suppressed for the rest of
	// the session. It gates the injection; whether the PreToolUse recall call
	// runs follows InjectPretoolGateMs, but turning InjectDedupe off also
	// disables that call gate, because the gate's clock lives in the dedupe
	// state.
	InjectDedupe *bool `json:"inject_dedupe,omitempty"`
	// InjectTelemetry reports what each hook actually injected vs suppressed
	// back to the server (POST /v1/activity/injected) so the activity feed and
	// metrics reflect reality instead of pre-suppression serves. Best-effort
	// and bounded; off disables the beacon entirely.
	InjectTelemetry *bool `json:"inject_telemetry,omitempty"`
	// InjectCooldownMs is the time window (ms) within which an already-injected
	// memory is not re-injected; 0 disables the time dimension. Must be >= 0.
	InjectCooldownMs *int `json:"inject_cooldown_ms,omitempty"`
	// InjectCooldownPrompts is the prompt-count window within which an
	// already-injected memory is not re-injected; 0 disables the prompt
	// dimension. Must be >= 0.
	InjectCooldownPrompts *int `json:"inject_cooldown_prompts,omitempty"`

	// InjectLabels selects which annotation labels to render alongside an
	// injected memory; each must be one of tier, confidence, age, reason.
	InjectLabels *[]string `json:"inject_labels,omitempty"`

	// Recall enables recall-driven injection at all.
	Recall *bool `json:"recall,omitempty"`
	// Capture enables capture (turns/digests) at all.
	Capture *bool `json:"capture,omitempty"`
	// RecallLimit caps memories per recall call.
	RecallLimit *int `json:"recall_limit,omitempty"`

	// InjectRecallMaxTok is a hard ceiling on recall injection tokens; 0 is uncapped.
	InjectRecallMaxTok *int `json:"inject_recall_max_tok,omitempty"`
	// InjectRecallMinScore floors the final ranked (composite) score for a
	// recall injection — the score shown in the activity feed. All bundled
	// integrations enforce it server-side via min_rank_score (floored hits
	// appear in the feed marked as filtered); a custom or older caller may
	// still apply it as a pre-rank fused-score floor.
	InjectRecallMinScore *float64 `json:"inject_recall_min_score,omitempty"`

	// MinCaptureChars is the minimum content length worth bothering to capture a turn.
	MinCaptureChars *int `json:"min_capture_chars,omitempty"`
	// CaptureUserMaxChars truncates the user side of a captured turn to this
	// many characters, marking the cut; 0 captures it whole.
	CaptureUserMaxChars *int `json:"capture_user_max_chars,omitempty"`
	// CaptureAssistantMaxChars truncates the assistant side of a captured turn
	// to this many characters, marking the cut; 0 captures it whole.
	CaptureAssistantMaxChars *int `json:"capture_assistant_max_chars,omitempty"`
	// RequestTimeoutMs is how long a client waits on one memini HTTP call before
	// giving up; it must stay above the server's own RerankTimeout, or a slow
	// reranker returns nothing at all instead of the composite-order fallback
	// finalizeRecall degrades to. Must be >= 100.
	RequestTimeoutMs *int `json:"request_timeout_ms,omitempty"`
	// NamespaceScope is "repo" or "owner_repo": "repo" derives the namespace
	// from the bare repo name; "owner_repo" disambiguates same-named repos
	// across owners with an owner-repo slug (owner + "-" + repo).
	NamespaceScope *string `json:"namespace_scope,omitempty"`
	// NamespacePrefix is a namespace path prepended ahead of the
	// derived/declared namespace; "" means no prefix.
	NamespacePrefix *string `json:"namespace_prefix,omitempty"`
}

// Validate returns a non-nil error when a set (non-nil) field violates the
// range/enum constraints the ClientSettings schema in api/openapi.yaml
// declares. Unset (nil) fields are never checked — validation is purely
// per-field, so a partial layer (e.g. one key's override) validates
// independently of any other layer.
//
// namespace_prefix's check calls httputil.ValidateNamespace directly rather
// than accepting an injected validator func: internal/httputil has no
// dependency on internal/store (it only imports the standard library), so
// importing it here does not create an import cycle — confirmed by reading
// internal/httputil/httputil.go before wiring this up.
func (s ClientSettings) Validate() error {
	if s.AutoSaveInterval != nil && *s.AutoSaveInterval < 1 {
		return fmt.Errorf("client settings: auto_save_interval must be >= 1, got %d", *s.AutoSaveInterval)
	}
	// 0 is deliberately NOT overloaded as "no timeout": a client with no timeout
	// hangs forever on a wedged server instead of failing soft, which is the one
	// outcome every hook in this repo is written to avoid.
	if s.RequestTimeoutMs != nil && *s.RequestTimeoutMs < 100 {
		return fmt.Errorf("client settings: request_timeout_ms must be >= 100, got %d", *s.RequestTimeoutMs)
	}
	nonNegativeInts := []struct {
		key string
		v   *int
	}{
		{"auto_save_min_events", s.AutoSaveMinEvents},
		{"inject_briefing_pinned", s.InjectBriefingPinned},
		{"inject_briefing_facts", s.InjectBriefingFacts},
		{"inject_briefing_procedures", s.InjectBriefingProcedures},
		{"inject_briefing_recent", s.InjectBriefingRecent},
		{"inject_briefing_max_tok", s.InjectBriefingMaxTok},
		{"inject_pretool_items", s.InjectPretoolItems},
		{"inject_pretool_max_tok", s.InjectPretoolMaxTok},
		{"inject_pretool_gate_ms", s.InjectPretoolGateMs},
		{"inject_cooldown_ms", s.InjectCooldownMs},
		{"inject_cooldown_prompts", s.InjectCooldownPrompts},
		{"recall_limit", s.RecallLimit},
		{"inject_recall_max_tok", s.InjectRecallMaxTok},
		{"min_capture_chars", s.MinCaptureChars},
		{"capture_user_max_chars", s.CaptureUserMaxChars},
		{"capture_assistant_max_chars", s.CaptureAssistantMaxChars},
	}
	// request_timeout_ms is checked separately: its floor is 100, not 0.
	for _, f := range nonNegativeInts {
		if f.v != nil && *f.v < 0 {
			return fmt.Errorf("client settings: %s must be >= 0, got %d", f.key, *f.v)
		}
	}
	nonNegativeFloats := []struct {
		key string
		v   *float64
	}{
		{"inject_pretool_min_score", s.InjectPretoolMinScore},
		{"inject_recall_min_score", s.InjectRecallMinScore},
	}
	for _, f := range nonNegativeFloats {
		if f.v != nil && *f.v < 0 {
			return fmt.Errorf("client settings: %s must be >= 0, got %g", f.key, *f.v)
		}
	}
	if s.NamespaceScope != nil {
		switch *s.NamespaceScope {
		case "repo", "owner_repo":
		default:
			return fmt.Errorf("client settings: namespace_scope must be %q or %q, got %q",
				"repo", "owner_repo", *s.NamespaceScope)
		}
	}
	if s.InjectLabels != nil {
		for _, label := range *s.InjectLabels {
			switch label {
			case "tier", "confidence", "age", "reason":
			default:
				return fmt.Errorf("client settings: inject_labels value must be one of "+
					"tier, confidence, age, reason, got %q", label)
			}
		}
	}
	if s.NamespacePrefix != nil && *s.NamespacePrefix != "" {
		if err := httputil.ValidateNamespace(*s.NamespacePrefix); err != nil {
			return fmt.Errorf("client settings: namespace_prefix: %w", err)
		}
	}
	// InjectPretoolTools is a tool-name allowlist; unmatched names are inert, so
	// this is a bounded-DoS guard, not a correctness one — a caller must not be
	// able to persist an oversized blob into its own row up to the decode cap.
	if s.InjectPretoolTools != nil {
		if n := len(*s.InjectPretoolTools); n > 64 {
			return fmt.Errorf("client settings: inject_pretool_tools may list at most 64 tools, got %d", n)
		}
		for _, name := range *s.InjectPretoolTools {
			if len(name) > 128 {
				return fmt.Errorf("client settings: inject_pretool_tools value must be <= 128 chars, got %d", len(name))
			}
		}
	}
	return nil
}

// DefaultClientSettings returns the built-in default ClientSettings, every
// field set from the ClientSettings schema's `default:` in api/openapi.yaml.
// It is the bottom layer of MergeClientSettings — passing it first guarantees
// the merge result has every field non-nil even when every other layer is
// empty.
func DefaultClientSettings() ClientSettings {
	return ClientSettings{
		CaptureTurns:  new(true),
		SessionDigest: new(true),
		InlineExtract: new(true),
		AutoSave:      new(true),

		AutoSaveInterval:  new(10),
		AutoSaveMinEvents: new(3),

		InjectBriefingPinned:     new(5),
		InjectBriefingFacts:      new(5),
		InjectBriefingProcedures: new(5),
		InjectBriefingRecent:     new(3),
		InjectBriefingMaxTok:     new(600),

		InjectPretoolItems:    new(3),
		InjectPretoolMaxTok:   new(200),
		InjectPretoolMinScore: new(float64(0)),
		InjectPretoolTools:    &[]string{"Read", "Write", "Edit", "MultiEdit", "Glob", "Grep"},
		InjectPretoolGateMs:   new(90000),
		InjectDedupe:          new(true),
		InjectTelemetry:       new(true),
		InjectCooldownMs:      new(1800000),
		InjectCooldownPrompts: new(3),

		InjectLabels: &[]string{},

		Recall:      new(true),
		Capture:     new(true),
		RecallLimit: new(3),

		InjectRecallMaxTok:   new(250),
		InjectRecallMinScore: new(float64(0)),

		MinCaptureChars:          new(0),
		CaptureUserMaxChars:      new(1000),
		CaptureAssistantMaxChars: new(3000),
		// Above the server's 10s default RerankTimeout, with room for the 250ms
		// response margin, the query embed and HTTP overhead — and already what
		// MEMINI_TIMEOUT_MS means in the pi/opencode integrations, so the knob
		// reads the same everywhere. A ceiling, not a target: the server degrades
		// to composite order at its own deadline, so a healthy client never waits
		// this long.
		RequestTimeoutMs: new(30000),
		NamespaceScope:   new("repo"),
		NamespacePrefix:  new(""),
	}
}

// SettingsLayer is one input to MergeClientSettings: a (possibly partial)
// ClientSettings plus a human-readable label for where it came from (e.g.
// "default", "global", "key:ci-bot"). Source is echoed back in
// MergeClientSettings' provenance map for whichever fields this layer wins.
type SettingsLayer struct {
	Source string
	S      ClientSettings
}

// MergeClientSettings flattens layers into one ClientSettings: later layers
// win field-by-field — a nil field never overrides, only an explicitly-set
// field in a later layer replaces an earlier one. Passing
// SettingsLayer{Source: "default", S: DefaultClientSettings()} first
// guarantees every field of the result is non-nil regardless of what (if
// anything) later layers set.
//
// The second return maps each wire-key (the JSON tag) to the Source label of
// whichever layer's value it took, for the /v1/self settings_sources
// provenance the REST layer (a later phase) surfaces to callers.
//
// gocyclo is silenced: the body is a flat per-field applyPtr enumeration
// (one branch per ClientSettings field), not genuinely complex control flow.
func MergeClientSettings(layers ...SettingsLayer) (ClientSettings, map[string]string) { //nolint:gocyclo
	var out ClientSettings
	sources := make(map[string]string, 29)

	for _, l := range layers {
		s := l.S
		if applyPtr(&out.CaptureTurns, s.CaptureTurns) {
			sources["capture_turns"] = l.Source
		}
		if applyPtr(&out.SessionDigest, s.SessionDigest) {
			sources["session_digest"] = l.Source
		}
		if applyPtr(&out.InlineExtract, s.InlineExtract) {
			sources["inline_extract"] = l.Source
		}
		if applyPtr(&out.AutoSave, s.AutoSave) {
			sources["auto_save"] = l.Source
		}
		if applyPtr(&out.AutoSaveInterval, s.AutoSaveInterval) {
			sources["auto_save_interval"] = l.Source
		}
		if applyPtr(&out.AutoSaveMinEvents, s.AutoSaveMinEvents) {
			sources["auto_save_min_events"] = l.Source
		}
		if applyPtr(&out.InjectBriefingPinned, s.InjectBriefingPinned) {
			sources["inject_briefing_pinned"] = l.Source
		}
		if applyPtr(&out.InjectBriefingFacts, s.InjectBriefingFacts) {
			sources["inject_briefing_facts"] = l.Source
		}
		if applyPtr(&out.InjectBriefingProcedures, s.InjectBriefingProcedures) {
			sources["inject_briefing_procedures"] = l.Source
		}
		if applyPtr(&out.InjectBriefingRecent, s.InjectBriefingRecent) {
			sources["inject_briefing_recent"] = l.Source
		}
		if applyPtr(&out.InjectBriefingMaxTok, s.InjectBriefingMaxTok) {
			sources["inject_briefing_max_tok"] = l.Source
		}
		if applyPtr(&out.InjectPretoolItems, s.InjectPretoolItems) {
			sources["inject_pretool_items"] = l.Source
		}
		if applyPtr(&out.InjectPretoolMaxTok, s.InjectPretoolMaxTok) {
			sources["inject_pretool_max_tok"] = l.Source
		}
		if applyPtr(&out.InjectPretoolMinScore, s.InjectPretoolMinScore) {
			sources["inject_pretool_min_score"] = l.Source
		}
		if applyPtr(&out.InjectPretoolTools, s.InjectPretoolTools) {
			sources["inject_pretool_tools"] = l.Source
		}
		if applyPtr(&out.InjectPretoolGateMs, s.InjectPretoolGateMs) {
			sources["inject_pretool_gate_ms"] = l.Source
		}
		if applyPtr(&out.InjectTelemetry, s.InjectTelemetry) {
			sources["inject_telemetry"] = l.Source
		}
		if applyPtr(&out.InjectDedupe, s.InjectDedupe) {
			sources["inject_dedupe"] = l.Source
		}
		if applyPtr(&out.InjectCooldownMs, s.InjectCooldownMs) {
			sources["inject_cooldown_ms"] = l.Source
		}
		if applyPtr(&out.InjectCooldownPrompts, s.InjectCooldownPrompts) {
			sources["inject_cooldown_prompts"] = l.Source
		}
		if applyPtr(&out.InjectLabels, s.InjectLabels) {
			sources["inject_labels"] = l.Source
		}
		if applyPtr(&out.Recall, s.Recall) {
			sources["recall"] = l.Source
		}
		if applyPtr(&out.Capture, s.Capture) {
			sources["capture"] = l.Source
		}
		if applyPtr(&out.RecallLimit, s.RecallLimit) {
			sources["recall_limit"] = l.Source
		}
		if applyPtr(&out.InjectRecallMaxTok, s.InjectRecallMaxTok) {
			sources["inject_recall_max_tok"] = l.Source
		}
		if applyPtr(&out.InjectRecallMinScore, s.InjectRecallMinScore) {
			sources["inject_recall_min_score"] = l.Source
		}
		if applyPtr(&out.MinCaptureChars, s.MinCaptureChars) {
			sources["min_capture_chars"] = l.Source
		}
		if applyPtr(&out.CaptureUserMaxChars, s.CaptureUserMaxChars) {
			sources["capture_user_max_chars"] = l.Source
		}
		if applyPtr(&out.CaptureAssistantMaxChars, s.CaptureAssistantMaxChars) {
			sources["capture_assistant_max_chars"] = l.Source
		}
		if applyPtr(&out.RequestTimeoutMs, s.RequestTimeoutMs) {
			sources["request_timeout_ms"] = l.Source
		}
		if applyPtr(&out.NamespaceScope, s.NamespaceScope) {
			sources["namespace_scope"] = l.Source
		}
		if applyPtr(&out.NamespacePrefix, s.NamespacePrefix) {
			sources["namespace_prefix"] = l.Source
		}
	}
	return out, sources
}

// applyPtr copies src into *dst when src is non-nil ("explicitly set"),
// reporting whether it did. Shared by MergeClientSettings for every
// ClientSettings field regardless of pointee type.
func applyPtr[T any](dst **T, src *T) bool {
	if src == nil {
		return false
	}
	*dst = src
	return true
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
	// Admin marks this key as an admin credential, the sibling bool to
	// Disabled above. It will grant access to the /v1/keys and
	// /v1/settings/defaults REST surfaces — the gating itself happens at the
	// REST layer (a later change), not here; this field only carries the
	// capability through storage and auth. Preserved by rotation, same as
	// every other field CLI/REST rotation does not explicitly change.
	Admin bool
	// Settings is this key's per-key ClientSettings override, merged over the
	// server's global defaults (ClientSettingsStore) and the built-in
	// defaults (DefaultClientSettings) by MergeClientSettings. The zero value
	// (every field nil) means no override at all.
	Settings ClientSettings
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

// Pin is a persisted project→namespace binding: an operator-created
// binding from a project's identity to the namespace every handshake for
// that project resolves to, overriding derivation. Key is the lookup key:
// "remote:<canonical-remote>" for a git-remote pin (canonical = normalized,
// credential-stripped) or "path:<absolute-toplevel>" for a path pin (used for
// remoteless repos and bare directories).
type Pin struct {
	Key       string
	Namespace string
	Note      string
	// CreatedBy is the API key name that created the pin; "" for the admin
	// key or dev mode, which carry no named principal.
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PinStore is implemented by drivers that persist pins, the
// config-handshake redesign's project→namespace pin table. It is an optional
// capability interface — the EmbedModelStore/LinkStore/APIKeyStore precedent
// above — so callers type-assert and degrade gracefully against a driver
// that predates it.
type PinStore interface {
	// PutPins upserts entries in a single transaction, keyed by
	// each entry's Key. An update preserves the existing row's CreatedAt and
	// CreatedBy — a pin's provenance is fixed at creation, unlike APIKey's
	// CreatedAt (which import restore can still overwrite by design) — while
	// Namespace, Note, and UpdatedAt take the incoming values.
	PutPins(ctx context.Context, entries []Pin) error
	// GetPins returns the entries matching the given keys, in no
	// particular order; a key with no matching row is simply absent from the
	// result (not an error).
	GetPins(ctx context.Context, keys []string) ([]Pin, error)
	// DeletePins removes the entries with the given keys and
	// returns the number of rows actually deleted; a key with no matching row
	// does not count and is not an error.
	DeletePins(ctx context.Context, keys []string) (int64, error)
	// ListPins returns every entry ordered by Key, for the CLI/UI.
	ListPins(ctx context.Context) ([]Pin, error)
	// RenamePinNamespaces rewrites every entry whose Namespace exactly
	// equals from to to instead (maintenance.Move, alongside
	// RenameLinkEndpoints/RenameAPIKeyNamespaces); a namespace that merely
	// starts with from (e.g. "memini2" against from="memini") is untouched.
	RenamePinNamespaces(ctx context.Context, from, to string) error
}

// ClientSettingsStore is implemented by drivers that persist the server's
// global default ClientSettings — the layer between the built-in defaults
// (DefaultClientSettings) and any per-API-key override (APIKey.Settings). It
// is an optional capability interface — the EmbedModelStore precedent, which
// stores its single value the same way, over the same meta key/value table.
type ClientSettingsStore interface {
	// GlobalClientSettings returns the stored global defaults, or the zero
	// ClientSettings (every field nil) when none has been set yet.
	GlobalClientSettings(ctx context.Context) (ClientSettings, error)
	// SetGlobalClientSettings replaces the stored global defaults wholesale.
	SetGlobalClientSettings(ctx context.Context, s ClientSettings) error
}

// EventKind names an operation the activity log records. Reads and writes are
// both logged; the kinds are the wire-level values the REST filter accepts.
type EventKind string

// EventPin/EventUnpin/EventSettings are part of the config-handshake wire
// contract (api/openapi.yaml's EventKind enum) landed ahead of the
// pin/settings write paths that will actually emit them — see
// internal/api/rest/config_stubs.go. Recognizing them here now means the
// GET /v1/activity ?kind= filter never 400s on a value the spec itself
// advertises as valid.
const (
	EventRecall    EventKind = "recall"
	EventGet       EventKind = "get"
	EventBriefing  EventKind = "briefing"
	EventRemember  EventKind = "remember"
	EventUpdate    EventKind = "update"
	EventForget    EventKind = "forget"
	EventSupersede EventKind = "supersede"
	EventPin       EventKind = "pin"
	EventUnpin     EventKind = "unpin"
	EventSettings  EventKind = "settings"
	// EventInject is a client injection-telemetry report (POST
	// /v1/activity/injected): which served memories a hook actually injected
	// into model context, and what its local gates suppressed.
	EventInject EventKind = "inject"
)

// ValidEventKind reports whether k is one of the recorded kinds, so the REST
// layer can reject an unknown filter value before it reaches SQL.
func ValidEventKind(k EventKind) bool {
	switch k {
	case EventRecall, EventGet, EventBriefing, EventRemember, EventUpdate, EventForget, EventSupersede,
		EventPin, EventUnpin, EventSettings, EventInject:
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
	Detail map[string]any
	// Actor is who performed the operation: the name of the NAMED API key that
	// authenticated the request, or "" for the admin env key, an
	// unauthenticated dev-mode request, or a legacy row written before
	// attribution existed. ActorKind disambiguates the empty cases.
	Actor string
	// ActorKind classifies the actor: "key" (a named API key, Actor holds its
	// name), "env" (the admin env key, Actor is ""), "none" (unauthenticated
	// dev mode, Actor is ""), or "" (a legacy row predating attribution —
	// unknown). Attribution is automatic and unconditional, stamped on every
	// event row from the request context (service.WithActor).
	ActorKind string
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
	// Actor restricts to events performed by the named API key (exact match on
	// Event.Actor); empty means no constraint. Matches the key name only — the
	// admin env key and dev-mode requests carry no name and so are never
	// selected by this filter.
	Actor string
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
