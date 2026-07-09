package rest_test

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/eleboucher/memini/internal/api/rest"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestNamespaceLinksCRUD drives the full create/list/overwrite/delete cycle
// through the REST surface.
func TestNamespaceLinksCRUD(t *testing.T) {
	h := newServer(t)

	// No links yet.
	rec := do(t, h, http.MethodGet, "/v1/namespaces/A/links", "", apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list (empty): want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var listed struct {
		Links []struct {
			Target    string `json:"target"`
			Tiers     string `json:"tiers"`
			CreatedAt string `json:"created_at"`
		} `json:"links"`
	}
	mustJSON(t, rec, &listed)
	if len(listed.Links) != 0 {
		t.Fatalf("list (empty) = %+v, want none", listed.Links)
	}

	// Create with an empty body: defaults to "durable".
	rec = do(t, h, http.MethodPut, "/v1/namespaces/A/links/B", "", apiKey, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("put (empty body): want 204, got %d (%s)", rec.Code, rec.Body)
	}

	rec = do(t, h, http.MethodGet, "/v1/namespaces/A/links", "", apiKey, nil)
	mustJSON(t, rec, &listed)
	if len(listed.Links) != 1 || listed.Links[0].Target != "B" || listed.Links[0].Tiers != "durable" {
		t.Fatalf("list after put = %+v, want one durable link to B", listed.Links)
	}

	// Idempotent overwrite: same pair, different tiers.
	rec = do(t, h, http.MethodPut, "/v1/namespaces/A/links/B", "", apiKey, map[string]any{"tiers": "all"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("put (overwrite): want 204, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodGet, "/v1/namespaces/A/links", "", apiKey, nil)
	mustJSON(t, rec, &listed)
	if len(listed.Links) != 1 || listed.Links[0].Tiers != "all" {
		t.Fatalf("list after overwrite = %+v, want a single link with tiers=all", listed.Links)
	}

	// Delete.
	rec = do(t, h, http.MethodDelete, "/v1/namespaces/A/links/B", "", apiKey, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d (%s)", rec.Code, rec.Body)
	}

	// Deleting an absent link is 404.
	rec = do(t, h, http.MethodDelete, "/v1/namespaces/A/links/B", "", apiKey, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete (absent): want 404, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestNamespaceLinksInvalidInput covers the 400 paths: bad tiers value,
// self-link.
func TestNamespaceLinksInvalidInput(t *testing.T) {
	h := newServer(t)

	rec := do(t, h, http.MethodPut, "/v1/namespaces/A/links/B", "", apiKey, map[string]any{"tiers": "bogus"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("put with invalid tiers: want 400, got %d (%s)", rec.Code, rec.Body)
	}

	rec = do(t, h, http.MethodPut, "/v1/namespaces/A/links/A", "", apiKey, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("put self-link: want 400, got %d (%s)", rec.Code, rec.Body)
	}
}

// noLinkStore wraps a store.Store, hiding any LinkStore methods the wrapped
// concrete type happens to implement (embedding the interface, not the
// concrete type, so LinkStore's methods aren't promoted) — simulates a
// backend without link support, to exercise the REST unsupported-backend
// (501) path against a real handler chain instead of only at the service
// layer.
type noLinkStore struct{ store.Store }

// TestNamespaceLinksUnsupportedBackend: a backend that doesn't implement
// store.LinkStore returns 501 with the standard error body on every link
// operation, and reads are unaffected (covered separately at the service
// layer — resolveDefaultReadSet simply skips the link lookup when linkStore
// is nil).
func TestNamespaceLinksUnsupportedBackend(t *testing.T) {
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "nolink.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := service.New(&noLinkStore{Store: st}, embedtest.New(dims))
	r := chi.NewRouter()
	rest.New(svc, rest.AuthConfig{
		APIKey: apiKey, NamespaceHeader: nsHdr, DefaultNamespace: "default",
	}).Mount(r)

	rec := do(t, r, http.MethodGet, "/v1/namespaces/A/links", "", apiKey, nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("list on unsupported backend: want 501, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, r, http.MethodPut, "/v1/namespaces/A/links/B", "", apiKey, nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("put on unsupported backend: want 501, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, r, http.MethodDelete, "/v1/namespaces/A/links/B", "", apiKey, nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("delete on unsupported backend: want 501, got %d (%s)", rec.Code, rec.Body)
	}
}
