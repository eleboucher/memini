package rest_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/eleboucher/memini/internal/api/rest"
	"github.com/eleboucher/memini/internal/apiauth"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// writeKeysFile writes contents to a temp YAML file and returns its path —
// the "temp YAML" fixture the K2b brief calls for in an end-to-end REST test.
func writeKeysFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api_keys.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	return path
}

// newServerWithFileKeys builds a REST server whose AuthConfig carries a
// FileKeySet loaded from a temp YAML file — MEMINI_API_KEYS_FILE's end-to-end
// path, minus the boot-time file load itself (covered separately at the
// cmd/memini level).
func newServerWithFileKeys(t *testing.T, adminKey, yaml string) http.Handler {
	t.Helper()
	fk, err := apiauth.LoadFileKeys(writeKeysFile(t, yaml))
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "rest-filekey.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := service.New(st, embedtest.New(dims))
	r := chi.NewRouter()
	rest.New(svc, rest.AuthConfig{
		APIKey: adminKey, FileKeys: fk, NamespaceHeader: nsHdr, DefaultNamespace: "default", HomeHeader: homeHdr,
	}).Mount(r)
	return r
}

// TestFileKeyAuthenticatesOverREST: the K2b end-to-end case — a key declared
// in a MEMINI_API_KEYS_FILE-style YAML (loaded here from a temp file)
// authenticates a real REST request and stamps attribution, exactly like a
// table key.
func TestFileKeyAuthenticatesOverREST(t *testing.T) {
	h := newServerWithFileKeys(t, "", `
keys:
  - name: alex
    secret: "tok-alex-file"
    home: personal/alex
    default_namespace: acme
`)
	rec := do(t, h, http.MethodPost, "/v1/memories", "", "tok-alex-file", map[string]any{
		"content": "a file-key write", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("file key remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var m struct {
		Namespace string         `json:"namespace"`
		Metadata  map[string]any `json:"metadata"`
	}
	mustJSON(t, rec, &m)
	if m.Namespace != "acme" {
		t.Fatalf("namespace = %q, want the file key's default_namespace acme", m.Namespace)
	}
	if m.Metadata["author"] != "alex" {
		t.Fatalf("metadata.author = %v, want alex", m.Metadata["author"])
	}
}

func TestFileKeyHashVariantAuthenticatesOverREST(t *testing.T) {
	h := newServerWithFileKeys(t, "", `
keys:
  - name: hash-bot
    hash: "`+hashOf("tok-hash-bot")+`"
`)
	rec := do(t, h, http.MethodPost, "/v1/search", "acme", "tok-hash-bot", map[string]any{"query": "x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("file key (hash variant): want 200, got %d (%s)", rec.Code, rec.Body)
	}
}

func TestDisabledFileKeyRejectedOverREST(t *testing.T) {
	h := newServerWithFileKeys(t, "", `
keys:
  - name: retired
    secret: "tok-retired-file"
    disabled: true
`)
	rec := do(t, h, http.MethodPost, "/v1/search", "acme", "tok-retired-file", map[string]any{"query": "x"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled file key: want 401, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestFileKeysEnforceAuthOverREST: any file key present forces auth even with
// no admin key configured — an unauthenticated request must be rejected.
func TestFileKeysEnforceAuthOverREST(t *testing.T) {
	h := newServerWithFileKeys(t, "", `
keys:
  - name: alex
    secret: "tok-alex-file2"
`)
	rec := do(t, h, http.MethodPost, "/v1/search", "acme", "", map[string]any{"query": "x"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("file keys present, no bearer: want 401, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestAdminKeyPrecedenceOverFileKeyOverREST: the admin key still authenticates
// (with no attribution) even with a file key set configured alongside it.
func TestAdminKeyPrecedenceOverFileKeyOverREST(t *testing.T) {
	h := newServerWithFileKeys(t, "admin-secret", `
keys:
  - name: alex
    secret: "tok-alex-file3"
`)
	rec := do(t, h, http.MethodPost, "/v1/memories", "acme", "admin-secret", map[string]any{
		"content": "admin write alongside a file key", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin key: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var m struct {
		Metadata map[string]any `json:"metadata"`
	}
	mustJSON(t, rec, &m)
	if _, ok := m.Metadata["author"]; ok {
		t.Fatalf("admin key write must carry no author stamp, got metadata=%v", m.Metadata)
	}
}
