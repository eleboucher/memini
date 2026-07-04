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
}

// Store is the persistence and retrieval contract for memories. Implementations
// must be safe for concurrent use.
type Store interface {
	// Upsert inserts or replaces a memory (matched by ID within its namespace),
	// including its embedding and keyword-search index entry. Returns ErrConflict
	// when the given ID already exists under a different namespace.
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
