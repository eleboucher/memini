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
	// IncludeExpired includes memories past their TTL (default excludes them).
	IncludeExpired bool
	// IncludeSuperseded includes contradiction-tombstoned memories.
	IncludeSuperseded bool
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

	// Delete removes a memory by ID, or returns ErrNotFound.
	Delete(ctx context.Context, namespace, id string) error

	// SetSuperseded tombstones a memory by recording the ID that replaced it,
	// excluding it from default search results. Returns ErrNotFound if missing.
	SetSuperseded(ctx context.Context, namespace, id, supersededBy string) error

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

	// Ping verifies the backend is reachable, for readiness checks.
	Ping(ctx context.Context) error

	// Close releases backend resources.
	Close() error
}
