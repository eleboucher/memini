package server

import (
	"sync"
	"time"
)

// DepTracker records the outcome of calls to out-of-process dependencies
// (embedder, LLM) so verbose healthz can report them without those packages
// depending on server. Call sites feed it via Record; nothing here changes
// /readyz, which stays store-only.
//
// Lives in internal/server (rather than a new package) because the only
// consumer of the read side is the health handler here, and the only
// producers (cmd/memini's embed/llm decorators) already import server for
// Server/SetReady — a dedicated internal/depstatus package would just add an
// import hop without decoupling anything.
type DepTracker struct {
	mu   sync.Mutex
	deps map[string]*depState
}

// depState is one dependency's last known outcome. Guarded by DepTracker.mu
// rather than its own lock: entries are looked up and mutated together under
// a single short critical section, so a per-entry lock would only add
// contention without adding concurrency (Record calls for different deps
// already don't block each other across separate DepTracker instances, and
// within one instance dependency count is fixed and tiny).
type depState struct {
	lastSuccess time.Time
	lastErr     string
	lastErrAt   time.Time
}

// NewDepTracker returns an empty tracker.
func NewDepTracker() *DepTracker {
	return &DepTracker{deps: make(map[string]*depState)}
}

// Record logs the outcome of one call to dep (e.g. "embedder", "llm"). A nil
// err records success, advancing lastSuccess; a non-nil err records the
// error text and its timestamp. Either way the other field is left as-is, so
// a dependency that is failing now but succeeded five minutes ago still
// reports that last success.
func (t *DepTracker) Record(dep string, err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	d, ok := t.deps[dep]
	if !ok {
		d = &depState{}
		t.deps[dep] = d
	}
	if err != nil {
		d.lastErr = err.Error()
		d.lastErrAt = time.Now()
		return
	}
	d.lastSuccess = time.Now()
}

// snapshot describes dep's current status for rendering. ok is true when no
// error has ever been recorded, or the most recent recorded event was a
// success. found is false when dep has never been recorded (e.g. an LLM that
// hasn't fielded a request yet), in which case the caller should render a
// bare "ok" default rather than a fabricated timestamp.
type depSnapshot struct {
	ok          bool
	lastErr     string
	lastSuccess time.Time
}

func (t *DepTracker) snapshot(dep string) (depSnapshot, bool) {
	if t == nil {
		return depSnapshot{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	d, ok := t.deps[dep]
	if !ok {
		return depSnapshot{}, false
	}
	ok2 := d.lastErrAt.IsZero() || d.lastSuccess.After(d.lastErrAt)
	return depSnapshot{ok: ok2, lastErr: d.lastErr, lastSuccess: d.lastSuccess}, true
}
