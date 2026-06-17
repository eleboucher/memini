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
	dims   = 64
	nsHdr  = "X-Memini-Namespace"
	apiKey = "secret-token"
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
		APIKey: apiKey, NamespaceHeader: nsHdr, DefaultNamespace: "default",
	}).Mount(r)
	return r
}

func do(t *testing.T, h http.Handler, method, path, ns, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if ns != "" {
		req.Header.Set(nsHdr, ns)
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

	rec := do(t, h, http.MethodGet, "/v1/namespaces/proj/briefing", "proj", apiKey, nil)
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
	for i := 0; i < 5; i++ {
		remember(fmt.Sprintf("semantic-u-%d", i), "semantic", nil)
	}
	for i := 0; i < 5; i++ {
		remember(fmt.Sprintf("procedural-u-%d", i), "procedural", nil)
	}
	for i := 0; i < 5; i++ {
		remember(fmt.Sprintf("episodic-u-%d", i), "episodic", nil)
	}
	for i := 0; i < 3; i++ {
		remember(fmt.Sprintf("semantic-p-%d", i), "semantic", []string{"pinned"})
	}
	for i := 0; i < 2; i++ {
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
	rec := do(t, h, http.MethodGet, "/v1/namespaces/proj/briefing", "proj", apiKey, nil)
	b := decode(rec)
	if len(b.Facts) != 5 || len(b.Procedures) != 5 || len(b.Recent) != 5 || len(b.Pinned) != 5 {
		t.Fatalf("default per_section=5 should cap all sections to 5, got facts=%d procs=%d recent=%d pinned=%d",
			len(b.Facts), len(b.Procedures), len(b.Recent), len(b.Pinned))
	}

	// Per-section overrides win over per_section: pin 2, facts 1, procedures 0
	// (off), recent 3. Verifies that one section can be disabled (procs=0)
	// while others get custom caps.
	q := "/v1/namespaces/proj/briefing?per_section=5&per_section_pinned=2&per_section_facts=1&per_section_procedures=0&per_section_recent=3"
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
