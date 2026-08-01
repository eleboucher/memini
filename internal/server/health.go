package server

import (
	"context"
	"net/http"
	"time"

	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/version"
)

const keyStatus = "status"

// storePingTimeout bounds the on-demand store ping verbose healthz performs;
// it must stay well under the request's own timeout so a stuck store doesn't
// hang the healthz response the operator is diagnosing the store with.
const storePingTimeout = 2 * time.Second

// depBlock is one dependency's status in the verbose healthz response.
type depBlock struct {
	OK          bool   `json:"ok"`
	LastError   string `json:"last_error,omitempty"`
	LastSuccess string `json:"last_success,omitempty"`
}

// optionalDepBlock adds Configured ahead of the shared depBlock fields. It
// backs both the "llm" and "reranker" blocks — dependencies that are only
// present when configured. OK must NOT be omitempty: false is the zero value,
// so omitempty would drop the "ok" key exactly when the dependency is
// configured but down — the one signal this block exists to report. An
// unconfigured dependency renders ok:false too; consumers gate on configured
// first.
type optionalDepBlock struct {
	Configured  bool   `json:"configured"`
	OK          bool   `json:"ok"`
	LastError   string `json:"last_error,omitempty"`
	LastSuccess string `json:"last_success,omitempty"`
}

// repairBlock is the deferred-repair backlog in the verbose healthz response.
//
// It is a sibling of Deps rather than an entry in it: repair is internal state,
// not a dependency, and a backlog must never read as "a dependency is down".
// Omitted entirely when the store has no repair queue.
//
// Stuck is the number that matters. Pending and Enrich drain on their own
// within seconds; Stuck memories exhausted their attempt ceiling and will stay
// keyword-only until the sweeper's circuit breaker re-arms them or an operator
// intervenes, so reporting them identically to Pending is what lets silent
// degradation hide.
type repairBlock struct {
	Pending   int    `json:"pending"`
	Enrich    int    `json:"enrich"`
	Stuck     int    `json:"stuck"`
	OldestAge string `json:"oldest_age,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

type verboseHealth struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Deps    struct {
		Store    depBlock         `json:"store"`
		Embedder depBlock         `json:"embedder"`
		LLM      optionalDepBlock `json:"llm"`
		Reranker optionalDepBlock `json:"reranker"`
	} `json:"deps"`
	Repair *repairBlock `json:"repair,omitempty"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// /healthz is mounted outside bearer auth on purpose (probes must reach
	// it unauthenticated), but ?verbose=1 leaks dependency error internals
	// (hostnames, driver errors). When an API key is configured, require it
	// for the verbose view; an absent/invalid token degrades to the plain
	// body instead of 401ing, so probes and naive monitors polling
	// ?verbose=1 keep working. No API key configured leaves verbose open,
	// matching prior behavior.
	verbose := r.URL.Query().Get("verbose") == "1"
	if verbose && s.cfg.APIKey != "" && !validBearer(r, s.cfg.APIKey) {
		verbose = false
	}
	if !verbose {
		httputil.JSON(w, http.StatusOK, map[string]string{
			keyStatus: "ok",
			"version": version.Version,
		})
		return
	}
	httputil.JSON(w, http.StatusOK, s.verboseHealthz(r.Context()))
}

// verboseHealthz assembles the ?verbose=1 body. Status is always "ok": like
// plain /healthz, this is a liveness check, not a readiness gate — a failing
// embedder or LLM is degraded (keyword-only recall), not dead. Dependency
// health is informational; /readyz (store-only) is the only signal that
// affects whether traffic is routed here.
func (s *Server) verboseHealthz(ctx context.Context) verboseHealth {
	var resp verboseHealth
	resp.Status = "ok"
	resp.Version = version.Version
	resp.Deps.Store = s.storeDepBlock(ctx)
	resp.Deps.Embedder = s.depBlockFor("embedder")

	resp.Deps.LLM.Configured = s.llmConfigured.Load()
	if resp.Deps.LLM.Configured {
		b := s.depBlockFor("llm")
		resp.Deps.LLM.OK = b.OK
		resp.Deps.LLM.LastError = b.LastError
		resp.Deps.LLM.LastSuccess = b.LastSuccess
	}

	resp.Deps.Reranker.Configured = s.rerankConfigured.Load()
	if resp.Deps.Reranker.Configured {
		b := s.depBlockFor("reranker")
		resp.Deps.Reranker.OK = b.OK
		resp.Deps.Reranker.LastError = b.LastError
		resp.Deps.Reranker.LastSuccess = b.LastSuccess
	}
	resp.Repair = s.repairBlock(ctx)
	return resp
}

