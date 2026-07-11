package rest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/eleboucher/memini/internal/api/rest"
	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

const (
	dims    = 64
	nsHdr   = "X-Memini-Namespace"
	homeHdr = "X-Memini-Home"
	apiKey  = "secret-token"
)

func newServer(t *testing.T) http.Handler {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "rest.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := service.New(st, embedtest.New(dims))
	r := chi.NewRouter()
	rest.New(svc, rest.AuthConfig{
		APIKey: apiKey, NamespaceHeader: nsHdr, DefaultNamespace: "default", HomeHeader: homeHdr,
	}).Mount(r)
	return r
}

// do issues a request with no X-Memini-Home header. Most tests don't care
// about the home leg; doHome below is the variant for the ones that do.
func do(t *testing.T, h http.Handler, method, path, ns, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return doHome(t, h, method, path, ns, "", token, body)
}

// doHome is do with an extra home parameter, setting X-Memini-Home when
// non-empty.
func doHome(t *testing.T, h http.Handler, method, path, ns, home, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if ns != "" {
		req.Header.Set(nsHdr, ns)
	}
	if home != "" {
		req.Header.Set(homeHdr, home)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuthRequired(t *testing.T) {
	h := newServer(t)
	rec := do(t, h, http.MethodPost, "/v1/search", "alice", "", map[string]any{"query": "x"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", rec.Code)
	}
}

func TestForgetByTag(t *testing.T) {
	h := newServer(t)

	// alice and bob each get a tagged memory; only alice's should be deleted.
	remember := func(ns, content string, tags []string) {
		rec := do(t, h, http.MethodPost, "/v1/memories", ns, apiKey, map[string]any{
			"content": content, "tier": "semantic", "tags": tags,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("remember %s: want 201, got %d (%s)", ns, rec.Code, rec.Body)
		}
	}
	remember("alice", "imported fact one", []string{"import:mem0:2026-06-12"})
	remember("alice", "imported fact two", []string{"import:mem0:2026-06-12"})
	remember("alice", "a memory alice wrote herself", []string{"manual"})
	remember("bob", "bob's imported fact", []string{"import:mem0:2026-06-12"})

	// A missing tag must be rejected (spec marks it required) — never a
	// delete-everything.
	rec := do(t, h, http.MethodDelete, "/v1/memories", "alice", apiKey, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing tag: want 400, got %d (%s)", rec.Code, rec.Body)
	}

	// Delete alice's import; her manual memory and bob's data are untouched.
	rec = do(t, h, http.MethodDelete, "/v1/memories?tag=import:mem0:2026-06-12", "alice", apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("forget by tag: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var got struct {
		Deleted int `json:"deleted"`
	}
	mustJSON(t, rec, &got)
	if got.Deleted != 2 {
		t.Fatalf("deleted = %d, want 2", got.Deleted)
	}

	// alice keeps her manual memory.
	rec = do(t, h, http.MethodGet, "/v1/memories", "alice", apiKey, nil)
	var list struct {
		Memories []struct {
			Content string `json:"content"`
		} `json:"memories"`
	}
	mustJSON(t, rec, &list)
	if len(list.Memories) != 1 || list.Memories[0].Content != "a memory alice wrote herself" {
		t.Fatalf("alice should keep only her manual memory, got %+v", list.Memories)
	}

	// bob's tagged memory is untouched (scoped to alice's namespace).
	rec = do(t, h, http.MethodGet, "/v1/memories", "bob", apiKey, nil)
	mustJSON(t, rec, &list)
	if len(list.Memories) != 1 {
		t.Fatalf("bob's memory should be untouched, got %d", len(list.Memories))
	}
}

func TestGetBriefing(t *testing.T) {
	h := newServer(t)
	remember := func(content, tier string, tags []string) {
		body := map[string]any{"content": content, "tier": tier}
		if tags != nil {
			body["tags"] = tags
		}
		rec := do(t, h, http.MethodPost, "/v1/memories", "proj", apiKey, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("remember: %d (%s)", rec.Code, rec.Body)
		}
	}
	remember("we chose Postgres over MySQL for JSONB", "semantic", nil)
	remember("to deploy, run make release then helm upgrade", "procedural", nil)
	remember("finished the auth refactor today", "episodic", nil)
	remember("the user is Erwan, prefers Go", "semantic", []string{"pinned"})

	// Diagnostic: confirm tags actually land on the stored memory so the
	// briefing's pinned section has something to find.
	listRec := do(t, h, http.MethodGet, "/v1/memories", "proj", apiKey, nil)
	t.Logf("TestGetBriefing list body: %s", listRec.Body)

	rec := do(t, h, http.MethodGet, "/v1/namespaces/briefing", "proj", apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("briefing: %d (%s)", rec.Code, rec.Body)
	}
	var b struct {
		Namespace  string                     `json:"namespace"`
		Facts      []struct{ Content string } `json:"facts"`
		Procedures []struct{ Content string } `json:"procedures"`
		Recent     []struct{ Content string } `json:"recent"`
		Pinned     []struct{ Content string } `json:"pinned"`
	}
	mustJSON(t, rec, &b)
	if b.Namespace != "proj" {
		t.Errorf("namespace = %q", b.Namespace)
	}
	if len(b.Facts) < 1 || len(b.Procedures) != 1 || len(b.Recent) != 1 || len(b.Pinned) != 1 {
		t.Fatalf("briefing sections off: facts=%d procs=%d recent=%d pinned=%d",
			len(b.Facts), len(b.Procedures), len(b.Recent), len(b.Pinned))
	}
}

func TestGetBriefingPerSectionCaps(t *testing.T) {
	h := newServer(t)
	remember := func(content, tier string, tags []string) {
		body := map[string]any{"content": content, "tier": tier}
		if tags != nil {
			body["tags"] = tags
		}
		rec := do(t, h, http.MethodPost, "/v1/memories", "proj", apiKey, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("remember %q: %d (%s)", content, rec.Code, rec.Body)
		}
	}
	// Five of each tier, plus three pinned (across tiers) — enough to exercise
	// per-section caps in isolation. The unpinned names distinguish from the
	// pinned ones so each section has a known floor of one tier.
	for i := range 5 {
		remember(fmt.Sprintf("semantic-u-%d", i), "semantic", nil)
	}
	for i := range 5 {
		remember(fmt.Sprintf("procedural-u-%d", i), "procedural", nil)
	}
	for i := range 5 {
		remember(fmt.Sprintf("episodic-u-%d", i), "episodic", nil)
	}
	for i := range 3 {
		remember(fmt.Sprintf("semantic-p-%d", i), "semantic", []string{"pinned"})
	}
	for i := range 2 {
		remember(fmt.Sprintf("procedural-p-%d", i), "procedural", []string{"pinned"})
	}

	type result struct {
		Namespace  string                     `json:"namespace"`
		Facts      []struct{ Content string } `json:"facts"`
		Procedures []struct{ Content string } `json:"procedures"`
		Recent     []struct{ Content string } `json:"recent"`
		Pinned     []struct{ Content string } `json:"pinned"`
	}
	decode := func(rec *httptest.ResponseRecorder) result {
		var b result
		mustJSON(t, rec, &b)
		return b
	}

	// Verify the seed put pinned memories in storage before asserting on the
	// briefing — surfaces any tag-plumbing bug here rather than in the cap
	// assertion.
	// Verify the seed put pinned memories in storage before asserting on the
	// briefing — surfaces any tag-plumbing bug here rather than in the cap
	// assertion.
	listRec := do(t, h, http.MethodGet, "/v1/memories?tag=pinned", "proj", apiKey, nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list pinned: %d (%s)", listRec.Code, listRec.Body)
	}
	var listed struct {
		Memories []struct{ Content string } `json:"memories"`
	}
	mustJSON(t, listRec, &listed)
	if len(listed.Memories) != 5 {
		t.Fatalf("expected 5 pinned memories in storage, got %d", len(listed.Memories))
	}

	// Defaults: per_section=5 (omitted), all four sections capped at 5. Pinned
	// carries 5 candidates (3 semantic + 2 procedural) so it caps at 5.
	rec := do(t, h, http.MethodGet, "/v1/namespaces/briefing", "proj", apiKey, nil)
	b := decode(rec)
	if len(b.Facts) != 5 || len(b.Procedures) != 5 || len(b.Recent) != 5 || len(b.Pinned) != 5 {
		t.Fatalf("default per_section=5 should cap all sections to 5, got facts=%d procs=%d recent=%d pinned=%d",
			len(b.Facts), len(b.Procedures), len(b.Recent), len(b.Pinned))
	}

	// Per-section overrides win over per_section: pin 2, facts 1, procedures 0
	// (off), recent 3. Verifies that one section can be disabled (procs=0)
	// while others get custom caps.
	q := "/v1/namespaces/briefing?per_section=5&per_section_pinned=2&per_section_facts=1&per_section_procedures=0&per_section_recent=3"
	rec = do(t, h, http.MethodGet, q, "proj", apiKey, nil)
	b = decode(rec)
	if len(b.Pinned) != 2 || len(b.Facts) != 1 || len(b.Procedures) != 0 || len(b.Recent) != 3 {
		t.Fatalf("per-section caps off: pinned=%d facts=%d procs=%d recent=%d",
			len(b.Pinned), len(b.Facts), len(b.Procedures), len(b.Recent))
	}
}

func TestSearchMinScoreFilters(t *testing.T) {
	h := newServer(t)
	// Seed a strongly-relevant memory (exact topic match) and a weakly-
	// relevant one (unrelated content). Both rank on the same query; the
	// min_score floor drops the weak one without touching the strong.
	strong := "the kubernetes pod scheduler assigns nodes to containers"
	weak := "the bakery has fresh croissants every morning"
	remember := func(content, tier string) {
		rec := do(t, h, http.MethodPost, "/v1/memories", "ns", apiKey, map[string]any{
			"content": content, "tier": tier,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("remember %q: %d", content, rec.Code)
		}
	}
	remember(strong, "semantic")
	remember(weak, "semantic")

	search := func(body map[string]any) []string {
		rec := do(t, h, http.MethodPost, "/v1/search", "ns", apiKey, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("search: %d (%s)", rec.Code, rec.Body)
		}
		var sr struct {
			Results []struct {
				Memory struct{ Content string } `json:"memory"`
			} `json:"results"`
		}
		mustJSON(t, rec, &sr)
		contents := make([]string, 0, len(sr.Results))
		for _, r := range sr.Results {
			contents = append(contents, r.Memory.Content)
		}
		return contents
	}

	// No min_score: both should appear, strong first.
	both := search(map[string]any{"query": "kubernetes scheduler", "limit": 5})
	if len(both) != 2 {
		t.Fatalf("baseline recall should return both, got %d (%v)", len(both), both)
	}

	// A min_score high enough to drop the weak but not the strong: per-call
	// MinScore overrides the server-wide default gate (0.1), proving the
	// field is wired through to the recall floor.
	strongOnly := search(map[string]any{
		"query": "kubernetes scheduler", "limit": 5, "min_score": 0.5,
	})
	if len(strongOnly) != 1 || strongOnly[0] != strong {
		t.Fatalf("min_score=0.5 should keep only the strong match, got %v", strongOnly)
	}

	// min_score above any achievable fused score: empty result set.
	none := search(map[string]any{
		"query": "kubernetes scheduler", "limit": 5, "min_score": 2.0,
	})
	if len(none) != 0 {
		t.Fatalf("min_score=2.0 should drop everything, got %v", none)
	}
}

func TestRememberSearchForgetRoundTrip(t *testing.T) {
	h := newServer(t)

	// Remember.
	rec := do(t, h, http.MethodPost, "/v1/memories", "alice", apiKey, map[string]any{
		"content": "kubernetes schedules containers", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	mustJSON(t, rec, &created)
	if created.ID == "" {
		t.Fatal("no id returned")
	}

	// Search finds it.
	rec = do(t, h, http.MethodPost, "/v1/search", "alice", apiKey, map[string]any{
		"query": "kubernetes containers", "limit": 5,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("search: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var sr struct {
		Results []struct {
			Memory struct {
				ID      string `json:"id"`
				Content string `json:"content"`
			} `json:"memory"`
			Score float64 `json:"score"`
		} `json:"results"`
	}
	mustJSON(t, rec, &sr)
	if len(sr.Results) == 0 || sr.Results[0].Memory.ID != created.ID {
		t.Fatalf("search did not return the created memory: %+v", sr.Results)
	}

	// Different namespace sees nothing.
	rec = do(t, h, http.MethodPost, "/v1/search", "bob", apiKey, map[string]any{"query": "kubernetes"})
	mustJSON(t, rec, &sr)
	if len(sr.Results) != 0 {
		t.Fatalf("namespace isolation broken: %+v", sr.Results)
	}

	// Forget.
	rec = do(t, h, http.MethodDelete, "/v1/memories/"+created.ID, "alice", apiKey, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("forget: want 204, got %d", rec.Code)
	}
	rec = do(t, h, http.MethodGet, "/v1/memories/"+created.ID, "alice", apiKey, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after forget: want 404, got %d", rec.Code)
	}
}

// TestPercentEncodedID guards against the path-param decoding bug: ids with
// reserved characters like ':' (e.g. agentmemory's "openclaw:main:<uuid>")
// arrive percent-encoded from most HTTP clients, and must resolve to the same
// record as their literal form.
func TestPercentEncodedID(t *testing.T) {
	h := newServer(t)

	const id = "agm-sum-openclaw:main:9119fd27"
	rec := do(t, h, http.MethodPost, "/v1/memories", "alice", apiKey, map[string]any{
		"content": "imported session summary", "tier": "semantic", "id": id,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}

	// Many clients (Python's urllib, etc.) percent-encode ':' in a path segment
	// even though url.PathEscape leaves it literal, so encode it explicitly.
	encoded := "/v1/memories/" + strings.ReplaceAll(id, ":", "%3A")

	rec = do(t, h, http.MethodGet, encoded, "alice", apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get encoded id: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodDelete, encoded, "alice", apiKey, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete encoded id: want 204, got %d (%s)", rec.Code, rec.Body)
	}
}

func TestGetMemoryHistory(t *testing.T) {
	h := newServer(t)
	mk := func(content string) string {
		rec := do(t, h, http.MethodPost, "/v1/memories", "alice", apiKey, map[string]any{
			"content": content, "tier": "semantic",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("remember %q: want 201, got %d (%s)", content, rec.Code, rec.Body)
		}
		var created struct {
			ID string `json:"id"`
		}
		mustJSON(t, rec, &created)
		return created.ID
	}

	v1 := mk("office is in Paris")
	v2 := mk("office is in Berlin")
	rec := do(t, h, http.MethodPost, "/v1/memories/"+v1+"/supersede", "alice", apiKey, map[string]any{"by": v2})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("supersede: want 204, got %d (%s)", rec.Code, rec.Body)
	}

	// History of the live tip returns both versions, including the tombstoned one.
	rec = do(t, h, http.MethodGet, "/v1/memories/"+v2+"/history", "alice", apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("history: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var hist struct {
		Memories []struct {
			ID string `json:"id"`
		} `json:"memories"`
	}
	mustJSON(t, rec, &hist)
	if len(hist.Memories) != 2 {
		t.Fatalf("history length = %d, want 2: %s", len(hist.Memories), rec.Body)
	}

	// Unknown id is 404, not an empty list.
	rec = do(t, h, http.MethodGet, "/v1/memories/nope/history", "alice", apiKey, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("history of missing id: want 404, got %d", rec.Code)
	}
}

func TestListStatsNamespaces(t *testing.T) {
	h := newServer(t)

	// Seed two namespaces with a couple of tiers.
	seed := []struct {
		ns, content, tier string
	}{
		{"alice", "alice runs the deploy pipeline", "semantic"},
		{"alice", "alice debugged the cache today", "episodic"},
		{"bob", "bob owns the billing service", "semantic"},
	}
	for _, s := range seed {
		rec := do(t, h, http.MethodPost, "/v1/memories", s.ns, apiKey, map[string]any{
			"content": s.content, "tier": s.tier,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed remember: want 201, got %d (%s)", rec.Code, rec.Body)
		}
	}

	// List is namespace-scoped.
	rec := do(t, h, http.MethodGet, "/v1/memories", "alice", apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var lr struct {
		Memories []struct {
			ID   string `json:"id"`
			Tier string `json:"tier"`
		} `json:"memories"`
	}
	mustJSON(t, rec, &lr)
	if len(lr.Memories) != 2 {
		t.Fatalf("list alice: want 2 memories, got %d", len(lr.Memories))
	}

	// List with a tier filter narrows results.
	rec = do(t, h, http.MethodGet, "/v1/memories?tier=semantic", "alice", apiKey, nil)
	mustJSON(t, rec, &lr)
	if len(lr.Memories) != 1 || lr.Memories[0].Tier != "semantic" {
		t.Fatalf("list alice?tier=semantic: want 1 semantic, got %+v", lr.Memories)
	}

	// Stats reflect the namespace.
	rec = do(t, h, http.MethodGet, "/v1/stats", "alice", apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("stats: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var stats struct {
		Total  int            `json:"total"`
		ByTier map[string]int `json:"by_tier"`
	}
	mustJSON(t, rec, &stats)
	if stats.Total != 2 || stats.ByTier["semantic"] != 1 || stats.ByTier["episodic"] != 1 {
		t.Fatalf("stats alice: unexpected %+v", stats)
	}

	// Namespaces lists both tenants.
	rec = do(t, h, http.MethodGet, "/v1/namespaces", "", apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("namespaces: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var nsr struct {
		Namespaces []string `json:"namespaces"`
	}
	mustJSON(t, rec, &nsr)
	if !slices.Contains(nsr.Namespaces, "alice") || !slices.Contains(nsr.Namespaces, "bob") {
		t.Fatalf("namespaces: want alice and bob, got %v", nsr.Namespaces)
	}
}

// TestTagMetadataFilter pins the tag/metadata filter contract on both the
// query-less browse (GET /v1/memories?tag=&meta=) and the search (POST
// /v1/search) surfaces, plus the 400 on a malformed meta= pair.
func TestTagMetadataFilter(t *testing.T) {
	h := newServer(t)

	seed := []struct {
		content  string
		tags     []string
		category string
	}{
		{"fixed the auth race", []string{"bug", "auth"}, "bug_fixes"},
		{"auth handler latency", []string{"perf", "auth"}, "performance_findings"},
		{"unrelated note", nil, ""},
	}
	for _, s := range seed {
		body := map[string]any{"content": s.content, "tier": "semantic"}
		if s.tags != nil {
			body["tags"] = s.tags
		}
		if s.category != "" {
			body["metadata"] = map[string]any{"category": s.category}
		}
		rec := do(t, h, http.MethodPost, "/v1/memories", "alice", apiKey, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed: want 201, got %d (%s)", rec.Code, rec.Body)
		}
	}

	var lr struct {
		Memories []struct {
			Content string `json:"content"`
		} `json:"memories"`
	}

	// Single tag matches both auth memories.
	rec := do(t, h, http.MethodGet, "/v1/memories?tag=auth", "alice", apiKey, nil)
	mustJSON(t, rec, &lr)
	if len(lr.Memories) != 2 {
		t.Fatalf("?tag=auth: want 2, got %d (%+v)", len(lr.Memories), lr.Memories)
	}

	// Comma-separated tags are ANDed.
	rec = do(t, h, http.MethodGet, "/v1/memories?tag=auth,bug", "alice", apiKey, nil)
	mustJSON(t, rec, &lr)
	if len(lr.Memories) != 1 {
		t.Fatalf("?tag=auth,bug: want 1, got %d", len(lr.Memories))
	}

	// Metadata category narrows the browse.
	rec = do(t, h, http.MethodGet, "/v1/memories?meta=category=bug_fixes", "alice", apiKey, nil)
	mustJSON(t, rec, &lr)
	if len(lr.Memories) != 1 {
		t.Fatalf("?meta=category=bug_fixes: want 1, got %d", len(lr.Memories))
	}

	// A malformed meta filter is a 400, never silently unfiltered.
	rec = do(t, h, http.MethodGet, "/v1/memories?meta=noequals", "alice", apiKey, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("?meta=noequals: want 400, got %d (%s)", rec.Code, rec.Body)
	}

	// Search composes query + tag + metadata.
	var sr struct {
		Results []struct {
			Memory struct {
				Content string `json:"content"`
			} `json:"memory"`
		} `json:"results"`
	}
	rec = do(t, h, http.MethodPost, "/v1/search", "alice", apiKey, map[string]any{
		"query": "auth", "tags": []string{"perf"}, "metadata": map[string]string{"category": "performance_findings"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("search: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	mustJSON(t, rec, &sr)
	if len(sr.Results) != 1 || sr.Results[0].Memory.Content != "auth handler latency" {
		t.Fatalf("filtered search: want only the perf memory, got %+v", sr.Results)
	}
}

func mustJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body)
	}
}

// TestListQueryParamValidation pins the ?tier= / ?limit= contract: unknown
// tiers and unparseable or negative limits are 400s (never silently
// unfiltered results), and comma-separated tier values keep working alongside
// repeats as documented in the spec.
func TestListQueryParamValidation(t *testing.T) {
	h := newServer(t)

	for _, s := range []struct{ content, tier string }{
		{"semantic fact", "semantic"},
		{"episodic note", "episodic"},
		{"working scratch", "working"},
	} {
		rec := do(t, h, http.MethodPost, "/v1/memories", "alice", apiKey, map[string]any{
			"content": s.content, "tier": s.tier,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed: want 201, got %d (%s)", rec.Code, rec.Body)
		}
	}

	var lr struct {
		Memories []struct {
			Tier string `json:"tier"`
		} `json:"memories"`
	}

	// Comma-separated and repeated tier filters are equivalent.
	for _, q := range []string{"?tier=semantic,episodic", "?tier=semantic&tier=episodic"} {
		rec := do(t, h, http.MethodGet, "/v1/memories"+q, "alice", apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("list %s: want 200, got %d (%s)", q, rec.Code, rec.Body)
		}
		mustJSON(t, rec, &lr)
		if len(lr.Memories) != 2 {
			t.Fatalf("list %s: want 2 memories, got %d", q, len(lr.Memories))
		}
	}

	for _, q := range []string{"?tier=semantik", "?limit=abc", "?limit=-1"} {
		rec := do(t, h, http.MethodGet, "/v1/memories"+q, "alice", apiKey, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("list %s: want 400, got %d (%s)", q, rec.Code, rec.Body)
		}
	}

	rec := do(t, h, http.MethodGet, "/v1/memories?limit=1", "alice", apiKey, nil)
	mustJSON(t, rec, &lr)
	if len(lr.Memories) != 1 {
		t.Fatalf("limit=1: want 1 memory, got %d", len(lr.Memories))
	}
}

// TestAuthBeforeNamespaceValidation pins the middleware order: an
// unauthenticated request gets 401 even when its namespace header is also
// invalid, so callers can't probe validation behavior without a token.
func TestAuthBeforeNamespaceValidation(t *testing.T) {
	h := newServer(t)
	rec := do(t, h, http.MethodGet, "/v1/memories", strings.Repeat("n", 300), "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 before namespace validation, got %d (%s)", rec.Code, rec.Body)
	}
	// With a valid token the invalid namespace is then rejected with 400.
	rec = do(t, h, http.MethodGet, "/v1/memories", strings.Repeat("n", 300), apiKey, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for invalid namespace once authenticated, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestNamespacesEmptyStoreIsArray guards the wire format on a fresh install:
// namespaces must be [] (not null) so clients can call .length on it.
func TestNamespacesEmptyStoreIsArray(t *testing.T) {
	h := newServer(t)
	rec := do(t, h, http.MethodGet, "/v1/namespaces", "", apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("namespaces: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"namespaces":[]`) {
		t.Fatalf("empty store must marshal namespaces as [], got %s", rec.Body)
	}
}

// errEmbedder fails every embed call, forcing search onto the keyword-only
// fallback path (with a recall embed budget configured).
type errEmbedder struct{ dims int }

func (e errEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("embed boom")
}
func (e errEmbedder) Dims() int { return e.dims }

// TestSearchDegradedSurfacesKeywordOnly confirms that when the query embed
// fails and search falls back to keyword-only matching, the REST response
// carries "degraded":"keyword_only" (plus a note); a healthy embedder leaves
// both fields absent (omitempty), matching the MCP recall semantics.
func TestSearchDegradedSurfacesKeywordOnly(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "degraded.db")
	st, err := sqlitevec.Open(ctx, dbPath, dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Seed with a healthy embedder so keyword search has something to find.
	seed := service.New(st, embedtest.New(dims))
	if _, err := seed.Remember(ctx, service.RememberInput{
		Namespace: "ns", Content: "hello world", Tier: "semantic",
	}); err != nil {
		t.Fatalf("seed remember: %v", err)
	}

	mount := func(svc *service.Service) http.Handler {
		r := chi.NewRouter()
		rest.New(svc, rest.AuthConfig{
			APIKey: apiKey, NamespaceHeader: nsHdr, DefaultNamespace: "default",
		}).Mount(r)
		return r
	}

	t.Run("degraded", func(t *testing.T) {
		h := mount(service.New(st, errEmbedder{dims: dims}, service.WithRecallEmbedTimeout(time.Second)))
		rec := do(t, h, http.MethodPost, "/v1/search", "ns", apiKey, map[string]any{"query": "hello", "limit": 5})
		if rec.Code != http.StatusOK {
			t.Fatalf("search: want 200, got %d (%s)", rec.Code, rec.Body)
		}
		var sr map[string]any
		mustJSON(t, rec, &sr)
		if sr["degraded"] != "keyword_only" {
			t.Fatalf("degraded = %v, want %q", sr["degraded"], "keyword_only")
		}
		note, _ := sr["note"].(string)
		if note == "" {
			t.Fatal("note should explain the degradation, got empty")
		}
	})

	t.Run("healthy", func(t *testing.T) {
		h := mount(service.New(st, embedtest.New(dims)))
		rec := do(t, h, http.MethodPost, "/v1/search", "ns", apiKey, map[string]any{"query": "hello", "limit": 5})
		if rec.Code != http.StatusOK {
			t.Fatalf("search: want 200, got %d (%s)", rec.Code, rec.Body)
		}
		var sr map[string]any
		mustJSON(t, rec, &sr)
		if _, ok := sr["degraded"]; ok {
			t.Fatalf("degraded key should be omitted on healthy search, got %+v", sr)
		}
		if _, ok := sr["note"]; ok {
			t.Fatalf("note key should be omitted on healthy search, got %+v", sr)
		}
	})
}

// failingStore wraps a real store but fails every Upsert, to exercise the
// 5xx branch of the error mapping.
type failingStore struct {
	store.Store
}

func (failingStore) Upsert(context.Context, *memory.Memory) error {
	return errors.New("disk unavailable")
}

// TestErrorStatusMapping pins statusFor end-to-end: caller mistakes are 4xx,
// configuration gaps 503, backend failures 500. Before the classification fix
// every one of these was a 400.
func TestErrorStatusMapping(t *testing.T) {
	open := func(t *testing.T) store.Store {
		t.Helper()
		st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "rest.db"), dims)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st
	}
	mount := func(svc *service.Service) http.Handler {
		r := chi.NewRouter()
		rest.New(svc, rest.AuthConfig{
			APIKey: apiKey, NamespaceHeader: nsHdr, DefaultNamespace: "default",
		}).Mount(r)
		return r
	}

	t.Run("invalid input is 400", func(t *testing.T) {
		h := mount(service.New(open(t), embedtest.New(dims)))
		for _, body := range []map[string]any{
			{"content": ""},                   // missing content
			{"content": "x", "tier": "bogus"}, // unknown tier
		} {
			rec := do(t, h, http.MethodPost, "/v1/memories", "alice", apiKey, body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("remember %v: want 400, got %d (%s)", body, rec.Code, rec.Body)
			}
		}
	})

	t.Run("cross-namespace id conflict is 409", func(t *testing.T) {
		h := mount(service.New(open(t), embedtest.New(dims)))
		body := map[string]any{"content": "x", "id": "shared-id"}
		if rec := do(t, h, http.MethodPost, "/v1/memories", "alice", apiKey, body); rec.Code != http.StatusCreated {
			t.Fatalf("first remember: want 201, got %d (%s)", rec.Code, rec.Body)
		}
		rec := do(t, h, http.MethodPost, "/v1/memories", "bob", apiKey, body)
		if rec.Code != http.StatusConflict {
			t.Fatalf("cross-namespace id: want 409, got %d (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("missing embedder is 503", func(t *testing.T) {
		h := mount(service.New(open(t), embed.Disabled{D: dims}))
		rec := do(t, h, http.MethodPost, "/v1/search", "alice", apiKey, map[string]any{"query": "x"})
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("search without embedder: want 503, got %d (%s)", rec.Code, rec.Body)
		}
		rec = do(t, h, http.MethodPost, "/v1/memories", "alice", apiKey, map[string]any{"content": "x"})
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("remember without embedder: want 503, got %d (%s)", rec.Code, rec.Body)
		}
		// The 503 carries the actionable ErrDisabled message; only 500s are
		// scrubbed to a generic body.
		if !strings.Contains(rec.Body.String(), "MEMINI_EMBED_BASE_URL") {
			t.Errorf("503 body should keep the actionable embeddings-disabled message, got %s", rec.Body)
		}
	})

	t.Run("store failure is 500 and does not leak internals", func(t *testing.T) {
		h := mount(service.New(failingStore{open(t)}, embedtest.New(dims)))
		rec := do(t, h, http.MethodPost, "/v1/memories", "alice", apiKey, map[string]any{"content": "x"})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("remember with broken store: want 500, got %d (%s)", rec.Code, rec.Body)
		}
		// The wrapped internal error ("disk unavailable") must not cross the API
		// boundary; the client gets a generic message.
		body := rec.Body.String()
		if strings.Contains(body, "disk unavailable") {
			t.Errorf("500 body leaked the internal error chain: %s", body)
		}
		if !strings.Contains(body, "internal error") {
			t.Errorf("500 body should carry a generic message, got %s", body)
		}
	})
}

// TestDedupNamespaceScoping guards the isolation fix: POST /v1/dedup defaults
// to the caller's namespace and must not touch other tenants unless
// all_namespaces is set.
func TestDedupNamespaceScoping(t *testing.T) {
	h := newServer(t)

	// A near-duplicate pair per namespace (distinct content so write-time
	// fingerprint dedup keeps both, but similar enough for the dedup pass to
	// cluster them; exact restatements never reach the maintenance dedup).
	seed := func(ns string) {
		for _, c := range []string{"the sky is blue", "the sky is very blue"} {
			rec := do(t, h, http.MethodPost, "/v1/memories", ns, apiKey, map[string]any{
				"content": c, "tier": "semantic",
			})
			if rec.Code != http.StatusCreated {
				t.Fatalf("seed %s: want 201, got %d (%s)", ns, rec.Code, rec.Body)
			}
		}
	}
	seed("alice")
	seed("bob")

	dedup := func(ns string, body any) struct {
		Namespaces, Tombstoned, ClustersFound int
	} {
		rec := do(t, h, http.MethodPost, "/v1/dedup", ns, apiKey, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("dedup %s: want 200, got %d (%s)", ns, rec.Code, rec.Body)
		}
		var out struct {
			Namespaces    int `json:"namespaces"`
			Tombstoned    int `json:"tombstoned"`
			ClustersFound int `json:"clusters_found"`
		}
		mustJSON(t, rec, &out)
		return struct{ Namespaces, Tombstoned, ClustersFound int }{out.Namespaces, out.Tombstoned, out.ClustersFound}
	}

	// Scoped to alice: only her duplicate is collapsed; bob is untouched.
	got := dedup("alice", map[string]any{"similarity": 0.5})
	if got.Namespaces != 1 || got.ClustersFound != 1 || got.Tombstoned != 1 {
		t.Fatalf("scoped dedup: namespaces=%d clusters=%d tombstoned=%d, want 1/1/1",
			got.Namespaces, got.ClustersFound, got.Tombstoned)
	}

	// bob still has both live: a second scoped dedup on bob finds a fresh pair.
	got = dedup("bob", map[string]any{"similarity": 0.5})
	if got.Tombstoned != 1 {
		t.Fatalf("bob untouched by alice's dedup; scoped pass should collapse 1, got tombstoned=%d", got.Tombstoned)
	}
}

// TestDedupAllNamespacesDryRun covers the all_namespaces opt-in (overriding the
// default request-namespace scope) and dry-run (report actions, tombstone
// nothing) at the HTTP layer.
func TestDedupAllNamespacesDryRun(t *testing.T) {
	h := newServer(t)
	for _, ns := range []string{"alice", "bob"} {
		for _, c := range []string{"the sky is blue", "the sky is very blue"} {
			rec := do(t, h, http.MethodPost, "/v1/memories", ns, apiKey, map[string]any{
				"content": c, "tier": "semantic",
			})
			if rec.Code != http.StatusCreated {
				t.Fatalf("seed %s: want 201, got %d (%s)", ns, rec.Code, rec.Body)
			}
		}
	}

	// Dry-run over all namespaces from a single caller: reports both clusters,
	// tombstones nothing.
	rec := do(t, h, http.MethodPost, "/v1/dedup", "alice", apiKey, map[string]any{
		"similarity": 0.5, "all_namespaces": true, "dry_run": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("dedup: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var out struct {
		Namespaces int  `json:"namespaces"`
		Tombstoned int  `json:"tombstoned"`
		DryRun     bool `json:"dry_run"`
		Actions    []struct {
			RepresentativeID string   `json:"representative_id"`
			TombstonedIDs    []string `json:"tombstoned_ids"`
			Size             int      `json:"size"`
		} `json:"actions"`
	}
	mustJSON(t, rec, &out)
	if out.Namespaces != 2 || out.Tombstoned != 2 || !out.DryRun {
		t.Fatalf("all-namespaces dry-run: namespaces=%d tombstoned=%d dry_run=%v, want 2/2/true",
			out.Namespaces, out.Tombstoned, out.DryRun)
	}
	if len(out.Actions) != 2 {
		t.Fatalf("want 2 actions (one per namespace), got %d", len(out.Actions))
	}
	for _, a := range out.Actions {
		if a.RepresentativeID == "" || len(a.TombstonedIDs) != 1 || a.Size != 2 {
			t.Errorf("malformed action: %+v", a)
		}
	}

	// Dry-run committed nothing: a real scoped pass still finds the duplicate.
	rec = do(t, h, http.MethodPost, "/v1/dedup", "bob", apiKey, map[string]any{"similarity": 0.5})
	mustJSON(t, rec, &out)
	if out.Tombstoned != 1 {
		t.Fatalf("dry-run should not have tombstoned; real pass on bob wanted 1, got %d", out.Tombstoned)
	}
}

// TestSearchExplicitNamespaces pins that POST /v1/search with a namespaces
// body field REPLACES the default read set: results span exactly the listed
// namespaces (a third namespace never leaks in) and each result's memory
// carries its source namespace.
func TestSearchExplicitNamespaces(t *testing.T) {
	h := newServer(t)
	remember := func(ns, content string) {
		rec := do(t, h, http.MethodPost, "/v1/memories", ns, apiKey, map[string]any{
			"content": content, "tier": "semantic",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("remember %s: %d (%s)", ns, rec.Code, rec.Body)
		}
	}
	remember("team-a", "the deploy pipeline is written in Go")
	remember("team-b", "the CLI tooling is written in Go")
	remember("team-c", "this Go namespace must not leak into the read set")

	rec := do(t, h, http.MethodPost, "/v1/search", "team-a", apiKey, map[string]any{
		"query": "Go", "namespaces": []string{"team-a", "team-b"}, "limit": 10,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d (%s)", rec.Code, rec.Body)
	}
	var out struct {
		Results []struct {
			Memory struct {
				Content   string `json:"content"`
				Namespace string `json:"namespace"`
			} `json:"memory"`
		} `json:"results"`
	}
	mustJSON(t, rec, &out)
	got := map[string]bool{}
	for _, r := range out.Results {
		if r.Memory.Namespace == "" {
			t.Errorf("result %q missing namespace provenance", r.Memory.Content)
		}
		got[r.Memory.Namespace] = true
	}
	if !got["team-a"] || !got["team-b"] {
		t.Fatalf("explicit namespaces should span team-a and team-b, got %v", got)
	}
	if got["team-c"] {
		t.Fatalf("team-c is outside the explicit read set, got %v", got)
	}
}

// TestSearchNamespacesCapRejected pins that more than 16 namespaces entries
// maps to 400 invalid input, not 500.
func TestSearchNamespacesCapRejected(t *testing.T) {
	h := newServer(t)
	over := make([]string, 17)
	for i := range over {
		over[i] = fmt.Sprintf("ns-%d", i)
	}
	rec := do(t, h, http.MethodPost, "/v1/search", "alice", apiKey, map[string]any{
		"query": "x", "namespaces": over,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("17 namespaces: want 400, got %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "16") {
		t.Fatalf("error should mention the 16 entry cap, got %s", rec.Body)
	}
}

// TestGetBriefingSubtreeScope pins that ?scope=subtree also briefs namespaces
// nested under the path namespace, while the default (exact) does not.
func TestGetBriefingSubtreeScope(t *testing.T) {
	h := newServer(t)
	remember := func(ns, content string) {
		rec := do(t, h, http.MethodPost, "/v1/memories", ns, apiKey, map[string]any{
			"content": content, "tier": "semantic",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("remember %s: %d (%s)", ns, rec.Code, rec.Body)
		}
	}
	remember("proj", "shared: the service is written in Go")
	remember("proj/agent-a", "private: agent-a fact")

	type briefing struct {
		Facts []struct {
			Content   string `json:"content"`
			Namespace string `json:"namespace"`
		} `json:"facts"`
	}
	get := func(q string) briefing {
		rec := do(t, h, http.MethodGet, q, "proj", apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("briefing %s: %d (%s)", q, rec.Code, rec.Body)
		}
		var b briefing
		mustJSON(t, rec, &b)
		return b
	}

	exact := get("/v1/namespaces/briefing")
	if len(exact.Facts) != 1 {
		t.Fatalf("exact briefing should see only proj's fact, got %+v", exact.Facts)
	}

	sub := get("/v1/namespaces/briefing?scope=subtree")
	if len(sub.Facts) != 2 {
		t.Fatalf("subtree briefing should span proj and proj/agent-a, got %+v", sub.Facts)
	}
	got := map[string]bool{}
	for _, f := range sub.Facts {
		got[f.Namespace] = true
	}
	if !got["proj"] || !got["proj/agent-a"] {
		t.Fatalf("subtree facts should carry namespace provenance for both, got %v", got)
	}
}

// TestGetBriefingExplicitNamespaces pins that repeated ?namespaces= params
// REPLACE the default read set: only the listed namespaces are briefed and
// the path namespace is not force-added.
func TestGetBriefingExplicitNamespaces(t *testing.T) {
	h := newServer(t)
	remember := func(ns, content string) {
		rec := do(t, h, http.MethodPost, "/v1/memories", ns, apiKey, map[string]any{
			"content": content, "tier": "semantic",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("remember %s: %d (%s)", ns, rec.Code, rec.Body)
		}
	}
	remember("team-a", "fact in team-a")
	remember("team-b", "fact in team-b")
	remember("team-c", "fact in team-c")

	rec := do(t, h, http.MethodGet, "/v1/namespaces/briefing?namespaces=team-b&namespaces=team-c", "team-a", apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("briefing: %d (%s)", rec.Code, rec.Body)
	}
	var b struct {
		Facts []struct {
			Content   string `json:"content"`
			Namespace string `json:"namespace"`
		} `json:"facts"`
	}
	mustJSON(t, rec, &b)
	got := map[string]bool{}
	for _, f := range b.Facts {
		got[f.Namespace] = true
	}
	if got["team-a"] {
		t.Fatalf("explicit namespaces must not force-add the path namespace, got %v", got)
	}
	if !got["team-b"] || !got["team-c"] || len(b.Facts) != 2 {
		t.Fatalf("explicit namespaces should brief exactly team-b and team-c, got %+v", b.Facts)
	}
}

// TestGetBriefingInvalidScope pins that an unknown ?scope= value is 400.
func TestGetBriefingInvalidScope(t *testing.T) {
	h := newServer(t)
	rec := do(t, h, http.MethodGet, "/v1/namespaces/briefing?scope=bogus", "proj", apiKey, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad scope: want 400, got %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "subtree") {
		t.Fatalf("error should list the valid scopes, got %s", rec.Body)
	}
}

// TestGetBriefingNamespacesCapRejected pins that more than 16 ?namespaces=
// entries maps to 400 invalid input, not 500.
func TestGetBriefingNamespacesCapRejected(t *testing.T) {
	h := newServer(t)
	q := "/v1/namespaces/briefing?"
	for i := range 17 {
		q += fmt.Sprintf("namespaces=ns-%d&", i)
	}
	rec := do(t, h, http.MethodGet, strings.TrimSuffix(q, "&"), "proj", apiKey, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("17 namespaces: want 400, got %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "16") {
		t.Fatalf("error should mention the 16 entry cap, got %s", rec.Body)
	}
}

// TestSearchInvalidScope pins that /v1/search rejects an unknown scope value
// with 400 instead of silently searching as exact (mirrors GetBriefing).
func TestSearchInvalidScope(t *testing.T) {
	h := newServer(t)
	rec := do(t, h, http.MethodPost, "/v1/search", "proj", apiKey, map[string]any{
		"query": "x", "scope": "bogus",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad scope: want 400, got %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "subtree") {
		t.Fatalf("error should list the valid scopes, got %s", rec.Body)
	}
}

// TestReassignMemory covers the reassign endpoint's semantics: target
// normalization, 404 for an ID absent from the request namespace, and 400 for
// a same-namespace or pattern-bearing target.
func TestReassignMemory(t *testing.T) {
	h := newServer(t)

	rec := do(t, h, http.MethodPost, "/v1/memories", "alice", apiKey, map[string]any{
		"content": "movable fact", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("decode created: %v (%s)", err, rec.Body)
	}

	// The target is normalized before use: " bob/ " must land in "bob".
	rec = do(t, h, http.MethodPost, "/v1/memories/"+created.ID+"/reassign", "alice", apiKey,
		map[string]any{"to": " bob/ "})
	if rec.Code != http.StatusOK {
		t.Fatalf("reassign: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"moved":1`) {
		t.Fatalf("reassign: want moved:1, got %s", rec.Body)
	}
	rec = do(t, h, http.MethodGet, "/v1/memories/"+created.ID, "bob", apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("memory should be readable from bob after reassign, got %d (%s)", rec.Code, rec.Body)
	}

	// The ID now lives in bob, so reassigning it from alice is a 404 — both
	// stores skip absent IDs rather than erroring, so zero rows moved is the
	// only not-found signal.
	rec = do(t, h, http.MethodPost, "/v1/memories/"+created.ID+"/reassign", "alice", apiKey,
		map[string]any{"to": "carol"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reassign from wrong namespace: want 404, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPost, "/v1/memories/no-such-id/reassign", "alice", apiKey,
		map[string]any{"to": "carol"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reassign unknown id: want 404, got %d (%s)", rec.Code, rec.Body)
	}

	// Same-namespace and pattern-bearing targets are caller mistakes, not
	// silent no-ops.
	rec = do(t, h, http.MethodPost, "/v1/memories/"+created.ID+"/reassign", "bob", apiKey,
		map[string]any{"to": "bob"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reassign to same namespace: want 400, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPost, "/v1/memories/"+created.ID+"/reassign", "bob", apiKey,
		map[string]any{"to": "work/*"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reassign to pattern: want 400, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestSplitNamespaceNestedName pins that a nested (percent-encoded) namespace
// reaches the split operation decoded — without the unescape, split silently
// operates on the literal escaped string and reports an empty result.
// TestSplitNamespaceNestedName pins that split works for a hierarchical
// namespace ("work/memini"): the source comes from the X-Memini-Namespace
// header (not a %2F-encoded path segment), so it needs no path encoding.
func TestSplitNamespaceNestedName(t *testing.T) {
	h := newServer(t)

	rec := do(t, h, http.MethodPost, "/v1/memories", "work/memini", apiKey, map[string]any{
		"content": "pooled fact", "tier": "semantic",
		"metadata": map[string]any{"user_id": "work/erwan"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}

	// dry_run reports the grouping without moving anything.
	rec = do(t, h, http.MethodPost, "/v1/namespaces/split", "work/memini", apiKey,
		map[string]any{"dry_run": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("split dry-run: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var rep struct {
		Moved int `json:"moved"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode report: %v (%s)", err, rec.Body)
	}
	if rep.Moved != 1 {
		t.Fatalf("dry-run should report moved=1, got %s", rec.Body)
	}
	// The dry run must not have moved anything: the target namespace is empty.
	rec = do(t, h, http.MethodGet, "/v1/memories?limit=10", "work/erwan", apiKey, nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "pooled fact") {
		t.Fatalf("dry-run moved rows into the target namespace: %d (%s)", rec.Code, rec.Body)
	}

	rec = do(t, h, http.MethodPost, "/v1/namespaces/split", "work/memini", apiKey,
		map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("split: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	rep.Moved = 0
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode report: %v (%s)", err, rec.Body)
	}
	if rep.Moved != 1 {
		t.Fatalf("apply should move the pooled fact, got %s", rec.Body)
	}
	rec = do(t, h, http.MethodGet, "/v1/memories?limit=10", "work/erwan", apiKey, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pooled fact") {
		t.Fatalf("apply should land the fact in work/erwan: %d (%s)", rec.Code, rec.Body)
	}
}

// TestMoveNamespace covers the move endpoint: nested (percent-encoded) source
// decoding, target normalization, dry-run leaving rows in place, apply
// relocating them, and 400 for a same-namespace or pattern-bearing target.
func TestMoveNamespace(t *testing.T) {
	h := newServer(t)

	rec := do(t, h, http.MethodPost, "/v1/memories", "work/old", apiKey, map[string]any{
		"content": "relocatable fact", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}

	// dry_run reports without moving; the untrimmed target normalizes to work/new.
	rec = do(t, h, http.MethodPost, "/v1/namespaces/move", "work/old", apiKey,
		map[string]any{"to": " work/new/ ", "dry_run": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("move dry-run: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var rep struct {
		Moved int `json:"moved"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil || rep.Moved != 1 {
		t.Fatalf("move dry-run: want moved=1, got %s (%v)", rec.Body, err)
	}
	rec = do(t, h, http.MethodGet, "/v1/memories?limit=10", "work/new", apiKey, nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "relocatable fact") {
		t.Fatalf("dry-run must not move anything into work/new: %d (%s)", rec.Code, rec.Body)
	}

	// Apply relocates the memory to work/new.
	rec = do(t, h, http.MethodPost, "/v1/namespaces/move", "work/old", apiKey,
		map[string]any{"to": "work/new"})
	if rec.Code != http.StatusOK {
		t.Fatalf("move: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodGet, "/v1/memories?limit=10", "work/new", apiKey, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "relocatable fact") {
		t.Fatalf("apply should land the fact in work/new: %d (%s)", rec.Code, rec.Body)
	}

	// Same-namespace and pattern-bearing targets are caller mistakes.
	rec = do(t, h, http.MethodPost, "/v1/namespaces/move", "work/new", apiKey,
		map[string]any{"to": "work/new"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("move to same namespace: want 400, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPost, "/v1/namespaces/move", "work/new", apiKey,
		map[string]any{"to": "work/*"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("move to pattern: want 400, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestNamespaceHeaderCanonicalized pins that the namespace header is
// canonicalized at the middleware boundary: a memory written under a
// non-canonical header ("work/proj/") is readable under the canonical form
// ("work/proj"), so a trailing slash never splits a namespace in two.
func TestNamespaceHeaderCanonicalized(t *testing.T) {
	h := newServer(t)

	rec := do(t, h, http.MethodPost, "/v1/memories", "work/proj/", apiKey, map[string]any{
		"content": "canonicalized namespace fact", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var created struct {
		ID        string `json:"id"`
		Namespace string `json:"namespace"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if created.Namespace != "work/proj" {
		t.Fatalf("stored namespace = %q, want canonical %q", created.Namespace, "work/proj")
	}
	// The canonical header must reach the same record.
	rec = do(t, h, http.MethodGet, "/v1/memories/"+created.ID, "work/proj", apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("canonical read: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	// A slash-only header collapses to empty → falls back to the default namespace.
	rec = do(t, h, http.MethodPost, "/v1/search", "///", apiKey, map[string]any{"query": "x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("slash-only header should fall back to default, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestDeleteNamespaceHierarchical pins that deleting a hierarchical namespace
// works via the X-Memini-Namespace header (no %2F path segment): DELETE
// /v1/namespaces removes exactly the header namespace's memories.
func TestDeleteNamespaceHierarchical(t *testing.T) {
	h := newServer(t)

	for _, ns := range []string{"work/memini", "work/other"} {
		rec := do(t, h, http.MethodPost, "/v1/memories", ns, apiKey, map[string]any{
			"content": "fact in " + ns, "tier": "semantic",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("remember %s: want 201, got %d (%s)", ns, rec.Code, rec.Body)
		}
	}

	rec := do(t, h, http.MethodDelete, "/v1/namespaces", "work/memini", apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"deleted":1`) {
		t.Fatalf("delete: want deleted:1, got %s", rec.Body)
	}
	// The sibling namespace is untouched.
	rec = do(t, h, http.MethodGet, "/v1/memories?limit=10", "work/other", apiKey, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fact in work/other") {
		t.Fatalf("sibling must survive the delete: %d (%s)", rec.Code, rec.Body)
	}
}

// TestSearchHomeHeaderMergesDurable pins the transport half of the home
// cascade (T2 built the merge itself): a durable memory written to
// personal/kit — the caller's home namespace — surfaces in a /v1/search
// against an unrelated request namespace (acme/phoenix) as long as the
// caller sends X-Memini-Home: personal/kit. Without the header there is no
// home leg at all, so the same search must come back empty.
func TestSearchHomeHeaderMergesDurable(t *testing.T) {
	h := newServer(t)

	rec := doHome(t, h, http.MethodPost, "/v1/memories", "personal/kit", "", apiKey, map[string]any{
		"content": "jon's personal laptop ssh key is ed25519", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("remember to personal/kit: want 201, got %d (%s)", rec.Code, rec.Body)
	}

	// With the home header, acme/phoenix's search sees the personal/kit fact.
	rec = doHome(t, h, http.MethodPost, "/v1/search", "acme/phoenix", "personal/kit", apiKey, map[string]any{
		"query": "ssh key", "limit": 5,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("search with home header: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var got struct {
		Results []struct {
			Memory struct {
				Namespace string `json:"namespace"`
				Content   string `json:"content"`
			} `json:"memory"`
		} `json:"results"`
	}
	mustJSON(t, rec, &got)
	if len(got.Results) != 1 || got.Results[0].Memory.Namespace != "personal/kit" {
		t.Fatalf("search with X-Memini-Home should surface the home-namespace fact, got %+v", got.Results)
	}

	// Without the header, the same search from acme/phoenix sees nothing —
	// no home leg is added to the read set.
	rec = do(t, h, http.MethodPost, "/v1/search", "acme/phoenix", apiKey, map[string]any{
		"query": "ssh key", "limit": 5,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("search without home header: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	mustJSON(t, rec, &got)
	if len(got.Results) != 0 {
		t.Fatalf("search without X-Memini-Home must not see the home namespace, got %+v", got.Results)
	}
}

// TestHomeHeaderInvalidRejected pins that an invalid X-Memini-Home value is
// rejected with 400, matching X-Memini-Namespace's validation — a typo'd home
// header must never be silently treated as "no home leg".
func TestHomeHeaderInvalidRejected(t *testing.T) {
	h := newServer(t)
	badHome := strings.Repeat("n", 300)
	rec := doHome(t, h, http.MethodPost, "/v1/search", "acme/phoenix", badHome, apiKey, map[string]any{"query": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid home header: want 400, got %d (%s)", rec.Code, rec.Body)
	}
}

// answerStub is a stand-in llm.Completer for exercising POST /v1/answer
// without a real LLM backend: it returns a fixed response regardless of the
// prompt, which is enough to prove the grounding sources (not the generated
// text) reflect the home namespace.
type answerStub struct{ resp string }

func (a answerStub) Complete(context.Context, string, string) (string, error) {
	return a.resp, nil
}

// newServerWithAnswerer is newServer with an LLM answerer configured, so
// tests can exercise /v1/answer.
func newServerWithAnswerer(t *testing.T, opts ...service.Option) http.Handler {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "rest-answer.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := service.New(st, embedtest.New(dims), opts...)
	r := chi.NewRouter()
	rest.New(svc, rest.AuthConfig{
		APIKey: apiKey, NamespaceHeader: nsHdr, DefaultNamespace: "default", HomeHeader: homeHdr,
	}).Mount(r)
	return r
}

// TestAnswerHomeHeaderGroundsOnHomeNamespace pins gap G1 at the REST surface:
// POST /v1/answer forwards X-Memini-Home into service.AnswerInput.Home, so a
// durable fact that only exists in the caller's home namespace (personal/kit)
// shows up among the answer's grounding sources when asked from an unrelated
// request namespace (acme/phoenix) — and is absent with no home header.
func TestAnswerHomeHeaderGroundsOnHomeNamespace(t *testing.T) {
	h := newServerWithAnswerer(t, service.WithAnswerer(answerStub{resp: "ed25519"}))

	rec := doHome(t, h, http.MethodPost, "/v1/memories", "personal/kit", "", apiKey, map[string]any{
		"content": "jon's personal laptop ssh key is ed25519", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("remember to personal/kit: want 201, got %d (%s)", rec.Code, rec.Body)
	}

	rec = doHome(t, h, http.MethodPost, "/v1/answer", "acme/phoenix", "personal/kit", apiKey, map[string]any{
		"query": "what is the ssh key type", "limit": 5,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("answer with home header: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var got struct {
		Answer  string `json:"answer"`
		Sources []struct {
			Memory struct {
				Namespace string `json:"namespace"`
			} `json:"memory"`
		} `json:"sources"`
	}
	mustJSON(t, rec, &got)
	if len(got.Sources) != 1 || got.Sources[0].Memory.Namespace != "personal/kit" {
		t.Fatalf("answer sources should include the home-namespace memory, got %+v", got.Sources)
	}

	// Without the header, the same question from acme/phoenix has no source
	// at all — no home leg in the read set.
	rec = do(t, h, http.MethodPost, "/v1/answer", "acme/phoenix", apiKey, map[string]any{
		"query": "what is the ssh key type", "limit": 5,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("answer without home header: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	mustJSON(t, rec, &got)
	if len(got.Sources) != 0 {
		t.Fatalf("answer without X-Memini-Home must not see the home namespace, got %+v", got.Sources)
	}
}

// TestBriefingHomeHeaderMergesDurable mirrors TestSearchHomeHeaderMergesDurable
// for GET /v1/namespaces/briefing: a durable fact in personal/kit shows up in
// acme/phoenix's briefing only when X-Memini-Home is sent.
func TestBriefingHomeHeaderMergesDurable(t *testing.T) {
	h := newServer(t)

	rec := doHome(t, h, http.MethodPost, "/v1/memories", "personal/kit", "", apiKey, map[string]any{
		"content": "jon prefers tabs over spaces", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("remember to personal/kit: want 201, got %d (%s)", rec.Code, rec.Body)
	}

	rec = doHome(t, h, http.MethodGet, "/v1/namespaces/briefing", "acme/phoenix", "personal/kit", apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("briefing with home header: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var b struct {
		Facts []struct{ Content string } `json:"facts"`
	}
	mustJSON(t, rec, &b)
	found := false
	for _, f := range b.Facts {
		if f.Content == "jon prefers tabs over spaces" {
			found = true
		}
	}
	if !found {
		t.Fatalf("briefing with X-Memini-Home should include the home-namespace fact, got %+v", b.Facts)
	}

	rec = do(t, h, http.MethodGet, "/v1/namespaces/briefing", "acme/phoenix", apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("briefing without home header: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	// A fresh decode target: Facts is omitempty, so decoding an empty-facts
	// response into the same b as above would silently keep its stale value.
	var b2 struct {
		Facts []struct{ Content string } `json:"facts"`
	}
	mustJSON(t, rec, &b2)
	for _, f := range b2.Facts {
		if f.Content == "jon prefers tabs over spaces" {
			t.Fatalf("briefing without X-Memini-Home must not see the home namespace, got %+v", b2.Facts)
		}
	}
}
