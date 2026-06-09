package store

import "time"

// Metrics receives store events for observability. Methods must be safe for
// concurrent use; a nil Metrics is replaced by a no-op. Implementations live
// alongside the Prometheus registry in cmd/memini.
type Metrics interface {
	// Upsert records one Upsert outcome. op is "insert" or "update"; tier
	// is the memory's tier (working/episodic/semantic/procedural).
	Upsert(op, tier string)
	// Delete records a hard delete from the Forget API path.
	Delete()
	// SoftDelete records a tombstone written by consolidation.
	SoftDelete()
	// SweepExpired records one memory removed by the decay sweeper.
	SweepExpired(tier string)
	// ActiveByTier sets the current count of live (non-superseded,
	// non-expired) memories per tier. Called periodically after sweeps/fsck.
	ActiveByTier(tier string, n int)
}

type nopMetrics struct{}

func (nopMetrics) Upsert(string, string)    {}
func (nopMetrics) Delete()                  {}
func (nopMetrics) SoftDelete()              {}
func (nopMetrics) SweepExpired(string)      {}
func (nopMetrics) ActiveByTier(string, int) {}

// NopMetrics is exported for tests.
func NopMetrics() Metrics { return nopMetrics{} }

var _ Metrics = nopMetrics{}

// guard so the file always references time if it grows later.
var _ = time.Time{}
