package rest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/eleboucher/memini/internal/api/rest"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/service"
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
