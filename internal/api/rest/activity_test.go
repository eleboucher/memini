package rest_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/eleboucher/memini/internal/api/rest"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// activityEvent mirrors the ActivityEvent wire shape for assertions.
type activityEvent struct {
	OpID      string         `json:"op_id"`
	Kind      string         `json:"kind"`
	Namespace string         `json:"namespace"`
	Actor     string         `json:"actor"`
	ActorKind string         `json:"actor_kind"`
	Query     string         `json:"query"`
	Detail    map[string]any `json:"detail"`
	Memories  []struct {
		ID       string   `json:"id"`
		Summary  string   `json:"summary"`
		Tier     string   `json:"tier"`
		Rank     int      `json:"rank"`
		Score    *float64 `json:"score"`
		Section  string   `json:"section"`
		Injected *bool    `json:"injected"`
	} `json:"memories"`
}

type activityResponse struct {
	Events     []activityEvent `json:"events"`
	NextCursor string          `json:"next_cursor"`
	HasMore    bool            `json:"has_more"`
}

// newActivityServer is newServer with the activity log on and synchronous, so
// a request's events are visible to the next request.
func newActivityServer(t *testing.T) http.Handler {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "activity.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := service.New(st, embedtest.New(dims),
		service.WithEventLog(true), service.WithSyncEventLog(), service.WithSyncReinforce())
	r := chi.NewRouter()
	rest.New(svc, rest.AuthConfig{
		APIKey: apiKey, NamespaceHeader: nsHdr, DefaultNamespace: "default", HomeHeader: homeHdr,
	}).Mount(r)
	return r
}

func TestListActivity(t *testing.T) {
	h := newActivityServer(t)

	rec := do(t, h, http.MethodPost, "/v1/memories", "acme", apiKey, map[string]any{
		"content": "the database is postgres", "tier": string(memory.TierSemantic),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPost, "/v1/search", "acme", apiKey, map[string]any{
		"query": "which database", "limit": 5,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("search: want 200, got %d (%s)", rec.Code, rec.Body)
	}

	t.Run("feed carries the recall query, rank and score", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/v1/activity?kind=recall", "acme", apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body)
		}
		var out activityResponse
		mustJSON(t, rec, &out)
		if len(out.Events) != 1 {
			t.Fatalf("got %d recall events, want 1", len(out.Events))
		}
		ev := out.Events[0]
		if ev.Kind != "recall" || ev.Query != "which database" {
			t.Fatalf("event = %+v, want a recall of %q", ev, "which database")
		}
		if len(ev.Memories) != 1 {
			t.Fatalf("recall served %d memories, want 1", len(ev.Memories))
		}
		m := ev.Memories[0]
		if m.Rank != 1 || m.Score == nil {
			t.Errorf("memory = %+v, want rank 1 with a score", m)
		}
		if m.Summary != "the database is postgres" {
			t.Errorf("summary snapshot = %q", m.Summary)
		}
	})

	t.Run("kind filter", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/v1/activity?kind=remember", "acme", apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body)
		}
		var out activityResponse
		mustJSON(t, rec, &out)
		if len(out.Events) != 1 || out.Events[0].Kind != "remember" {
			t.Fatalf("kind=remember returned %+v", out.Events)
		}
	})

	t.Run("comma-separated kinds", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/v1/activity?kind=recall,remember", "acme", apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body)
		}
		var out activityResponse
		mustJSON(t, rec, &out)
		if len(out.Events) != 2 {
			t.Fatalf("got %d events, want 2", len(out.Events))
		}
	})

	t.Run("unknown kind is a 400", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/v1/activity?kind=bogus", "acme", apiKey, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("want 400 for an unknown kind, got %d (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("namespace scoping", func(t *testing.T) {
		// A different namespace sees none of acme's activity.
		rec := do(t, h, http.MethodGet, "/v1/activity", "other", apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body)
		}
		var out activityResponse
		mustJSON(t, rec, &out)
		if len(out.Events) != 0 {
			t.Fatalf("namespace 'other' sees %d of acme's events", len(out.Events))
		}
		// all_namespaces aggregates regardless of the header.
		rec = do(t, h, http.MethodGet, "/v1/activity?all_namespaces=true", "other", apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body)
		}
		mustJSON(t, rec, &out)
		if len(out.Events) == 0 {
			t.Fatal("all_namespaces=true returned nothing")
		}
	})

	t.Run("pagination round trip", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/v1/activity?limit=1", "acme", apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body)
		}
		var page1 activityResponse
		mustJSON(t, rec, &page1)
		if len(page1.Events) != 1 || !page1.HasMore || page1.NextCursor == "" {
			t.Fatalf("page 1 = %+v, want 1 event, has_more and a cursor", page1)
		}
		rec = do(t, h, http.MethodGet, "/v1/activity?limit=1&before="+page1.NextCursor, "acme", apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("page 2: want 200, got %d (%s)", rec.Code, rec.Body)
		}
		var page2 activityResponse
		mustJSON(t, rec, &page2)
		if len(page2.Events) != 1 {
			t.Fatalf("page 2 returned %d events, want 1", len(page2.Events))
		}
		if page2.Events[0].OpID == page1.Events[0].OpID {
			t.Fatal("the cursor re-returned page 1's event")
		}
	})

	t.Run("malformed cursor is a 400", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/v1/activity?before=nonsense", "acme", apiKey, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("want 400 for a malformed cursor, got %d (%s)", rec.Code, rec.Body)
		}
	})
}

