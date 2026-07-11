package rest_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func hashOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// newServerWithKeyStore builds a REST server whose backing store implements
// store.APIKeyStore, wired into AuthConfig, so table-key auth is available
// alongside (or instead of) the admin env key. adminKey may be "".
func newServerWithKeyStore(t *testing.T, adminKey string) (http.Handler, store.APIKeyStore) {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "rest-apikey.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var ks store.APIKeyStore = st

	svc := service.New(st, embedtest.New(dims))
	r := chi.NewRouter()
	rest.New(svc, rest.AuthConfig{
		APIKey: adminKey, APIKeyStore: ks, NamespaceHeader: nsHdr, DefaultNamespace: "default", HomeHeader: homeHdr,
	}).Mount(r)
	return r, ks
}

func TestTableKeyAuthenticates(t *testing.T) {
	h, ks := newServerWithKeyStore(t, "")
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "bot", Hash: hashOf("tok-bot")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	rec := do(t, h, http.MethodPost, "/v1/memories", "alice", "tok-bot", map[string]any{
		"content": "a table-key write", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("table key remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}
}

func TestDisabledTableKeyRejected(t *testing.T) {
	h, ks := newServerWithKeyStore(t, "")
	if err := ks.PutAPIKey(context.Background(), store.APIKey{
		Name: "retired", Hash: hashOf("tok-retired"), Disabled: true,
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	rec := do(t, h, http.MethodPost, "/v1/search", "alice", "tok-retired", map[string]any{"query": "x"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled key: want 401, got %d (%s)", rec.Code, rec.Body)
	}
}

func TestUnknownTableKeyRejected(t *testing.T) {
	h, _ := newServerWithKeyStore(t, "")
	rec := do(t, h, http.MethodPost, "/v1/search", "alice", "no-such-token", map[string]any{"query": "x"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key: want 401, got %d (%s)", rec.Code, rec.Body)
	}
}

func TestAdminKeyStillWorksWithKeyStoreWired(t *testing.T) {
	h, _ := newServerWithKeyStore(t, apiKey)
	rec := do(t, h, http.MethodPost, "/v1/memories", "alice", apiKey, map[string]any{
		"content": "admin write", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin key: want 201, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestAuthDisabledModeUnchangedWithEmptyTable: no admin key configured and
// the api_keys table is wired but empty — a request with no bearer at all
// must still be allowed, exactly like the pre-K2 no-auth dev mode.
func TestAuthDisabledModeUnchangedWithEmptyTable(t *testing.T) {
	h, _ := newServerWithKeyStore(t, "")
	rec := do(t, h, http.MethodPost, "/v1/search", "alice", "", map[string]any{"query": "x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("empty table, no admin key, no bearer: want 200 (auth disabled), got %d (%s)", rec.Code, rec.Body)
	}
}

// TestNonEmptyTableRequiresAuth: once any table key exists, even with no
// admin key configured, an unauthenticated request must be rejected.
func TestNonEmptyTableRequiresAuth(t *testing.T) {
	h, ks := newServerWithKeyStore(t, "")
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "bot", Hash: hashOf("tok-bot")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	rec := do(t, h, http.MethodPost, "/v1/search", "alice", "", map[string]any{"query": "x"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-empty table, no bearer: want 401, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestBoundKeyHomeWinsNoHeader: a key bound to a home namespace surfaces that
// home's durable memories via the home leg even when the caller sends no
// X-Memini-Home header at all.
func TestBoundKeyHomeWinsNoHeader(t *testing.T) {
	h, ks := newServerWithKeyStore(t, "")
	if err := ks.PutAPIKey(context.Background(), store.APIKey{
		Name: "bound-bot", Hash: hashOf("tok-bound"), HomeNS: "acme/home",
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	// Seed a durable memory in the bound home namespace using the same key
	// (its own writes land in the request namespace, not the home namespace,
	// so seed directly via a request namespaced at acme/home).
	rec := do(t, h, http.MethodPost, "/v1/memories", "acme/home", "tok-bound", map[string]any{
		"content": "the vpn config lives in the team vault", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	// Recall from an unrelated namespace, no home header: the bound key's home
	// must still surface the seeded memory via the home leg.
	rec = do(t, h, http.MethodPost, "/v1/search", "acme/unrelated", "tok-bound", map[string]any{
		"query": "vpn config", "limit": 5,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("search: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var out struct {
		Results []struct {
			Memory struct {
				Namespace string `json:"namespace"`
			} `json:"memory"`
		} `json:"results"`
	}
	mustJSON(t, rec, &out)
	if len(out.Results) != 1 || out.Results[0].Memory.Namespace != "acme/home" {
		t.Fatalf("bound key home (no header): want the acme/home memory surfaced, got %+v", out.Results)
	}
}

// TestBoundKeyHomeWinsOverConflictingHeader: a conflicting X-Memini-Home
// header must be silently ignored (never a 400) for a bound key — the key's
// own home always wins.
func TestBoundKeyHomeWinsOverConflictingHeader(t *testing.T) {
	h, ks := newServerWithKeyStore(t, "")
	if err := ks.PutAPIKey(context.Background(), store.APIKey{
		Name: "bound-bot2", Hash: hashOf("tok-bound2"), HomeNS: "acme/home2",
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	rec := do(t, h, http.MethodPost, "/v1/memories", "acme/home2", "tok-bound2", map[string]any{
		"content": "the release runbook lives in confluence", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	// Conflicting header pointing elsewhere; must be ignored (log-and-ignore),
	// never a 400 and never actually switching the home leg.
	rec = doHome(t, h, http.MethodPost, "/v1/search", "acme/unrelated", "someone/elses/home", "tok-bound2", map[string]any{
		"query": "release runbook", "limit": 5,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("search with conflicting home header: want 200 (never 400), got %d (%s)", rec.Code, rec.Body)
	}
	var out struct {
		Results []struct {
			Memory struct {
				Namespace string `json:"namespace"`
			} `json:"memory"`
		} `json:"results"`
	}
	mustJSON(t, rec, &out)
	if len(out.Results) != 1 || out.Results[0].Memory.Namespace != "acme/home2" {
		t.Fatalf("bound key home must win over a conflicting header, got %+v", out.Results)
	}
}

// TestUnboundTableKeyRespectsHeader: a table key with no HomeNS behaves like
// the admin key — the X-Memini-Home header is honored normally.
func TestUnboundTableKeyRespectsHeader(t *testing.T) {
	h, ks := newServerWithKeyStore(t, "")
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "unbound-bot", Hash: hashOf("tok-unbound")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	rec := do(t, h, http.MethodPost, "/v1/memories", "acme/theirhome", "tok-unbound", map[string]any{
		"content": "the staging creds rotate weekly", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	rec = doHome(t, h, http.MethodPost, "/v1/search", "acme/unrelated", "acme/theirhome", "tok-unbound", map[string]any{
		"query": "staging creds", "limit": 5,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("search: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var out struct {
		Results []struct {
			Memory struct {
				Namespace string `json:"namespace"`
			} `json:"memory"`
		} `json:"results"`
	}
	mustJSON(t, rec, &out)
	if len(out.Results) != 1 || out.Results[0].Memory.Namespace != "acme/theirhome" {
		t.Fatalf("unbound key must respect the home header, got %+v", out.Results)
	}
}

// --- Per-key default namespace precedence ---

func TestKeyDefaultNamespaceUsedWhenNoHeader(t *testing.T) {
	h, ks := newServerWithKeyStore(t, "")
	if err := ks.PutAPIKey(context.Background(), store.APIKey{
		Name: "ns-bot", Hash: hashOf("tok-ns"), DefaultNS: "acme/keydefault",
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	rec := do(t, h, http.MethodPost, "/v1/memories", "", "tok-ns", map[string]any{
		"content": "lands in the key default namespace", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var m struct {
		Namespace string `json:"namespace"`
	}
	mustJSON(t, rec, &m)
	if m.Namespace != "acme/keydefault" {
		t.Fatalf("namespace = %q, want key DefaultNS acme/keydefault", m.Namespace)
	}
}

func TestHeaderOverridesKeyDefaultNamespace(t *testing.T) {
	h, ks := newServerWithKeyStore(t, "")
	if err := ks.PutAPIKey(context.Background(), store.APIKey{
		Name: "ns-bot2", Hash: hashOf("tok-ns2"), DefaultNS: "acme/keydefault2",
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	rec := do(t, h, http.MethodPost, "/v1/memories", "acme/explicit", "tok-ns2", map[string]any{
		"content": "the explicit header wins over the key default", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var m struct {
		Namespace string `json:"namespace"`
	}
	mustJSON(t, rec, &m)
	if m.Namespace != "acme/explicit" {
		t.Fatalf("namespace = %q, want the explicit header acme/explicit", m.Namespace)
	}
}

func TestNoKeyDefaultFallsBackToServerDefault(t *testing.T) {
	h, ks := newServerWithKeyStore(t, "")
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "plain-bot", Hash: hashOf("tok-plain")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	rec := do(t, h, http.MethodPost, "/v1/memories", "", "tok-plain", map[string]any{
		"content": "no key default, no header: falls to the server default", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var m struct {
		Namespace string `json:"namespace"`
	}
	mustJSON(t, rec, &m)
	if m.Namespace != "default" {
		t.Fatalf("namespace = %q, want the server default \"default\"", m.Namespace)
	}
}

// --- Attribution ---

func TestNamedKeyStampsAuthor(t *testing.T) {
	h, ks := newServerWithKeyStore(t, "")
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "author-bot", Hash: hashOf("tok-author")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	rec := do(t, h, http.MethodPost, "/v1/memories", "alice", "tok-author", map[string]any{
		"content": "a fact written by a named key", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var m struct {
		Metadata map[string]any `json:"metadata"`
	}
	mustJSON(t, rec, &m)
	if m.Metadata["author"] != "author-bot" {
		t.Fatalf("metadata.author = %v, want %q", m.Metadata["author"], "author-bot")
	}
}

func TestAdminKeyWriteHasNoAuthorStamp(t *testing.T) {
	h, _ := newServerWithKeyStore(t, apiKey)
	rec := do(t, h, http.MethodPost, "/v1/memories", "alice", apiKey, map[string]any{
		"content": "a fact written by the admin key", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var m struct {
		Metadata map[string]any `json:"metadata"`
	}
	mustJSON(t, rec, &m)
	if _, ok := m.Metadata["author"]; ok {
		t.Fatalf("admin key write must carry no author stamp, got metadata=%v", m.Metadata)
	}
}

func TestCallerSetAuthorPreserved(t *testing.T) {
	h, ks := newServerWithKeyStore(t, "")
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "author-bot2", Hash: hashOf("tok-author2")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	rec := do(t, h, http.MethodPost, "/v1/memories", "alice", "tok-author2", map[string]any{
		"content": "imported from elsewhere", "tier": "semantic",
		"metadata": map[string]any{"author": "original-author"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var m struct {
		Metadata map[string]any `json:"metadata"`
	}
	mustJSON(t, rec, &m)
	if m.Metadata["author"] != "original-author" {
		t.Fatalf("caller-set metadata.author must be preserved, got %v", m.Metadata["author"])
	}
}
