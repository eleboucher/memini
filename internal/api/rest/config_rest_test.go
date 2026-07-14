package rest_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/eleboucher/memini/internal/api/rest"
	"github.com/eleboucher/memini/internal/apiauth"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// --- test server + helpers ---------------------------------------------------

// newConfigServer builds a REST server over a real sqlitevec store for the
// config-handshake surface, with the activity log on and synchronous so a
// write's events are visible to the next request. adminKey, fileYAML
// (MEMINI_API_KEYS_FILE-equivalent) and clientDefaults (env-managed globals) are
// all optional.
func newConfigServer(t *testing.T, adminKey, fileYAML string, clientDefaults *store.ClientSettings) (http.Handler, store.APIKeyStore) {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "config.db"), dims)
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

	svc := service.New(st, embedtest.New(dims), service.WithEventLog(true), service.WithSyncEventLog())
	r := chi.NewRouter()
	rest.New(svc, rest.AuthConfig{
		APIKey: adminKey, APIKeyStore: ks, FileKeys: fk,
		NamespaceHeader: nsHdr, DefaultNamespace: "default", HomeHeader: homeHdr,
		ClientDefaults: clientDefaults,
	}).Mount(r)
	return r, ks
}

// storeNoAPIKeys hides the optional APIKeyStore/ProjectMapStore/etc capabilities
// of a Store (a type assertion to any of them fails), exercising the 501 degrade
// paths. Only the base Store methods are promoted from the embedded interface.
type storeNoAPIKeys struct{ store.Store }

func handshakeBody(project map[string]any) map[string]any {
	return map[string]any{"project": project}
}

type callerIdentityDTO struct {
	Authenticated    bool    `json:"authenticated"`
	Admin            bool    `json:"admin"`
	KeyName          *string `json:"key_name"`
	Home             *string `json:"home"`
	DefaultNamespace *string `json:"default_namespace"`
}

type handshakeRespDTO struct {
	Namespace       string `json:"namespace"`
	NamespaceSource string `json:"namespace_source"`
	Pin             *struct {
		Key       string  `json:"key"`
		Note      *string `json:"note"`
		CreatedBy *string `json:"created_by"`
		UpdatedAt string  `json:"updated_at"`
	} `json:"pin"`
	Identity        callerIdentityDTO `json:"identity"`
	Settings        map[string]any    `json:"settings"`
	SettingsSources map[string]string `json:"settings_sources"`
	ReadSet         []struct {
		Namespace string `json:"namespace"`
		Origin    string `json:"origin"`
	} `json:"read_set"`
	Server struct {
		Version          string `json:"version"`
		DefaultNamespace string `json:"default_namespace"`
	} `json:"server"`
}

type selfRespDTO struct {
	Identity        callerIdentityDTO `json:"identity"`
	Settings        map[string]any    `json:"settings"`
	SettingsSources map[string]string `json:"settings_sources"`
}

// --- handshake: identity by credential class ---------------------------------

func TestHandshakeIdentityByCredential(t *testing.T) {
	fileYAML := `
keys:
  - name: filebot
    secret: "tok-file"
`
	h, ks := newConfigServer(t, "admin-secret", fileYAML, nil)
	if err := ks.PutAPIKey(context.Background(), store.APIKey{
		Name: "tablebot", Hash: hashOf("tok-table"), HomeNS: "personal/bot", DefaultNS: "acme",
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	body := handshakeBody(map[string]any{"cwd_basename": "proj", "remote_url": "https://github.com/acme/phoenix.git"})

	// Credentialed classes against the admin-keyed server.
	cases := []struct {
		name        string
		token       string
		wantKeyName string // "" means key_name must be absent (admin, no principal)
	}{
		{"admin key", "admin-secret", ""},
		{"named table key", "tok-table", "tablebot"},
		{"named file key", "tok-file", "filebot"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, "/v1/handshake", "", c.token, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("handshake: want 200, got %d (%s)", rec.Code, rec.Body)
			}
			var out handshakeRespDTO
			mustJSON(t, rec, &out)
			if !out.Identity.Authenticated {
				t.Errorf("authenticated = false, want true for %s", c.name)
			}
			switch {
			case c.wantKeyName == "" && out.Identity.KeyName != nil:
				t.Errorf("key_name = %q, want absent", *out.Identity.KeyName)
			case c.wantKeyName != "" && (out.Identity.KeyName == nil || *out.Identity.KeyName != c.wantKeyName):
				t.Errorf("key_name = %v, want %q", out.Identity.KeyName, c.wantKeyName)
			}
		})
	}

	// Dev mode is a distinct server (no admin key, empty table): a no-bearer
	// handshake is allowed and reports authenticated=false.
	t.Run("dev mode (no bearer)", func(t *testing.T) {
		dev, _ := newConfigServer(t, "", "", nil)
		rec := do(t, dev, http.MethodPost, "/v1/handshake", "", "", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("dev handshake: want 200, got %d (%s)", rec.Code, rec.Body)
		}
		var out handshakeRespDTO
		mustJSON(t, rec, &out)
		if out.Identity.Authenticated {
			t.Errorf("dev mode authenticated = true, want false")
		}
		if out.Identity.KeyName != nil {
			t.Errorf("dev mode key_name = %q, want absent", *out.Identity.KeyName)
		}
	})
}

