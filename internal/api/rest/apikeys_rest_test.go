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
	Name             string         `json:"name"`
	Home             string         `json:"home"`
	DefaultNamespace string         `json:"default_namespace"`
	CreatedAt        *time.Time     `json:"created_at"`
	Disabled         bool           `json:"disabled"`
	Source           string         `json:"source"`
	Settings         map[string]any `json:"settings"`
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
	if out.CreatedAt == nil || out.CreatedAt.IsZero() {
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

// TestCreateApiKeyNameWithSlashRejected400 pins a routing-safety guard: a
// name containing "/" would permanently strand the key, since chi's {name}
// path param on UpdateApiKey/DeleteApiKey/RotateApiKey only matches a single
// path segment (no unescapeID-style wildcard, unlike the memory {id} routes)
// -- "/v1/keys/acme/ci-bot" would never route back to such a key.
func TestCreateApiKeyNameWithSlashRejected400(t *testing.T) {
	h, _ := newKeysTestServer(t, "admin-secret", "")
	rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{"name": "acme/ci-bot"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("name with slash: want 400, got %d (%s)", rec.Code, rec.Body)
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
	if bySource["dbbot"].CreatedAt == nil {
		t.Error("dbbot (source=db) should have a populated created_at")
	}
	// A file-sourced key carries no creation timestamp at all (the file is
	// the source of truth, not a database row) -- emitting the Go zero time
	// here would render as a nonsensical "0001-01-01" in the UI rather than
	// being recognizably absent.
	if bySource["filebot"].CreatedAt != nil {
		t.Errorf("filebot (source=file) created_at should be omitted, got %v", bySource["filebot"].CreatedAt)
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
	if rotated.CreatedAt == nil || created.CreatedAt == nil || !rotated.CreatedAt.Equal(*created.CreatedAt) {
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

// --- Per-key settings (escaped defect: apiKeyModel/UpdateApiKey ignored
// store.APIKey.Settings even though the spec's ApiKey/UpdateApiKeyRequest
// schemas both carry the field) ---------------------------------------------

// TestListApiKeysIncludesSettings pins that GET /v1/keys surfaces a key's
// per-key settings override -- including a non-integral min-score value,
// which crosses the store's float64 <-> the wire's float32 boundary (see
// config_shared.go's float64PtrToFloat32) -- and that a key with no override
// at all omits the field entirely, exactly like home/default_namespace.
func TestListApiKeysIncludesSettings(t *testing.T) {
	h, _ := newConfigServer(t, "admin-secret", "", nil)
	rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{"name": "withsettings"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPatch, "/v1/keys/withsettings", "", "admin-secret", map[string]any{
		"settings": map[string]any{"capture_turns": false, "inject_pretool_min_score": 0.3},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch settings: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{"name": "nosettings"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create (no settings): want 201, got %d (%s)", rec.Code, rec.Body)
	}

	rec = do(t, h, http.MethodGet, "/v1/keys", "", "admin-secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var out struct {
		Keys []apiKeyDTO `json:"keys"`
	}
	mustJSON(t, rec, &out)
	byName := map[string]apiKeyDTO{}
	for _, k := range out.Keys {
		byName[k.Name] = k
	}

	got := byName["withsettings"]
	if got.Settings == nil {
		t.Fatal("GET /v1/keys must surface the key's settings override")
	}
	if v, _ := got.Settings["capture_turns"].(bool); v {
		t.Errorf("capture_turns = %v, want false", got.Settings["capture_turns"])
	}
	if v, _ := got.Settings["inject_pretool_min_score"].(float64); v != 0.3 {
		t.Errorf("inject_pretool_min_score = %v, want 0.3 (non-integral round trip)", got.Settings["inject_pretool_min_score"])
	}
	if byName["nosettings"].Settings != nil {
		t.Errorf("key with no settings override should omit settings, got %+v", byName["nosettings"].Settings)
	}
}

// TestUpdateApiKeySettingsReplaceFullyAndPreserveWhenOmitted pins the PATCH
// settings contract end to end: a present settings object REPLACES the whole
// stored blob (a field left out of the new blob is gone, not carried over --
// it re-inherits the global/default layers), an absent settings field
// preserves the existing blob untouched, an invalid blob is rejected 400
// before any write, and each successful settings edit is activity-logged
// (kind=settings), matching PUT /v1/self/settings.
func TestUpdateApiKeySettingsReplaceFullyAndPreserveWhenOmitted(t *testing.T) {
	h, _ := newConfigServer(t, "admin-secret", "", nil)
	rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{"name": "settingsbot"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create: want 201, got %d (%s)", rec.Code, rec.Body)
	}

	// First PATCH: sets two fields.
	rec = do(t, h, http.MethodPatch, "/v1/keys/settingsbot", "", "admin-secret", map[string]any{
		"settings": map[string]any{"capture_turns": false, "recall_limit": 5},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("first patch: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var out apiKeyDTO
	mustJSON(t, rec, &out)
	if v, _ := out.Settings["capture_turns"].(bool); v {
		t.Errorf("capture_turns = %v, want false", out.Settings["capture_turns"])
	}
	if v, _ := out.Settings["recall_limit"].(float64); v != 5 {
		t.Errorf("recall_limit = %v, want 5", out.Settings["recall_limit"])
	}

	// Second PATCH: a new blob that omits recall_limit -- full replace means
	// it must be GONE (re-inherits), not carried over from the first PATCH.
	// (A fresh DTO per decode: json.Unmarshal into an already-populated map
	// MERGES keys rather than replacing them, which would mask exactly the
	// bug this assertion exists to catch.)
	rec = do(t, h, http.MethodPatch, "/v1/keys/settingsbot", "", "admin-secret", map[string]any{
		"settings": map[string]any{"capture_turns": true},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("second patch: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	out = apiKeyDTO{}
	mustJSON(t, rec, &out)
	if v, _ := out.Settings["capture_turns"].(bool); !v {
		t.Errorf("capture_turns = %v, want true", out.Settings["capture_turns"])
	}
	if _, present := out.Settings["recall_limit"]; present {
		t.Errorf("recall_limit should be gone after full-replace, got %v", out.Settings["recall_limit"])
	}

	// Third PATCH: no settings field at all (only disabled) -- the blob must
	// survive completely unchanged.
	rec = do(t, h, http.MethodPatch, "/v1/keys/settingsbot", "", "admin-secret", map[string]any{"disabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("third patch: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	out = apiKeyDTO{}
	mustJSON(t, rec, &out)
	if v, _ := out.Settings["capture_turns"].(bool); !v {
		t.Errorf("settings must be preserved when the PATCH omits them, capture_turns = %v, want true", out.Settings["capture_turns"])
	}
	if _, present := out.Settings["recall_limit"]; present {
		t.Errorf("recall_limit should still be gone, got %v", out.Settings["recall_limit"])
	}

	// Invalid settings (auto_save_interval must be >= 1) -> 400, no write.
	rec = do(t, h, http.MethodPatch, "/v1/keys/settingsbot", "", "admin-secret", map[string]any{
		"settings": map[string]any{"auto_save_interval": 0},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid settings: want 400, got %d (%s)", rec.Code, rec.Body)
	}

	// The two successful settings edits above are activity-logged (kind=settings).
	rec = do(t, h, http.MethodGet, "/v1/activity?kind=settings&all_namespaces=true", "", "admin-secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("activity: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var act activityResponse
	mustJSON(t, rec, &act)
	if len(act.Events) != 2 {
		t.Fatalf("settings activity events = %d, want 2 (one per successful settings PATCH), got %+v", len(act.Events), act.Events)
	}
	for _, ev := range act.Events {
		if name, _ := ev.Detail["key_name"].(string); name != "settingsbot" {
			t.Errorf("event detail.key_name = %q, want settingsbot", name)
		}
	}
}

// TestUpdateApiKeyFileKeySettingsRejected409 confirms the file-key PATCH
// guard covers a settings-only edit too -- settings must flow through the
// same 409 guard as every other field, no new bypass.
func TestUpdateApiKeyFileKeySettingsRejected409(t *testing.T) {
	h, _ := newKeysTestServer(t, "admin-secret", `
keys:
  - name: filebot
    secret: "tok-filebot-settings"
`)
	rec := do(t, h, http.MethodPatch, "/v1/keys/filebot", "", "admin-secret", map[string]any{
		"settings": map[string]any{"capture_turns": false},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("patch file key settings: want 409, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestSelfReflectsAdminPatchedSettings proves the whole loop: an admin PATCH
// to a key's settings is what GET /v1/self (authenticated as that key) then
// reports, with provenance "key" -- not merely what the PATCH response itself
// echoes back.
func TestSelfReflectsAdminPatchedSettings(t *testing.T) {
	h, _ := newConfigServer(t, "admin-secret", "", nil)
	rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{"name": "selfbot"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var created apiKeyWithSecretDTO
	mustJSON(t, rec, &created)

	rec = do(t, h, http.MethodPatch, "/v1/keys/selfbot", "", "admin-secret", map[string]any{
		"settings": map[string]any{"capture_turns": false, "inject_pretool_min_score": 0.3},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch settings: want 200, got %d (%s)", rec.Code, rec.Body)
	}

	rec = do(t, h, http.MethodGet, "/v1/self", "", created.Secret, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/self: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var self selfRespDTO
	mustJSON(t, rec, &self)
	if v, _ := self.Settings["capture_turns"].(bool); v {
		t.Errorf("self capture_turns = %v, want false", self.Settings["capture_turns"])
	}
	if v, _ := self.Settings["inject_pretool_min_score"].(float64); v != 0.3 {
		t.Errorf("self inject_pretool_min_score = %v, want 0.3", self.Settings["inject_pretool_min_score"])
	}
	if self.SettingsSources["capture_turns"] != "key" {
		t.Errorf("capture_turns provenance = %q, want key", self.SettingsSources["capture_turns"])
	}
}

// TestRotateApiKeyPreservesAndExposesSettings pins that ApiKeyWithSecret (the
// create/rotate response shape) surfaces settings just like ApiKey does --
// same schema via allOf in the spec, same underlying store.APIKey.Settings
// field -- so rotate (which preserves a key's existing settings across the
// secret swap) must not silently drop them from the response.
func TestRotateApiKeyPreservesAndExposesSettings(t *testing.T) {
	h, _ := newKeysTestServer(t, "admin-secret", "")
	rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", map[string]any{"name": "rotatebot"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPatch, "/v1/keys/rotatebot", "", "admin-secret", map[string]any{
		"settings": map[string]any{"capture_turns": false},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch settings: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPost, "/v1/keys/rotatebot/rotate", "", "admin-secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var rotated apiKeyWithSecretDTO
	mustJSON(t, rec, &rotated)
	if rotated.Settings == nil {
		t.Fatal("rotate response must surface the preserved settings override")
	}
	if v, _ := rotated.Settings["capture_turns"].(bool); v {
		t.Errorf("rotated capture_turns = %v, want false", rotated.Settings["capture_turns"])
	}
}
