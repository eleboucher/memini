package rest_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/eleboucher/memini/internal/api/rest"
	"github.com/eleboucher/memini/internal/apiauth"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// newKeysTestServer builds a REST server with a real APIKeyStore-backed
// store (K3b's /v1/keys surface reads AND writes, unlike apikey_test.go's
// auth-only fixtures) and, optionally, a MEMINI_API_KEYS_FILE-equivalent
// FileKeySet loaded from fileYAML ("" for none — see writeKeysFile in
// filekey_test.go, reused here).
func newKeysTestServer(t *testing.T, adminKey, fileYAML string) (http.Handler, store.APIKeyStore) {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "rest-keys.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var ks store.APIKeyStore = st

	var fk *apiauth.FileKeySet
	if fileYAML != "" {
		fk, err = apiauth.LoadFileKeys(writeKeysFile(t, fileYAML))
		if err != nil {
			t.Fatalf("LoadFileKeys: %v", err)
		}
	}

	svc := service.New(st, embedtest.New(dims))
	r := chi.NewRouter()
	rest.New(svc, rest.AuthConfig{
		APIKey: adminKey, APIKeyStore: ks, FileKeys: fk,
		NamespaceHeader: nsHdr, DefaultNamespace: "default", HomeHeader: homeHdr,
	}).Mount(r)
	return r, ks
}

type apiKeyDTO struct {
	Name             string    `json:"name"`
	Home             string    `json:"home"`
	DefaultNamespace string    `json:"default_namespace"`
	CreatedAt        time.Time `json:"created_at"`
	Disabled         bool      `json:"disabled"`
	Source           string    `json:"source"`
}

type apiKeyWithSecretDTO struct {
	apiKeyDTO
	Secret string `json:"secret"`
}

// --- Admin gating (binding rule): allowed for admin key or dev mode, 403 for a named principal ---

// TestNamedPrincipal403OnEveryKeysEndpoint pins the K3b admin gating rule: a
// request authenticated by a NAMED table key (never the admin key, never dev
// mode) must be refused with 403 on every /v1/keys operation, even though
// that same key is a perfectly valid credential for the ordinary memory API.
func TestNamedPrincipal403OnEveryKeysEndpoint(t *testing.T) {
	h, ks := newKeysTestServer(t, "admin-secret", "")
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "named-bot", Hash: hashOf("tok-named")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	// Target key for the mutating endpoints, created via the admin key.
	rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{"name": "target"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create (admin): want 201, got %d (%s)", rec.Code, rec.Body)
	}

	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/keys", nil},
		{http.MethodPost, "/v1/keys", map[string]any{"name": "other"}},
		{http.MethodPatch, "/v1/keys/target", map[string]any{"disabled": true}},
		{http.MethodDelete, "/v1/keys/target", nil},
		{http.MethodPost, "/v1/keys/target/rotate", nil},
	}
	for _, c := range cases {
		rec := do(t, h, c.method, c.path, "", "tok-named", c.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with a named principal: want 403, got %d (%s)", c.method, c.path, rec.Code, rec.Body)
		}
	}
}