// repairBlock renders the deferred-repair backlog, or nil when the store has no
// repair queue. Bounded by the same deadline as the store ping so a wedged
// store cannot hang the endpoint an operator is using to diagnose it, and a
// failed query degrades to omitting the block rather than failing the response.
func (s *Server) repairBlock(ctx context.Context) *repairBlock {
	fn := s.repairStats.Load()
	if fn == nil {
		return nil
	}
	statCtx, cancel := context.WithTimeout(ctx, storePingTimeout)
	defer cancel()
	stats, err := (*fn)(statCtx)
	if err != nil {
		return nil
	}
	var b repairBlock
	var oldest time.Time
	for _, st := range stats {
		switch st.State {
		case store.RepairPending:
			b.Pending = st.Count
		case store.RepairEnrich:
			b.Enrich = st.Count
		case store.RepairFailed:
			b.Stuck = st.Count
			b.LastError = st.LastError
		}
		if !st.OldestAt.IsZero() && (oldest.IsZero() || st.OldestAt.Before(oldest)) {
			oldest = st.OldestAt
		}
	}
	if b.Pending == 0 && b.Enrich == 0 && b.Stuck == 0 {
		return &b
	}
	if !oldest.IsZero() {
		b.OldestAge = time.Since(oldest).Round(time.Second).String()
	}
	return &b
}

// storeDepBlock pings the store on demand via the existing readiness func
// rather than tracking it: the store is a single local check (no network
// round trip worth avoiding), so there's nothing a tracker would add over
// asking it right now, and this stays consistent with /readyz by
// construction instead of needing to be kept in sync with it. A healthy
// probe stamps LastSuccess with the current time (rendered exactly like
// depBlockFor's tracker timestamps): the block must be self-describing —
// without it a consumer can't tell ok-freshly-probed from ok-never-called.
func (s *Server) storeDepBlock(ctx context.Context) depBlock {
	fn := s.ready.Load()
	if fn == nil {
		return depBlock{OK: true, LastSuccess: time.Now().UTC().Format(time.RFC3339)}
	}
	pingCtx, cancel := context.WithTimeout(ctx, storePingTimeout)
	defer cancel()
	if err := (*fn)(pingCtx); err != nil {
		return depBlock{OK: false, LastError: err.Error()}
	}
	return depBlock{OK: true, LastSuccess: time.Now().UTC().Format(time.RFC3339)}
}

// depBlockFor renders dep's tracker snapshot. A dep with no recorded events
// (no tracker installed, or the tracker has never seen a call for dep) reads
// as an unremarkable ok:true default rather than a fabricated failure.
func (s *Server) depBlockFor(dep string) depBlock {
	snap, ok := s.deps.Load().snapshot(dep)
	if !ok {
		return depBlock{OK: true}
	}
	b := depBlock{OK: snap.ok, LastError: snap.lastErr}
	if !snap.lastSuccess.IsZero() {
		b.LastSuccess = snap.lastSuccess.UTC().Format(time.RFC3339)
	}
	return b
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if fn := s.ready.Load(); fn != nil {
		if err := (*fn)(r.Context()); err != nil {
			httputil.JSON(w, http.StatusServiceUnavailable, map[string]string{
				keyStatus: "not ready",
				"error":   err.Error(),
			})
			return
		}
	}
	httputil.JSON(w, http.StatusOK, map[string]string{keyStatus: "ready"})
}
