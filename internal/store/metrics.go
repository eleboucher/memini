package store

import (
	"time"

	"github.com/eleboucher/memini/internal/memory"
)

// Metrics receives store events for observability. Methods must be safe for
// concurrent use; a nil Metrics is replaced by a no-op. Implementations live
// alongside the Prometheus registry in cmd/memini.
type Metrics interface {
	// Upsert records one Upsert outcome. op is "insert" or "update"; tier
	// is the memory's tier (working/episodic/semantic/procedural); memoryType
	// is the typed-extraction class (decision/preference/problem) or "".
	Upsert(op, tier, memoryType string)
	// Delete records a hard delete from the Forget API path.
	Delete()
	// SoftDelete records a tombstone written by consolidation.
	SoftDelete()
	// SweepExpired records one memory removed by the decay sweeper.
	SweepExpired(tier string)
	// ActiveByTier sets the current count of live (non-superseded,
	// non-expired) memories per tier. Called periodically after sweeps/fsck.
	ActiveByTier(tier string, n int)
	// DedupTombstoned records the number of memories tombstoned by one
	// dedup pass (the periodic vector-cluster job or a one-shot dedup call).
	// Called once per pass with the pass total.
	DedupTombstoned(n int)
}

type nopMetrics struct{}

func (nopMetrics) Upsert(string, string, string) {}
func (nopMetrics) Delete()                       {}
func (nopMetrics) SoftDelete()                   {}
func (nopMetrics) SweepExpired(string)           {}
func (nopMetrics) ActiveByTier(string, int)      {}
func (nopMetrics) DedupTombstoned(int)           {}

// NopMetrics is exported for tests.
func NopMetrics() Metrics { return nopMetrics{} }

var _ Metrics = nopMetrics{}

// knownMemoryTypes bounds the memory_type metric label to the typed-extraction
// classes, so an arbitrary caller-supplied metadata value can't explode series
// cardinality.
var knownMemoryTypes = map[string]bool{"decision": true, "preference": true, "problem": true}

// MemoryTypeLabel returns m's typed-extraction class for the memory_type metric
// label, or "" when absent or unrecognized (keeping cardinality bounded).
func MemoryTypeLabel(m *memory.Memory) string {
	if mt, ok := m.Metadata["memory_type"].(string); ok && knownMemoryTypes[mt] {
		return mt
	}
	return ""
}

// guard so the file always references time if it grows later.
var _ = time.Time{}