// TestAdminKeyAllowedOnKeysEndpoints: the admin key (case (a) of the gating
// rule) can reach every /v1/keys operation.
func TestAdminKeyAllowedOnKeysEndpoints(t *testing.T) {
	h, _ := newKeysTestServer(t, "admin-secret", "")
	rec := do(t, h, http.MethodGet, "/v1/keys", "", "admin-secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET /v1/keys: want 200, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestDevModeAllowedOnKeysEndpoints: case (b) of the gating rule — no admin
// key configured and an empty table (nothing requires auth yet) — must also
// reach /v1/keys with no bearer at all. This is exactly the UI-first
// bootstrap precondition.
func TestDevModeAllowedOnKeysEndpoints(t *testing.T) {
	h, _ := newKeysTestServer(t, "", "")
	rec := do(t, h, http.MethodGet, "/v1/keys", "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dev mode GET /v1/keys: want 200, got %d (%s)", rec.Code, rec.Body)
	}
}

// --- CRUD happy paths ---

func TestCreateApiKeyHappyPath(t *testing.T) {
	h, ks := newKeysTestServer(t, "admin-secret", "")
	rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{
		"name": "alice", "home": "acme/alice", "default_namespace": "acme/default",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var out apiKeyWithSecretDTO
	mustJSON(t, rec, &out)
	if out.Secret == "" {
		t.Fatal("create response must include the secret")
	}
	if out.Name != "alice" || out.Home != "acme/alice" || out.DefaultNamespace != "acme/default" {
		t.Fatalf("unexpected key metadata: %+v", out)
	}
	if out.Source != "db" {
		t.Fatalf("source = %q, want db", out.Source)
	}
	if out.CreatedAt.IsZero() {
		t.Fatal("created_at should be populated")
	}

	found, err := ks.GetAPIKeyByHash(context.Background(), apiauth.HashToken(out.Secret))
	if err != nil {
		t.Fatalf("GetAPIKeyByHash: %v", err)
	}
	if found == nil || found.Name != "alice" {
		t.Fatalf("secret should authenticate to the stored key, got %+v", found)
	}
}

func TestCreateApiKeyInvalidNamespace400(t *testing.T) {
	h, _ := newKeysTestServer(t, "admin-secret", "")
	rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{
		"name": "bad", "home": strings.Repeat("x", 300),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid home: want 400, got %d (%s)", rec.Code, rec.Body)
	}
}

func TestCreateApiKeyEmptyName400(t *testing.T) {
	h, _ := newKeysTestServer(t, "admin-secret", "")
	rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{"name": "   "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name: want 400, got %d (%s)", rec.Code, rec.Body)
	}
}

func TestCreateApiKeyDuplicateTableNameConflicts(t *testing.T) {
	h, _ := newKeysTestServer(t, "admin-secret", "")
	rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{"name": "dup"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{"name": "dup"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate name (table): want 409, got %d (%s)", rec.Code, rec.Body)
	}
}

func TestCreateApiKeyDuplicateFileNameConflicts(t *testing.T) {
	h, _ := newKeysTestServer(t, "admin-secret", `
keys:
  - name: fromfile
    secret: "tok-fromfile"
`)
	rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{"name": "fromfile"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate name (file): want 409, got %d (%s)", rec.Code, rec.Body)
	}
}

func TestListApiKeysIncludesFileKeys(t *testing.T) {
	h, ks := newKeysTestServer(t, "admin-secret", `
keys:
  - name: filebot
    secret: "tok-filebot"
    home: acme/filehome
`)
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "dbbot", Hash: hashOf("tok-dbbot")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	rec := do(t, h, http.MethodGet, "/v1/keys", "", "admin-secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var out struct {
		Keys []apiKeyDTO `json:"keys"`
	}
	mustJSON(t, rec, &out)
	if len(out.Keys) != 2 {
		t.Fatalf("want 2 keys (1 db + 1 file), got %d: %+v", len(out.Keys), out.Keys)
	}
	bySource := map[string]apiKeyDTO{}
	for _, k := range out.Keys {
		bySource[k.Name] = k
	}
	if bySource["dbbot"].Source != "db" {
		t.Errorf("dbbot source = %q, want db", bySource["dbbot"].Source)
	}
	if bySource["filebot"].Source != "file" {
		t.Errorf("filebot source = %q, want file", bySource["filebot"].Source)
	}
	if bySource["filebot"].Home != "acme/filehome" {
		t.Errorf("filebot home = %q, want acme/filehome", bySource["filebot"].Home)
	}
}

// TestListAndGetNeverExposeSecretOrHash: the list response body must never
// contain a "secret" or "hash" field — not just absent from the generated
// schema, but actually verified against the wire JSON.
func TestListNeverExposesSecretOrHash(t *testing.T) {
	h, ks := newKeysTestServer(t, "admin-secret", "")
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "bot", Hash: hashOf("tok-bot")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	rec := do(t, h, http.MethodGet, "/v1/keys", "", "admin-secret", nil)
	body := rec.Body.String()
	if strings.Contains(body, "\"secret\"") || strings.Contains(body, "\"hash\"") {
		t.Fatalf("list response must never contain a secret or hash field, got: %s", body)
	}
	if strings.Contains(body, hashOf("tok-bot")) {
		t.Fatalf("list response must never contain the raw hash value, got: %s", body)
	}
}

// --- PATCH preserve-unspecified matrix (mirrors the CLI's key_test.go) ---

func TestUpdateApiKeyPreservesUnspecifiedFields(t *testing.T) {
	h, _ := newKeysTestServer(t, "admin-secret", "")
	rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{
		"name": "ivy", "home": "acme/phoenix", "default_namespace": "acme/default", "disabled": true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create: want 201, got %d (%s)", rec.Code, rec.Body)
	}

	// Patch with an empty body: every field should survive unchanged.
	rec = do(t, h, http.MethodPatch, "/v1/keys/ivy", "", "admin-secret", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch (no fields): want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var out apiKeyDTO
	mustJSON(t, rec, &out)
	if out.Home != "acme/phoenix" {
		t.Errorf("unspecified home must survive, got %q", out.Home)
	}
	if out.DefaultNamespace != "acme/default" {
		t.Errorf("unspecified default_namespace must survive, got %q", out.DefaultNamespace)
	}
	if !out.Disabled {
		t.Error("unspecified disabled must survive as true")
	}
}

func TestUpdateApiKeyExplicitEmptyHomeClears(t *testing.T) {
	h, _ := newKeysTestServer(t, "admin-secret", "")
	rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{
		"name": "judy", "home": "acme/phoenix", "default_namespace": "acme/default",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPatch, "/v1/keys/judy", "", "admin-secret", map[string]any{"home": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch explicit empty home: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var out apiKeyDTO
	mustJSON(t, rec, &out)
	if out.Home != "" {
		t.Errorf("explicit empty home must clear the binding, got %q", out.Home)
	}
	if out.DefaultNamespace != "acme/default" {
		t.Errorf("unspecified default_namespace must survive, got %q", out.DefaultNamespace)
	}
}

func TestUpdateApiKeyExplicitEnableReEnables(t *testing.T) {
	h, _ := newKeysTestServer(t, "admin-secret", "")
	rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{"name": "kim", "disabled": true})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPatch, "/v1/keys/kim", "", "admin-secret", map[string]any{"disabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch explicit disabled=false: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var out apiKeyDTO
	mustJSON(t, rec, &out)
	if out.Disabled {
		t.Error("explicit disabled=false must re-enable the key")
	}
}

func TestUpdateApiKeyNotFound404(t *testing.T) {
	h, _ := newKeysTestServer(t, "admin-secret", "")
	rec := do(t, h, http.MethodPatch, "/v1/keys/ghost", "", "admin-secret", map[string]any{"disabled": true})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("patch missing key: want 404, got %d (%s)", rec.Code, rec.Body)
	}
}

func TestUpdateApiKeyFileKeyRejected409(t *testing.T) {
	h, _ := newKeysTestServer(t, "admin-secret", `
keys:
  - name: filebot
    secret: "tok-filebot2"
`)
	rec := do(t, h, http.MethodPatch, "/v1/keys/filebot", "", "admin-secret", map[string]any{"disabled": true})
	if rec.Code != http.StatusConflict {
		t.Fatalf("patch file key: want 409, got %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "MEMINI_API_KEYS_FILE") {
		t.Errorf("error should mention MEMINI_API_KEYS_FILE, got: %s", rec.Body.String())
	}
}

// --- Delete ---

func TestDeleteApiKeyHappyPath(t *testing.T) {
	h, ks := newKeysTestServer(t, "admin-secret", "")
	rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{"name": "erin"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodDelete, "/v1/keys/erin", "", "admin-secret", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d (%s)", rec.Code, rec.Body)
	}
	keys, err := ks.ListAPIKeys(context.Background())
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("key should be gone, got %+v", keys)
	}
}

func TestDeleteApiKeyNotFound404(t *testing.T) {
	h, _ := newKeysTestServer(t, "admin-secret", "")
	rec := do(t, h, http.MethodDelete, "/v1/keys/ghost", "", "admin-secret", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing key: want 404, got %d (%s)", rec.Code, rec.Body)
	}
}

func TestDeleteApiKeyFileKeyRejected409(t *testing.T) {
	h, _ := newKeysTestServer(t, "admin-secret", `
keys:
  - name: filebot
    secret: "tok-filebot3"
`)
	rec := do(t, h, http.MethodDelete, "/v1/keys/filebot", "", "admin-secret", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete file key: want 409, got %d (%s)", rec.Code, rec.Body)
	}
}

// --- Rotate ---

func TestRotateApiKeyHappyPath(t *testing.T) {
	h, ks := newKeysTestServer(t, "admin-secret", "")
	rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{
		"name": "bob", "home": "acme/bob", "disabled": false,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var created apiKeyWithSecretDTO
	mustJSON(t, rec, &created)
	time.Sleep(2 * time.Millisecond) // guard against a same-instant CreatedAt false-passing the assertion below

	rec = do(t, h, http.MethodPost, "/v1/keys/bob/rotate", "", "admin-secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var rotated apiKeyWithSecretDTO
	mustJSON(t, rec, &rotated)

	if rotated.Secret == "" || rotated.Secret == created.Secret {
		t.Fatal("rotate must return a new, different secret")
	}
	if rotated.Home != "acme/bob" {
		t.Errorf("rotate must preserve home, got %q", rotated.Home)
	}
	if !rotated.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("rotate must preserve created_at: got %v, want %v", rotated.CreatedAt, created.CreatedAt)
	}
	if rotated.Disabled {
		t.Error("rotate must preserve disabled=false")
	}

	ctx := context.Background()
	if found, err := ks.GetAPIKeyByHash(ctx, apiauth.HashToken(created.Secret)); err != nil {
		t.Fatalf("GetAPIKeyByHash(old): %v", err)
	} else if found != nil {
		t.Fatalf("old secret must no longer authenticate after rotation, got %+v", found)
	}
	if found, err := ks.GetAPIKeyByHash(ctx, apiauth.HashToken(rotated.Secret)); err != nil {
		t.Fatalf("GetAPIKeyByHash(new): %v", err)
	} else if found == nil || found.Name != "bob" {
		t.Fatalf("new secret must authenticate, got %+v", found)
	}
}

func TestRotateApiKeyNotFound404(t *testing.T) {
	h, _ := newKeysTestServer(t, "admin-secret", "")
	rec := do(t, h, http.MethodPost, "/v1/keys/ghost/rotate", "", "admin-secret", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("rotate missing key: want 404, got %d (%s)", rec.Code, rec.Body)
	}
}

func TestRotateApiKeyFileKeyRejected409(t *testing.T) {
	h, _ := newKeysTestServer(t, "admin-secret", `
keys:
  - name: filebot
    secret: "tok-filebot4"
`)
	rec := do(t, h, http.MethodPost, "/v1/keys/filebot/rotate", "", "admin-secret", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("rotate file key: want 409, got %d (%s)", rec.Code, rec.Body)
	}
}

// --- Cache invalidation / bootstrap end-to-end (binding requirement) ---

// TestBootstrapFlowEndToEnd is the headline K3b guarantee: with no admin key
// configured and an empty table (dev mode), creating the first key via REST
// must enforce auth IMMEDIATELY afterward — no waiting out
// apiauth's ~10s table-emptiness cache — proving the mutating handler calls
// apiauth.Config.Invalidate. Before that hook existed, a request sent right
// after creation would still sail through unauthenticated for up to the TTL.
func TestBootstrapFlowEndToEnd(t *testing.T) {
	h, _ := newKeysTestServer(t, "", "")

	// Dev mode: unauthenticated requests work before any key exists.
	rec := do(t, h, http.MethodPost, "/v1/search", "acme", "", map[string]any{"query": "x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-bootstrap search with no bearer: want 200, got %d (%s)", rec.Code, rec.Body)
	}

	// Bootstrap: create the first key while auth is still open.
	rec = do(t, h, http.MethodPost, "/v1/keys", "", "", map[string]any{"name": "first-admin"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap create: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var created apiKeyWithSecretDTO
	mustJSON(t, rec, &created)
	if created.Secret == "" {
		t.Fatal("bootstrap create must return a secret")
	}

	// Immediately afterward (same test, no sleep): a request with no bearer
	// must now be rejected -- proves the cache was invalidated, not merely
	// that it will expire eventually.
	rec = do(t, h, http.MethodPost, "/v1/search", "acme", "", map[string]any{"query": "x"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("post-bootstrap search with no bearer: want 401 immediately, got %d (%s)", rec.Code, rec.Body)
	}

	// And the freshly minted key authenticates right away.
	rec = do(t, h, http.MethodPost, "/v1/search", "acme", created.Secret, map[string]any{"query": "x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("post-bootstrap search with the new key: want 200, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestDeleteApiKeyInvalidatesCacheImmediately pins the delete-side half of
// the cache-invalidation binding: deleting a table key over REST must call
// apiauth.Config.Invalidate so a request presenting that key's now-dead
// secret is rejected right away. (Deleting a specific row's own credential
// is actually always live — GetAPIKeyByHash is queried fresh every request,
// never cached; only the table-EMPTINESS reading behind the pure-dev-mode,
// no-bearer fallback is cached. That reading can only ever flip
// observably when no admin key is configured — an admin key configured
// makes the no-bearer path reject unconditionally regardless of table state,
// see apiauth.Config.Authenticate — and, once any key exists, /v1/keys
// itself requires admin-or-dev auth, so deleting the LAST key with no admin
// key configured is architecturally unreachable through REST alone: doing
// so needs the CLI's direct store access instead, which is the documented
// TTL-lag exception. This test instead exercises the reachable, meaningful
// half: the deleted key's credential must stop authenticating immediately,
// proving the delete handler ran and didn't leave anything the emptiness
// cache could paper over.)
func TestDeleteApiKeyInvalidatesCacheImmediately(t *testing.T) {
	h, ks := newKeysTestServer(t, "admin-secret", "")
	rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{"name": "victim"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var created apiKeyWithSecretDTO
	mustJSON(t, rec, &created)

	// The freshly created key authenticates against the ordinary memory API.
	rec = do(t, h, http.MethodPost, "/v1/search", "acme", created.Secret, map[string]any{"query": "x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-delete search with the key: want 200, got %d (%s)", rec.Code, rec.Body)
	}

	rec = do(t, h, http.MethodDelete, "/v1/keys/victim", "", "admin-secret", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d (%s)", rec.Code, rec.Body)
	}

	// Immediately afterward: the deleted key's secret must be dead.
	rec = do(t, h, http.MethodPost, "/v1/search", "acme", created.Secret, map[string]any{"query": "x"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("post-delete search with the deleted key: want 401 immediately, got %d (%s)", rec.Code, rec.Body)
	}
	if keys, err := ks.ListAPIKeys(context.Background()); err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	} else if len(keys) != 0 {
		t.Fatalf("key should be gone, got %+v", keys)
	}
}