// newDegradedServer is a dev-mode server whose store hides every optional
// capability (no APIKeyStore/ProjectMapStore/ClientSettingsStore), for
// exercising the 501 degrade paths. Dev mode (no admin key, no key store)
// authenticates every request with a nil principal.
func newDegradedServer(t *testing.T) http.Handler {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "degraded.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := service.New(storeNoAPIKeys{st}, embedtest.New(dims))
	r := chi.NewRouter()
	rest.New(svc, rest.AuthConfig{NamespaceHeader: nsHdr, DefaultNamespace: "default", HomeHeader: homeHdr}).Mount(r)
	return r
}

// TestGetSelfEveryCredentialClass pins that GET /v1/self answers for every
// credential class (admin, dev, named table key, file key) — it is the
// lighter-weight identity/settings refresh and must never 501/403.
func TestGetSelfEveryCredentialClass(t *testing.T) {
	fileYAML := `
keys:
  - name: filebot
    secret: "tok-file"
`
	h, ks := newConfigServer(t, "admin-secret", fileYAML, nil)
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "tablebot", Hash: hashOf("tok-table")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	for _, tok := range []string{"admin-secret", "tok-table", "tok-file"} {
		rec := do(t, h, http.MethodGet, "/v1/self", "", tok, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/self with %q: want 200, got %d (%s)", tok, rec.Code, rec.Body)
		}
		var out selfRespDTO
		mustJSON(t, rec, &out)
		if !out.Identity.Authenticated {
			t.Errorf("GET /v1/self with %q: authenticated = false", tok)
		}
	}
	// Dev mode (separate server): unauthenticated, still 200.
	dev, _ := newConfigServer(t, "", "", nil)
	rec := do(t, dev, http.MethodGet, "/v1/self", "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dev GET /v1/self: want 200, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestPutSettingsDefaultsNamedKey403 pins the admin gate on the write side too:
// a named table key cannot change the server-wide global defaults.
func TestPutSettingsDefaultsNamedKey403(t *testing.T) {
	h, ks := newConfigServer(t, "admin-secret", "", nil)
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "bot", Hash: hashOf("tok-bot")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	rec := do(t, h, http.MethodPut, "/v1/settings/defaults", "", "tok-bot", map[string]any{"capture_turns": false})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("named-key PUT defaults: want 403, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestSettingsDefaults501NoStore pins the capability degrade: without a
// ClientSettingsStore and without env-managed defaults, GET and PUT
// /v1/settings/defaults both answer 501.
func TestSettingsDefaults501NoStore(t *testing.T) {
	h := newDegradedServer(t)
	rec := do(t, h, http.MethodGet, "/v1/settings/defaults", "", "", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("GET defaults degraded: want 501, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPut, "/v1/settings/defaults", "", "", map[string]any{"capture_turns": false})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("PUT defaults degraded: want 501, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestPins501NoProjectMapStore pins the pins capability degrade: every verb
// answers 501 against a backend with no project_map.
func TestPins501NoProjectMapStore(t *testing.T) {
	h := newDegradedServer(t)
	cases := []struct {
		method string
		body   any
	}{
		{http.MethodGet, nil},
		{http.MethodPut, map[string]any{"namespace": "x", "toplevel_path": "/srv/app"}},
		{http.MethodDelete, map[string]any{"toplevel_path": "/srv/app"}},
	}
	for _, c := range cases {
		rec := do(t, h, c.method, "/v1/pins", "", "", c.body)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s /v1/pins degraded: want 501, got %d (%s)", c.method, rec.Code, rec.Body)
		}
	}
}

// TestHandshakeDegradesWithoutPins pins that a handshake against a backend with
// no project_map resolves derived-only rather than erroring — the pin step is
// simply skipped.
func TestHandshakeDegradesWithoutPins(t *testing.T) {
	h := newDegradedServer(t)
	rec := do(t, h, http.MethodPost, "/v1/handshake", "", "", handshakeBody(map[string]any{
		"cwd_basename": "proj", "remote_url": "https://github.com/acme/phoenix.git",
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("degraded handshake: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var out handshakeRespDTO
	mustJSON(t, rec, &out)
	if out.Namespace != "phoenix" || out.NamespaceSource != "remote" {
		t.Fatalf("degraded handshake: got %q/%q, want phoenix/remote", out.Namespace, out.NamespaceSource)
	}
}

// TestHandshakeMissingCwdBasename400 pins the one required project field.
func TestHandshakeMissingCwdBasename400(t *testing.T) {
	h, _ := newConfigServer(t, "", "", nil)
	rec := do(t, h, http.MethodPost, "/v1/handshake", "", "", handshakeBody(map[string]any{
		"remote_url": "https://github.com/acme/phoenix.git",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("handshake with no cwd_basename: want 400, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestHandshakeUnknownFieldRejected pins the strict JSON decode (rest.go's
// decode helper calls DisallowUnknownFields): a stray field anywhere in the
// body is a 400, not silently ignored, whether it appears inside the nested
// project object or at the request's top level.
func TestHandshakeUnknownFieldRejected(t *testing.T) {
	h, _ := newConfigServer(t, "", "", nil)
	cases := []struct {
		name string
		body map[string]any
	}{
		{
			"unknown field inside project",
			handshakeBody(map[string]any{"cwd_basename": "proj", "bogus_field": "nope"}),
		},
		{
			"unknown top-level field",
			map[string]any{"project": map[string]any{"cwd_basename": "proj"}, "bogus": true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, "/v1/handshake", "", "", c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("handshake with %s: want 400, got %d (%s)", c.name, rec.Code, rec.Body)
			}
		})
	}
}

// --- handshake: precedence (pin > env > declared > derive) -------------------

func TestHandshakeNamespacePrecedence(t *testing.T) {
	h, _ := newConfigServer(t, "", "", nil)
	const remote = "https://github.com/acme/phoenix.git"

	handshake := func(project map[string]any) handshakeRespDTO {
		rec := do(t, h, http.MethodPost, "/v1/handshake", "", "", handshakeBody(project))
		if rec.Code != http.StatusOK {
			t.Fatalf("handshake: want 200, got %d (%s)", rec.Code, rec.Body)
		}
		var out handshakeRespDTO
		mustJSON(t, rec, &out)
		return out
	}

	// Derivation from the remote.
	got := handshake(map[string]any{"cwd_basename": "proj", "remote_url": remote})
	if got.Namespace != "phoenix" || got.NamespaceSource != "remote" {
		t.Fatalf("derive: got %q/%q, want phoenix/remote", got.Namespace, got.NamespaceSource)
	}

	// declared beats derivation (verbatim, no agent suffix), loses to env.
	got = handshake(map[string]any{"cwd_basename": "proj", "remote_url": remote, "declared_namespace": "gateway/hook", "agent": "reviewer"})
	if got.Namespace != "gateway/hook" || got.NamespaceSource != "declared" {
		t.Fatalf("declared: got %q/%q, want gateway/hook/declared", got.Namespace, got.NamespaceSource)
	}
	got = handshake(map[string]any{"cwd_basename": "proj", "remote_url": remote, "declared_namespace": "gateway/hook", "env_namespace": "from-env"})
	if got.Namespace != "from-env" || got.NamespaceSource != "env" {
		t.Fatalf("env beats declared: got %q/%q, want from-env/env", got.Namespace, got.NamespaceSource)
	}

	// A pin beats env (and everything else).
	rec := do(t, h, http.MethodPut, "/v1/pins", "", "", map[string]any{"namespace": "pinned/ns", "remote_url": remote})
	if rec.Code != http.StatusOK {
		t.Fatalf("put pin: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	got = handshake(map[string]any{"cwd_basename": "proj", "remote_url": remote, "env_namespace": "from-env"})
	if got.Namespace != "pinned/ns" || got.NamespaceSource != "pin" {
		t.Fatalf("pin beats env: got %q/%q, want pinned/ns/pin", got.Namespace, got.NamespaceSource)
	}
	if got.Pin == nil || got.Pin.Key != "remote:github.com/acme/phoenix" {
		t.Fatalf("pin block = %+v, want key remote:github.com/acme/phoenix", got.Pin)
	}
}

// TestHandshakePinBlockDetails pins that a pin hit populates the response's
// pin block beyond just the key: the note round-trips and updated_at is a real
// timestamp. TestHandshakeNamespacePrecedence only asserts Pin.Key, so this
// covers the note/timestamp wiring in Handshake's SourcePin branch.
func TestHandshakePinBlockDetails(t *testing.T) {
	h, _ := newConfigServer(t, "", "", nil)
	const remote = "https://github.com/acme/phoenix.git"

	rec := do(t, h, http.MethodPut, "/v1/pins", "", "", map[string]any{
		"namespace": "pinned/ns", "remote_url": remote, "note": "team decision",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put pin: want 200, got %d (%s)", rec.Code, rec.Body)
	}

	rec = do(t, h, http.MethodPost, "/v1/handshake", "", "", handshakeBody(map[string]any{
		"cwd_basename": "proj", "remote_url": remote,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("handshake: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var got handshakeRespDTO
	mustJSON(t, rec, &got)
	if got.Pin == nil {
		t.Fatalf("expected a pin block, got none")
	}
	if got.Pin.Note == nil || *got.Pin.Note != "team decision" {
		t.Errorf("pin note = %v, want %q", got.Pin.Note, "team decision")
	}
	if got.Pin.UpdatedAt == "" {
		t.Errorf("pin updated_at is empty, want a timestamp")
	}
}

// TestHandshakeEnvNamespacePrefix pins the client-side MEMINI_NAMESPACE_PREFIX
// override: it prepends to a derived name (personal/<repo>) exactly as the
// namespace_prefix setting would, is reported with source "env", and wins over
// a server-set global default — so one credential (here the no-principal env
// key) can serve several namespace trees selected per shell/directory.
func TestHandshakeEnvNamespacePrefix(t *testing.T) {
	h, _ := newConfigServer(t, "", "", nil)
	const remote = "https://github.com/acme/phoenix.git"

	// A global default prefix, to prove the client env wins over it.
	rec := do(t, h, http.MethodPut, "/v1/settings/defaults", "", "", map[string]any{"namespace_prefix": "personal"})
	if rec.Code != http.StatusOK {
		t.Fatalf("put defaults: want 200, got %d (%s)", rec.Code, rec.Body)
	}

	rec = do(t, h, http.MethodPost, "/v1/handshake", "", "", handshakeBody(map[string]any{
		"cwd_basename": "proj", "remote_url": remote, "env_namespace_prefix": "work",
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("handshake: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var got handshakeRespDTO
	mustJSON(t, rec, &got)
	if got.Namespace != "work/phoenix" || got.NamespaceSource != "remote" {
		t.Fatalf("env prefix override: got %q/%q, want work/phoenix/remote", got.Namespace, got.NamespaceSource)
	}
	if got.Settings["namespace_prefix"] != "work" {
		t.Errorf("settings.namespace_prefix = %v, want work", got.Settings["namespace_prefix"])
	}
	if got.SettingsSources["namespace_prefix"] != "env" {
		t.Errorf("settings_sources.namespace_prefix = %q, want env", got.SettingsSources["namespace_prefix"])
	}

	// Without the env override, the global default applies (personal), proving
	// the override is per-request, not sticky.
	rec = do(t, h, http.MethodPost, "/v1/handshake", "", "", handshakeBody(map[string]any{
		"cwd_basename": "proj", "remote_url": remote,
	}))
	mustJSON(t, rec, &got)
	if got.Namespace != "personal/phoenix" {
		t.Fatalf("without env prefix: got %q, want personal/phoenix (global default)", got.Namespace)
	}
}

// --- handshake: determinism + no writes --------------------------------------

func TestHandshakeDeterministicAndSideEffectFree(t *testing.T) {
	h, _ := newConfigServer(t, "", "", nil)
	body := handshakeBody(map[string]any{"cwd_basename": "proj", "remote_url": "https://github.com/acme/phoenix.git", "agent": "reviewer"})

	first := do(t, h, http.MethodPost, "/v1/handshake", "", "", body)
	if first.Code != http.StatusOK {
		t.Fatalf("handshake: want 200, got %d (%s)", first.Code, first.Body)
	}
	// A derived (non-pin) response carries no timestamps, so two identical calls
	// must produce byte-identical bodies.
	for range 3 {
		again := do(t, h, http.MethodPost, "/v1/handshake", "", "", body)
		if again.Body.String() != first.Body.String() {
			t.Fatalf("handshake is not deterministic:\n first: %s\n again: %s", first.Body, again.Body)
		}
	}

	// Side-effect-free: after several handshakes the activity log is still empty
	// GLOBALLY (a handshake resolves; it never writes). all_namespaces=true
	// ignores the namespace header entirely (see ListActivity), so this catches
	// a stray write into ANY namespace, not just the one the client happened to
	// resolve to.
	rec := do(t, h, http.MethodGet, "/v1/activity?all_namespaces=true", "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("activity: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var act activityResponse
	mustJSON(t, rec, &act)
	if len(act.Events) != 0 {
		t.Fatalf("handshake must write nothing anywhere, got %d activity events globally: %+v", len(act.Events), act.Events)
	}
}

// --- handshake: read_set parity with GET /v1/namespaces/readset --------------

func TestHandshakeReadSetParity(t *testing.T) {
	h, _ := newConfigServer(t, "", "", nil)
	// declared_namespace pins a stable three-level namespace with a real
	// ancestor cascade so the read-set has more than one entry to compare.
	body := handshakeBody(map[string]any{"cwd_basename": "proj", "declared_namespace": "acme/phoenix/api"})
	rec := do(t, h, http.MethodPost, "/v1/handshake", "", "", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("handshake: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var hs handshakeRespDTO
	mustJSON(t, rec, &hs)

	rec = do(t, h, http.MethodGet, "/v1/namespaces/readset", hs.Namespace, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("readset: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var rs struct {
		Entries []struct {
			Namespace string `json:"namespace"`
			Origin    string `json:"origin"`
		} `json:"entries"`
	}
	mustJSON(t, rec, &rs)

	if len(hs.ReadSet) != len(rs.Entries) || len(rs.Entries) == 0 {
		t.Fatalf("read_set length mismatch: handshake %d vs readset %d", len(hs.ReadSet), len(rs.Entries))
	}
	for i := range rs.Entries {
		if hs.ReadSet[i].Namespace != rs.Entries[i].Namespace || hs.ReadSet[i].Origin != rs.Entries[i].Origin {
			t.Errorf("read_set[%d] = %+v, want %+v", i, hs.ReadSet[i], rs.Entries[i])
		}
	}
}

// --- pins: open to all, round trip, 400/404, activity ------------------------

func TestPinsLifecycleAndAudit(t *testing.T) {
	h, ks := newConfigServer(t, "admin-secret", "", nil)
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "bot", Hash: hashOf("tok-bot")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	const remote = "git@github.com:acme/app.git"

	// A named table key (not admin) may pin — pins are open to all.
	rec := do(t, h, http.MethodPut, "/v1/pins", "", "tok-bot", map[string]any{
		"namespace": "team/app", "remote_url": remote, "note": "pinned by bot",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put pin (named key): want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var entry struct {
		Key       string  `json:"key"`
		Namespace string  `json:"namespace"`
		CreatedBy *string `json:"created_by"`
	}
	mustJSON(t, rec, &entry)
	if entry.Key != "remote:github.com/acme/app" || entry.Namespace != "team/app" {
		t.Fatalf("entry = %+v, want key remote:github.com/acme/app ns team/app", entry)
	}
	if entry.CreatedBy == nil || *entry.CreatedBy != "bot" {
		t.Errorf("created_by = %v, want bot", entry.CreatedBy)
	}

	// GET is open to all (dev/no-bearer here would 401 since an admin key is set;
	// use the named key).
	rec = do(t, h, http.MethodGet, "/v1/pins", "", "tok-bot", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list pins: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var list struct {
		Entries []struct {
			Key string `json:"key"`
		} `json:"entries"`
	}
	mustJSON(t, rec, &list)
	if len(list.Entries) != 1 || list.Entries[0].Key != "remote:github.com/acme/app" {
		t.Fatalf("list = %+v, want the one pin", list.Entries)
	}

	// No key fact → 400.
	rec = do(t, h, http.MethodPut, "/v1/pins", "", "admin-secret", map[string]any{"namespace": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("put pin with no key fact: want 400, got %d (%s)", rec.Code, rec.Body)
	}

	// The pin write and delete are activity-logged (kind pin / unpin).
	rec = do(t, h, http.MethodGet, "/v1/activity?kind=pin", "team/app", "admin-secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("activity pin: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var pinAct activityResponse
	mustJSON(t, rec, &pinAct)
	if len(pinAct.Events) != 1 || pinAct.Events[0].Namespace != "team/app" {
		t.Fatalf("pin event = %+v, want one against team/app", pinAct.Events)
	}
	// The actor now lives on the event row (not detail.by): the named key "bot"
	// created the pin, so it is attributed on the event with kind "key".
	if pinAct.Events[0].Actor != "bot" || pinAct.Events[0].ActorKind != "key" {
		t.Errorf("pin event actor = (%q, %q), want (bot, key)",
			pinAct.Events[0].Actor, pinAct.Events[0].ActorKind)
	}
	if _, ok := pinAct.Events[0].Detail["by"]; ok {
		t.Errorf("pin event detail still carries redundant \"by\": %+v", pinAct.Events[0].Detail)
	}

	// Delete by the same key fact → 204, then a repeat → 404.
	rec = do(t, h, http.MethodDelete, "/v1/pins", "", "tok-bot", map[string]any{"remote_url": remote})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete pin: want 204, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodDelete, "/v1/pins", "", "tok-bot", map[string]any{"remote_url": remote})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing pin: want 404, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodGet, "/v1/activity?kind=unpin", "team/app", "admin-secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("activity unpin: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var unpinAct activityResponse
	mustJSON(t, rec, &unpinAct)
	if len(unpinAct.Events) != 1 {
		t.Fatalf("unpin event = %+v, want exactly one", unpinAct.Events)
	}
}

// TestActivityAttribution is the point of T5: every activity event records who
// performed it. A named key is attributed by name (kind "key"), the admin env
// key by kind "env" (no name), and a dev-mode request by kind "none". The
// actor filter then selects exactly one key's operations.
func TestActivityAttribution(t *testing.T) {
	h, ks := newConfigServer(t, "admin-secret", "", nil)
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "bot", Hash: hashOf("tok-bot")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}

	// A named key's recall is attributed to it; the admin env key's is "env".
	if rec := do(t, h, http.MethodPost, "/v1/search", "team/app", "tok-bot",
		map[string]any{"query": "named search"}); rec.Code != http.StatusOK {
		t.Fatalf("named search: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	if rec := do(t, h, http.MethodPost, "/v1/search", "team/app", "admin-secret",
		map[string]any{"query": "env search"}); rec.Code != http.StatusOK {
		t.Fatalf("env search: want 200, got %d (%s)", rec.Code, rec.Body)
	}

	rec := do(t, h, http.MethodGet, "/v1/activity?kind=recall", "team/app", "admin-secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("activity: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var act activityResponse
	mustJSON(t, rec, &act)
	byQuery := map[string]activityEvent{}
	for _, e := range act.Events {
		byQuery[e.Query] = e
	}
	if got := byQuery["named search"]; got.Actor != "bot" || got.ActorKind != "key" {
		t.Errorf("named recall actor = (%q, %q), want (bot, key)", got.Actor, got.ActorKind)
	}
	if got := byQuery["env search"]; got.Actor != "" || got.ActorKind != "env" {
		t.Errorf("env recall actor = (%q, %q), want ('', env)", got.Actor, got.ActorKind)
	}
	// The recall's "why" defaults to "api" for a direct REST search.
	if got := byQuery["named search"].Detail["source"]; got != "api" {
		t.Errorf("recall source = %v, want api", got)
	}

	// The actor filter selects only the named key's operations.
	rec = do(t, h, http.MethodGet, "/v1/activity?kind=recall&actor=bot", "team/app", "admin-secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("actor filter: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var filtered activityResponse
	mustJSON(t, rec, &filtered)
	if len(filtered.Events) != 1 || filtered.Events[0].Query != "named search" {
		t.Fatalf("actor=bot returned %+v, want only the named search", filtered.Events)
	}
}

// TestActivityAttributionDevMode covers the "none" kind: an unauthenticated
// dev-mode request (no admin key, no table keys) carries no bearer, so its
// events are attributed to kind "none" with no name.
func TestActivityAttributionDevMode(t *testing.T) {
	h, _ := newConfigServer(t, "", "", nil) // no admin key, empty table → auth disabled

	if rec := do(t, h, http.MethodPost, "/v1/search", "dev", "",
		map[string]any{"query": "dev search"}); rec.Code != http.StatusOK {
		t.Fatalf("dev search: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	rec := do(t, h, http.MethodGet, "/v1/activity?kind=recall", "dev", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("activity: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var act activityResponse
	mustJSON(t, rec, &act)
	if len(act.Events) != 1 || act.Events[0].Actor != "" || act.Events[0].ActorKind != "none" {
		t.Fatalf("dev event = %+v, want actor='' kind='none'", act.Events)
	}
}

// TestListPinsEveryCredentialClass pins that GET /v1/pins answers for every
// credential class (admin, dev mode, named table key, named file key): it is
// ungated by design (see ListPins's doc comment) — the project map is
// machine-wide derivation state, not scoped to one namespace or principal.
func TestListPinsEveryCredentialClass(t *testing.T) {
	fileYAML := `
keys:
  - name: filebot
    secret: "tok-file"
`
	h, ks := newConfigServer(t, "admin-secret", fileYAML, nil)
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "tablebot", Hash: hashOf("tok-table")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	for _, tok := range []string{"admin-secret", "tok-table", "tok-file"} {
		rec := do(t, h, http.MethodGet, "/v1/pins", "", tok, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/pins with %q: want 200, got %d (%s)", tok, rec.Code, rec.Body)
		}
	}
	// Dev mode (separate server, no auth configured): unauthenticated, still 200.
	dev, _ := newConfigServer(t, "", "", nil)
	rec := do(t, dev, http.MethodGet, "/v1/pins", "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dev-mode GET /v1/pins: want 200, got %d (%s)", rec.Code, rec.Body)
	}
}

// --- self settings: authz matrix ---------------------------------------------

func TestPutSelfSettingsAuthzMatrix(t *testing.T) {
	fileYAML := `
keys:
  - name: filebot
    secret: "tok-file"
`
	h, ks := newConfigServer(t, "admin-secret", fileYAML, nil)
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "tablebot", Hash: hashOf("tok-table")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	body := map[string]any{"capture_turns": false}

	// Admin key (nil principal) → 403, pointing at the global-defaults endpoint.
	rec := do(t, h, http.MethodPut, "/v1/self/settings", "", "admin-secret", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin self-settings: want 403, got %d (%s)", rec.Code, rec.Body)
	}
	// File key → 409 (managed declaratively).
	rec = do(t, h, http.MethodPut, "/v1/self/settings", "", "tok-file", body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("file-key self-settings: want 409, got %d (%s)", rec.Code, rec.Body)
	}
	// Named table key → 200.
	rec = do(t, h, http.MethodPut, "/v1/self/settings", "", "tok-table", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("table-key self-settings: want 200, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestPutSelfSettings403DevMode pins the nil-principal 403 for dev mode too (no
// admin key, no bearer): there is no "self" to update.
func TestPutSelfSettings403DevMode(t *testing.T) {
	h, _ := newConfigServer(t, "", "", nil)
	rec := do(t, h, http.MethodPut, "/v1/self/settings", "", "", map[string]any{"capture_turns": false})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("dev-mode self-settings: want 403, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestPutSelfSettings501NoAPIKeyStore pins the 501 degrade: a backend whose
// store cannot persist per-key settings (no APIKeyStore capability) answers 501,
// even though auth still works via the separately-wired AuthConfig.APIKeyStore.
func TestPutSelfSettings501NoAPIKeyStore(t *testing.T) {
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "nokeystore.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var ks store.APIKeyStore = st
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "bot", Hash: hashOf("tok-bot")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	// The service sees a store WITHOUT the APIKeyStore capability, so
	// h.keyStore() (svc.Store()) fails; auth sees the real store, so the key
	// still authenticates.
	svc := service.New(storeNoAPIKeys{st}, embedtest.New(dims))
	r := chi.NewRouter()
	rest.New(svc, rest.AuthConfig{
		APIKeyStore: ks, NamespaceHeader: nsHdr, DefaultNamespace: "default", HomeHeader: homeHdr,
	}).Mount(r)

	rec := do(t, r, http.MethodPut, "/v1/self/settings", "", "tok-bot", map[string]any{"capture_turns": false})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("self-settings without APIKeyStore: want 501, got %d (%s)", rec.Code, rec.Body)
	}
}

// --- self settings: full replace + float32 round trip + provenance -----------

func TestSelfSettingsFullReplaceAndFloatRoundTrip(t *testing.T) {
	h, ks := newConfigServer(t, "admin-secret", "", nil)
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "bot", Hash: hashOf("tok-bot")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}

	// PUT a non-integral float and a bool; both must survive the round trip
	// (the float across the store's float64 <-> the wire's float32 boundary).
	rec := do(t, h, http.MethodPut, "/v1/self/settings", "", "tok-bot", map[string]any{
		"capture_turns": false, "inject_pretool_min_score": 0.3,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put self-settings: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var self selfRespDTO
	mustJSON(t, rec, &self)
	if v, _ := self.Settings["capture_turns"].(bool); v {
		t.Errorf("capture_turns = %v, want false", self.Settings["capture_turns"])
	}
	if v, _ := self.Settings["inject_pretool_min_score"].(float64); v != 0.3 {
		t.Errorf("inject_pretool_min_score = %v, want 0.3 (non-integral round trip)", self.Settings["inject_pretool_min_score"])
	}
	if self.SettingsSources["capture_turns"] != "key" {
		t.Errorf("capture_turns provenance = %q, want key", self.SettingsSources["capture_turns"])
	}

	// GET /v1/self reflects the stored override.
	rec = do(t, h, http.MethodGet, "/v1/self", "", "tok-bot", nil)
	mustJSON(t, rec, &self)
	if v, _ := self.Settings["capture_turns"].(bool); v {
		t.Errorf("GET /v1/self capture_turns = %v, want false", self.Settings["capture_turns"])
	}

	// Full-replace: a second PUT that omits capture_turns returns it to the
	// inherited default (true, source default), and applies the new field.
	rec = do(t, h, http.MethodPut, "/v1/self/settings", "", "tok-bot", map[string]any{"recall_limit": 2})
	if rec.Code != http.StatusOK {
		t.Fatalf("second put: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	mustJSON(t, rec, &self)
	if v, _ := self.Settings["capture_turns"].(bool); !v {
		t.Errorf("after full-replace, capture_turns = %v, want true (back to inherited)", self.Settings["capture_turns"])
	}
	if self.SettingsSources["capture_turns"] != "default" {
		t.Errorf("capture_turns provenance = %q, want default after replace", self.SettingsSources["capture_turns"])
	}
	if v, _ := self.Settings["recall_limit"].(float64); v != 2 {
		t.Errorf("recall_limit = %v, want 2", self.Settings["recall_limit"])
	}
}

// --- settings defaults: authz, merge provenance, managed_by ------------------

func TestSettingsDefaultsAuthzAndMerge(t *testing.T) {
	h, ks := newConfigServer(t, "admin-secret", "", nil)
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "bot", Hash: hashOf("tok-bot")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}

	// A named key is refused (admin-gated), like /v1/keys.
	rec := do(t, h, http.MethodGet, "/v1/settings/defaults", "", "tok-bot", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("named-key GET defaults: want 403, got %d (%s)", rec.Code, rec.Body)
	}

	// Admin GET: fully resolved, managed_by=api.
	rec = do(t, h, http.MethodGet, "/v1/settings/defaults", "", "admin-secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET defaults: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var def map[string]any
	mustJSON(t, rec, &def)
	if def["managed_by"] != "api" {
		t.Errorf("managed_by = %v, want api", def["managed_by"])
	}
	if _, ok := def["capture_turns"]; !ok {
		t.Error("GET defaults must be fully resolved (capture_turns present)")
	}

	// Admin PUT a global default, then a named key's handshake reports it with
	// provenance "global"; an untouched field stays "default".
	rec = do(t, h, http.MethodPut, "/v1/settings/defaults", "", "admin-secret", map[string]any{"capture_turns": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin PUT defaults: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPost, "/v1/handshake", "", "tok-bot", handshakeBody(map[string]any{"cwd_basename": "proj"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("handshake: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var hs handshakeRespDTO
	mustJSON(t, rec, &hs)
	if v, _ := hs.Settings["capture_turns"].(bool); v {
		t.Errorf("capture_turns = %v, want false (global default)", hs.Settings["capture_turns"])
	}
	if hs.SettingsSources["capture_turns"] != "global" {
		t.Errorf("capture_turns provenance = %q, want global", hs.SettingsSources["capture_turns"])
	}
	if hs.SettingsSources["session_digest"] != "default" {
		t.Errorf("session_digest provenance = %q, want default", hs.SettingsSources["session_digest"])
	}

	// A per-key override then wins over the global for that field (provenance key).
	rec = do(t, h, http.MethodPut, "/v1/self/settings", "", "tok-bot", map[string]any{"capture_turns": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("put self-settings: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPost, "/v1/handshake", "", "tok-bot", handshakeBody(map[string]any{"cwd_basename": "proj"}))
	mustJSON(t, rec, &hs)
	if v, _ := hs.Settings["capture_turns"].(bool); !v {
		t.Errorf("per-key capture_turns = %v, want true (key beats global)", hs.Settings["capture_turns"])
	}
	if hs.SettingsSources["capture_turns"] != "key" {
		t.Errorf("capture_turns provenance = %q, want key", hs.SettingsSources["capture_turns"])
	}
}

// TestSettingsDefaultsDevModeAllowed pins case (b) of the admin gate: no admin
// key + empty table = dev mode, which may read/write the global defaults.
func TestSettingsDefaultsDevModeAllowed(t *testing.T) {
	h, _ := newConfigServer(t, "", "", nil)
	rec := do(t, h, http.MethodGet, "/v1/settings/defaults", "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dev-mode GET defaults: want 200, got %d (%s)", rec.Code, rec.Body)
	}
}

// --- per-key admin: identity wire + named-admin reaches admin surfaces -------

// TestIdentityAdminByCredentialClass pins CallerIdentity.admin (the effective
// admin capability) across every credential class, on BOTH GET /v1/self and
// POST /v1/handshake: dev mode is unauthenticated-but-admin (bootstrap open),
// the admin env key and a named admin key are authenticated-and-admin, and a
// named non-admin key is authenticated-but-not-admin.
func TestIdentityAdminByCredentialClass(t *testing.T) {
	fileYAML := `
keys:
  - name: fadmin
    secret: "tok-fadmin"
    admin: true
  - name: fplain
    secret: "tok-fplain"
`
	h, ks := newConfigServer(t, "admin-secret", fileYAML, nil)
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "tadmin", Hash: hashOf("tok-tadmin"), Admin: true}); err != nil {
		t.Fatalf("seed table admin: %v", err)
	}
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "tplain", Hash: hashOf("tok-tplain")}); err != nil {
		t.Fatalf("seed table non-admin: %v", err)
	}
	body := handshakeBody(map[string]any{"cwd_basename": "proj", "remote_url": "https://github.com/acme/phoenix.git"})

	cases := []struct {
		class             string
		token             string
		wantAuthenticated bool
		wantAdmin         bool
	}{
		{"env key", "admin-secret", true, true},
		{"named table admin", "tok-tadmin", true, true},
		{"named file admin", "tok-fadmin", true, true},
		{"named table non-admin", "tok-tplain", true, false},
		{"named file non-admin", "tok-fplain", true, false},
	}
	for _, c := range cases {
		t.Run("self/"+c.class, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, "/v1/self", "", c.token, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /v1/self: want 200, got %d (%s)", rec.Code, rec.Body)
			}
			var out selfRespDTO
			mustJSON(t, rec, &out)
			if out.Identity.Authenticated != c.wantAuthenticated || out.Identity.Admin != c.wantAdmin {
				t.Errorf("self identity = {authenticated:%v admin:%v}, want {%v %v}",
					out.Identity.Authenticated, out.Identity.Admin, c.wantAuthenticated, c.wantAdmin)
			}
		})
		t.Run("handshake/"+c.class, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, "/v1/handshake", "", c.token, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("handshake: want 200, got %d (%s)", rec.Code, rec.Body)
			}
			var out handshakeRespDTO
			mustJSON(t, rec, &out)
			if out.Identity.Authenticated != c.wantAuthenticated || out.Identity.Admin != c.wantAdmin {
				t.Errorf("handshake identity = {authenticated:%v admin:%v}, want {%v %v}",
					out.Identity.Authenticated, out.Identity.Admin, c.wantAuthenticated, c.wantAdmin)
			}
		})
	}

	// Dev mode (separate bare server, no bearer): unauthenticated but admin=true.
	dev, _ := newConfigServer(t, "", "", nil)
	rec := do(t, dev, http.MethodGet, "/v1/self", "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dev GET /v1/self: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var self selfRespDTO
	mustJSON(t, rec, &self)
	if self.Identity.Authenticated || !self.Identity.Admin {
		t.Errorf("dev self identity = {authenticated:%v admin:%v}, want {false true}", self.Identity.Authenticated, self.Identity.Admin)
	}
	rec = do(t, dev, http.MethodPost, "/v1/handshake", "", "", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("dev handshake: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var hs handshakeRespDTO
	mustJSON(t, rec, &hs)
	if hs.Identity.Authenticated || !hs.Identity.Admin {
		t.Errorf("dev handshake identity = {authenticated:%v admin:%v}, want {false true}", hs.Identity.Authenticated, hs.Identity.Admin)
	}
}

// TestNamedAdminReachesSettingsDefaults pins that a named admin key now passes
// the GET/PUT /v1/settings/defaults gate that previously 403'd every named key
// — and that PutSelfSettings polarity is UNCHANGED: the same named admin key
// still edits its own per-key settings (200), because self-settings requires a
// named principal regardless of admin.
func TestNamedAdminReachesSettingsDefaults(t *testing.T) {
	h, ks := newConfigServer(t, "admin-secret", "", nil)
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "tadmin", Hash: hashOf("tok-tadmin"), Admin: true}); err != nil {
		t.Fatalf("seed table admin: %v", err)
	}
	rec := do(t, h, http.MethodGet, "/v1/settings/defaults", "", "tok-tadmin", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("named admin GET defaults: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPut, "/v1/settings/defaults", "", "tok-tadmin", map[string]any{"capture_turns": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("named admin PUT defaults: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	// PutSelfSettings polarity unchanged: a named admin still edits its own key.
	rec = do(t, h, http.MethodPut, "/v1/self/settings", "", "tok-tadmin", map[string]any{"recall_limit": 3})
	if rec.Code != http.StatusOK {
		t.Fatalf("named admin PUT self settings: want 200, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestSettingsDefaultsEnvManaged pins the MEMINI_CLIENT_DEFAULTS behavior: the
// env layer IS the globals — GET reports managed_by=env, PUT is refused 409, and
// a handshake resolves through it (provenance global).
func TestSettingsDefaultsEnvManaged(t *testing.T) {
	envDefaults := &store.ClientSettings{CaptureTurns: new(false), RecallLimit: new(9)}
	h, ks := newConfigServer(t, "admin-secret", "", envDefaults)
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "bot", Hash: hashOf("tok-bot")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}

	rec := do(t, h, http.MethodGet, "/v1/settings/defaults", "", "admin-secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET defaults: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var def map[string]any
	mustJSON(t, rec, &def)
	if def["managed_by"] != "env" {
		t.Errorf("managed_by = %v, want env", def["managed_by"])
	}
	if v, _ := def["recall_limit"].(float64); v != 9 {
		t.Errorf("recall_limit = %v, want 9 (from env defaults)", def["recall_limit"])
	}

	// PUT is refused with the documented 409 message.
	rec = do(t, h, http.MethodPut, "/v1/settings/defaults", "", "admin-secret", map[string]any{"capture_turns": true})
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT env-managed defaults: want 409, got %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "MEMINI_CLIENT_DEFAULTS") {
		t.Errorf("409 body should name MEMINI_CLIENT_DEFAULTS, got: %s", rec.Body)
	}

	// A handshake resolves through the env globals.
	rec = do(t, h, http.MethodPost, "/v1/handshake", "", "tok-bot", handshakeBody(map[string]any{"cwd_basename": "proj"}))
	var hs handshakeRespDTO
	mustJSON(t, rec, &hs)
	if v, _ := hs.Settings["recall_limit"].(float64); v != 9 {
		t.Errorf("handshake recall_limit = %v, want 9 (env global)", hs.Settings["recall_limit"])
	}
	if hs.SettingsSources["recall_limit"] != "global" {
		t.Errorf("recall_limit provenance = %q, want global", hs.SettingsSources["recall_limit"])
	}
}
