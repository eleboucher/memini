package rest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func mustJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body)
	}
}
