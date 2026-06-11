package rest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	})

	t.Run("store failure is 500", func(t *testing.T) {
		h := mount(service.New(failingStore{open(t)}, embedtest.New(dims)))
		rec := do(t, h, http.MethodPost, "/v1/memories", "alice", apiKey, map[string]any{"content": "x"})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("remember with broken store: want 500, got %d (%s)", rec.Code, rec.Body)
		}
	})
}

// TestDedupNamespaceScoping guards the isolation fix: POST /v1/dedup defaults
// to the caller's namespace and must not touch other tenants unless
// all_namespaces is set.
func TestDedupNamespaceScoping(t *testing.T) {
	h := newServer(t)

	// Two identical pairs, one per namespace.
	seed := func(ns string) {
		for range 2 {
			rec := do(t, h, http.MethodPost, "/v1/memories", ns, apiKey, map[string]any{
				"content": "the sky is blue", "tier": "semantic",
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
		for range 2 {
			rec := do(t, h, http.MethodPost, "/v1/memories", ns, apiKey, map[string]any{
				"content": "the sky is blue", "tier": "semantic",
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