// TestReportInjected covers POST /v1/activity/injected: a valid report is a
// 204 whose inject event lands in the feed and annotates the matching recall's
// memories (true for a named id, false for an omitted one — and the flag is
// ABSENT, not false, on a recall no report covered); a missing or unknown
// surface, an unknown body field, and a negative count are all 400s.
func TestReportInjected(t *testing.T) {
	h := newActivityServer(t)

	seed := func(content string) string {
		t.Helper()
		rec := do(t, h, http.MethodPost, "/v1/memories", "acme", apiKey, map[string]any{
			"content": content, "tier": string(memory.TierSemantic),
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed: want 201, got %d (%s)", rec.Code, rec.Body)
		}
		var out struct {
			ID string `json:"id"`
		}
		mustJSON(t, rec, &out)
		return out.ID
	}
	injectedID := seed("the primary database is postgres")
	suppressedID := seed("the backup database is mysql")

	rec := do(t, h, http.MethodPost, "/v1/search", "acme", apiKey, map[string]any{
		"query": "database", "limit": 5,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("search: want 200, got %d (%s)", rec.Code, rec.Body)
	}

	t.Run("valid report is a 204", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, "/v1/activity/injected", "acme", apiKey, map[string]any{
			"session_id":          "s1",
			"surface":             "prompt",
			"source":              "claude-code",
			"injected_ids":        []string{injectedID, "unknown-id-is-fine"},
			"injected_tokens_est": 412,
			"injected_chars":      1650,
			"suppressed":          map[string]int{"seen": 2, "cooldown": 1, "score": 3},
		})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("want 204, got %d (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("inject event lands in the feed", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/v1/activity?kind=inject", "acme", apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body)
		}
		var out activityResponse
		mustJSON(t, rec, &out)
		if len(out.Events) != 1 {
			t.Fatalf("got %d inject events, want 1", len(out.Events))
		}
		ev := out.Events[0]
		if ev.Kind != "inject" || ev.Detail["surface"] != "prompt" || ev.Detail["session_id"] != "s1" {
			t.Fatalf("event = %+v, want an inject with surface=prompt session_id=s1", ev)
		}
		if len(ev.Memories) != 2 {
			t.Fatalf("inject event carries %d memory refs, want 2 (unknown ids kept)", len(ev.Memories))
		}
	})

	t.Run("recall memories annotated true/false", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/v1/activity?kind=recall,inject", "acme", apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body)
		}
		var out activityResponse
		mustJSON(t, rec, &out)
		var recall *activityEvent
		for i := range out.Events {
			if out.Events[i].Kind == "recall" {
				recall = &out.Events[i]
			}
		}
		if recall == nil || len(recall.Memories) < 2 {
			t.Fatalf("no recall event serving both memories in %+v", out.Events)
		}
		flags := map[string]*bool{}
		for _, m := range recall.Memories {
			flags[m.ID] = m.Injected
		}
		if got := flags[injectedID]; got == nil || !*got {
			t.Errorf("injected memory flag = %v, want true", got)
		}
		if got := flags[suppressedID]; got == nil || *got {
			t.Errorf("suppressed memory flag = %v, want false", got)
		}
	})

	t.Run("uncovered recall omits the flag entirely", func(t *testing.T) {
		// A recall in another namespace with no report: the wire must OMIT the
		// injected key (absent means unknown), never emit false.
		rec := do(t, h, http.MethodPost, "/v1/memories", "quiet", apiKey, map[string]any{
			"content": "the cache is redis", "tier": string(memory.TierSemantic),
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed quiet: %d (%s)", rec.Code, rec.Body)
		}
		rec = do(t, h, http.MethodPost, "/v1/search", "quiet", apiKey, map[string]any{
			"query": "cache", "limit": 5,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("quiet search: %d (%s)", rec.Code, rec.Body)
		}
		rec = do(t, h, http.MethodGet, "/v1/activity?kind=recall", "quiet", apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body)
		}
		// Assert on the raw JSON: a decoded *bool cannot distinguish a
		// deliberate "injected": false from an omitted key.
		if strings.Contains(rec.Body.String(), `"injected"`) {
			t.Fatalf("uncovered recall carries an injected key: %s", rec.Body)
		}
		var out activityResponse
		mustJSON(t, rec, &out)
		if len(out.Events) != 1 || len(out.Events[0].Memories) == 0 {
			t.Fatalf("quiet feed = %+v, want one recall with memories", out.Events)
		}
	})

	t.Run("suppression-only report is a 204", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, "/v1/activity/injected", "acme", apiKey, map[string]any{
			"session_id": "s1", "surface": "pretool",
			"suppressed": map[string]int{"budget": 4},
		})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("want 204, got %d (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("missing surface is a 400", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, "/v1/activity/injected", "acme", apiKey, map[string]any{
			"session_id": "s1", "injected_ids": []string{injectedID},
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("unknown surface is a 400", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, "/v1/activity/injected", "acme", apiKey, map[string]any{
			"session_id": "s1", "surface": "bogus",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("malformed body is a 400", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, "/v1/activity/injected", "acme", apiKey, map[string]any{
			"surface": "prompt", "not_a_field": true,
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unknown field: want 400, got %d (%s)", rec.Code, rec.Body)
		}
		rec = do(t, h, http.MethodPost, "/v1/activity/injected", "acme", apiKey, map[string]any{
			"surface": "prompt", "injected_tokens_est": -1,
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("negative count: want 400, got %d (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("kind=inject filter is accepted", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/v1/activity?kind=inject", "acme", apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body)
		}
	})
}

// TestListActivityFilters covers the feed's filters, and in particular that
// tier and text select whole operations: a filtered recall must still report
// every memory it served, not just the ones that matched the filter.
func TestListActivityFilters(t *testing.T) {
	h := newActivityServer(t)

	seed := func(content string, tier memory.Tier) {
		t.Helper()
		rec := do(t, h, http.MethodPost, "/v1/memories", "acme", apiKey, map[string]any{
			"content": content, "tier": string(tier),
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed %q: %d (%s)", content, rec.Code, rec.Body)
		}
	}
	seed("the production database is postgres", memory.TierSemantic)
	seed("ran the database migration today", memory.TierEpisodic)

	// One recall spanning both tiers.
	rec := do(t, h, http.MethodPost, "/v1/search", "acme", apiKey, map[string]any{
		"query": "database", "limit": 10,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d (%s)", rec.Code, rec.Body)
	}

	get := func(t *testing.T, query string) activityResponse {
		t.Helper()
		rec := do(t, h, http.MethodGet, "/v1/activity"+query, "acme", apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: want 200, got %d (%s)", query, rec.Code, rec.Body)
		}
		var out activityResponse
		mustJSON(t, rec, &out)
		return out
	}

	// How many memories the unfiltered recall served — the number a tier-filtered
	// view must keep reporting.
	base := get(t, "?kind=recall")
	if len(base.Events) != 1 {
		t.Fatalf("expected 1 recall event, got %d", len(base.Events))
	}
	served := len(base.Events[0].Memories)
	if served < 2 {
		t.Fatalf("recall served %d memories, want both seeded tiers", served)
	}

	t.Run("tier selects whole operations", func(t *testing.T) {
		// The recall touched a semantic memory, so it matches — and comes back
		// whole. Returning only its semantic row would misreport the recall as
		// having served fewer memories than it did.
		out := get(t, "?kind=recall&tier=semantic")
		if len(out.Events) != 1 {
			t.Fatalf("tier=semantic returned %d recall events, want 1", len(out.Events))
		}
		if got := len(out.Events[0].Memories); got != served {
			t.Fatalf("tier-filtered recall reports %d memories, want all %d it actually served",
				got, served)
		}
		// A tier nothing touched matches nothing.
		if out := get(t, "?tier=working"); len(out.Events) != 0 {
			t.Fatalf("tier=working matched %d events, want 0", len(out.Events))
		}
	})

	t.Run("text filter", func(t *testing.T) {
		// Matches the recall's query...
		if out := get(t, "?q=database&kind=recall"); len(out.Events) != 1 {
			t.Fatalf("q=database matched %d recall events, want 1", len(out.Events))
		}
		// ...and a served memory's summary, case-insensitively.
		if out := get(t, "?q=POSTGRES"); len(out.Events) == 0 {
			t.Fatal("q=POSTGRES matched nothing; the text filter should be case-insensitive")
		}
		if out := get(t, "?q=kangaroo"); len(out.Events) != 0 {
			t.Fatalf("q=kangaroo matched %d events, want 0", len(out.Events))
		}
	})

	t.Run("since window", func(t *testing.T) {
		if out := get(t, "?since=2020-01-01T00:00:00Z"); len(out.Events) == 0 {
			t.Fatal("since=2020 matched nothing, want the recent events")
		}
		if out := get(t, "?since=2030-01-01T00:00:00Z"); len(out.Events) != 0 {
			t.Fatalf("since=2030 matched %d events, want 0", len(out.Events))
		}
	})

	t.Run("invalid tier is a 400", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/v1/activity?tier=bogus", "acme", apiKey, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("want 400 for an unknown tier, got %d (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("namespace narrowing in all_namespaces mode", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, "/v1/memories", "other", apiKey, map[string]any{
			"content": "a memory elsewhere", "tier": string(memory.TierSemantic),
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed other ns: %d (%s)", rec.Code, rec.Body)
		}
		rec = do(t, h, http.MethodGet, "/v1/activity?all_namespaces=true&namespace=other", "acme", apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body)
		}
		var out activityResponse
		mustJSON(t, rec, &out)
		if len(out.Events) == 0 {
			t.Fatal("namespace=other matched nothing")
		}
		for _, ev := range out.Events {
			if ev.Namespace != "other" {
				t.Fatalf("namespace=other leaked an event from %q", ev.Namespace)
			}
		}
	})
}
