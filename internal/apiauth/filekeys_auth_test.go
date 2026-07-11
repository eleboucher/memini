package apiauth_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/apiauth"
	"github.com/eleboucher/memini/internal/store"
)

// TestAuthenticateExampleFileHashResolves loads the runnable example
// referenced by config.Config.APIKeysFile's doc (testdata/api_keys.example.yaml)
// and confirms its documented hash entry ("alex") actually authenticates with
// the plaintext secret the comment claims it hashes ("swordfish") — a
// regression guard on the doc staying accurate.
func TestAuthenticateExampleFileHashResolves(t *testing.T) {
	fk, err := apiauth.LoadFileKeys(filepath.Join("testdata", "api_keys.example.yaml"))
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	cfg := apiauth.New("", nil).WithFileKeys(fk)
	p, ok, err := cfg.Authenticate(context.Background(), "swordfish")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !ok || p == nil || p.Name != "alex" {
		t.Fatalf("example file hash entry: want authenticated as alex, got (%+v, %v, %v)", p, ok, err)
	}

	// The disabled example entry must reject even with its correct secret.
	_, ok2, err := cfg.Authenticate(context.Background(), "no-longer-valid")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ok2 {
		t.Fatalf("example file disabled entry (retired-bot): want rejected")
	}
}

// TestAuthenticateFileKeyPrincipal: a file key authenticates and its
// Principal carries name/home/default_ns exactly like a table key.
func TestAuthenticateFileKeyPrincipal(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: alex
    secret: "tok-alex"
    home: personal/alex
    default_namespace: acme
`)
	fk, err := apiauth.LoadFileKeys(path)
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	cfg := apiauth.New("", nil).WithFileKeys(fk)
	p, ok, err := cfg.Authenticate(context.Background(), "tok-alex")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !ok {
		t.Fatalf("file key: want authenticated")
	}
	if p == nil || p.Name != "alex" || p.HomeNS != "personal/alex" || p.DefaultNS != "acme" {
		t.Fatalf("file key principal = %+v, want {alex personal/alex acme}", p)
	}
}

func TestAuthenticateFileKeyHashVariantResolvesSameAsSecret(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: hash-alex
    hash: "`+hashOf("tok-hash")+`"
  - name: secret-alex
    secret: "tok-secret"
`)
	fk, err := apiauth.LoadFileKeys(path)
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	cfg := apiauth.New("", nil).WithFileKeys(fk)

	p1, ok1, err := cfg.Authenticate(context.Background(), "tok-hash")
	if err != nil || !ok1 || p1 == nil || p1.Name != "hash-alex" {
		t.Fatalf("hash variant: got (%+v, %v, %v), want authenticated as hash-alex", p1, ok1, err)
	}
	p2, ok2, err := cfg.Authenticate(context.Background(), "tok-secret")
	if err != nil || !ok2 || p2 == nil || p2.Name != "secret-alex" {
		t.Fatalf("secret variant: got (%+v, %v, %v), want authenticated as secret-alex", p2, ok2, err)
	}
}

func TestAuthenticateDisabledFileKeyRejected(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: retired
    secret: "tok-retired"
    disabled: true
`)
	fk, err := apiauth.LoadFileKeys(path)
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	cfg := apiauth.New("", nil).WithFileKeys(fk)
	_, ok, err := cfg.Authenticate(context.Background(), "tok-retired")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ok {
		t.Fatalf("disabled file key: want rejected")
	}
}

// TestAuthenticatePrecedenceAdminOverFile: the admin key authenticates even
// when a colliding bearer would also match a file key.
func TestAuthenticatePrecedenceAdminOverFile(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: alex
    secret: "shared-secret"
`)
	fk, err := apiauth.LoadFileKeys(path)
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	cfg := apiauth.New("shared-secret", nil).WithFileKeys(fk)
	p, ok, err := cfg.Authenticate(context.Background(), "shared-secret")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !ok {
		t.Fatalf("admin key: want authenticated")
	}
	if p != nil {
		t.Fatalf("admin key: want nil principal (not the file key's), got %+v", p)
	}
}

// TestAuthenticatePrecedenceFileOverTable: when the same hash is registered
// as both a file key and a (differently named) table key, the file key wins.
func TestAuthenticatePrecedenceFileOverTable(t *testing.T) {
	ks := openKeyStore(t)
	ctx := context.Background()
	if err := ks.PutAPIKey(ctx, store.APIKey{
		Name: "table-name", Hash: hashOf("shared-secret"), CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	path := writeKeysFile(t, `
keys:
  - name: file-name
    secret: "shared-secret"
`)
	fk, err := apiauth.LoadFileKeys(path)
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	cfg := apiauth.New("", ks).WithFileKeys(fk)
	p, ok, err := cfg.Authenticate(ctx, "shared-secret")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !ok || p == nil || p.Name != "file-name" {
		t.Fatalf("want the file key (file-name) to win over the table key, got (%+v, %v, %v)", p, ok, err)
	}
}

// TestAuthenticateFileKeysEnforceAuthRegardlessOfEmptyTable: any file key
// present forces auth (rejecting a no-bearer request) even with no admin key
// and an empty (or absent) table — mirroring the table's own
// non-empty-forces-auth rule, but keyed off the file instead.
func TestAuthenticateFileKeysEnforceAuthRegardlessOfEmptyTable(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: alex
    secret: "tok-alex"
`)
	fk, err := apiauth.LoadFileKeys(path)
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	// No admin key, no KeyStore at all.
	cfg := apiauth.New("", nil).WithFileKeys(fk)
	_, ok, err := cfg.Authenticate(context.Background(), "")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ok {
		t.Fatalf("file keys present, no bearer: want rejected")
	}

	// Also true with an empty table wired in alongside.
	ks := openKeyStore(t)
	cfg2 := apiauth.New("", ks).WithFileKeys(fk)
	_, ok2, err := cfg2.Authenticate(context.Background(), "")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ok2 {
		t.Fatalf("file keys present, empty table, no bearer: want rejected")
	}
}

// TestAuthenticateUnmatchedTokenFallsThroughFileToTable: a presented token
// that doesn't match any file key still gets a chance against the table.
func TestAuthenticateUnmatchedTokenFallsThroughFileToTable(t *testing.T) {
	ks := openKeyStore(t)
	ctx := context.Background()
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "table-bot", Hash: hashOf("tok-table"), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	path := writeKeysFile(t, `
keys:
  - name: file-bot
    secret: "tok-file"
`)
	fk, err := apiauth.LoadFileKeys(path)
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	cfg := apiauth.New("", ks).WithFileKeys(fk)
	p, ok, err := cfg.Authenticate(ctx, "tok-table")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !ok || p == nil || p.Name != "table-bot" {
		t.Fatalf("want the table key to authenticate when it doesn't match any file key, got (%+v, %v, %v)", p, ok, err)
	}
}

// TestAuthenticateNoFileKeysUnchanged: WithFileKeys(nil) — the feature-off
// case (MEMINI_API_KEYS_FILE unset) — must not change any existing behavior.
func TestAuthenticateNoFileKeysUnchanged(t *testing.T) {
	cfg := apiauth.New("", nil).WithFileKeys(nil)
	p, ok, err := cfg.Authenticate(context.Background(), "")
	if err != nil || !ok || p != nil {
		t.Fatalf("feature off: want dev mode (nil, true, nil), got (%+v, %v, %v)", p, ok, err)
	}
}

// TestAuthenticateEmptyFileKeysDoesNotForceAuth pins the documented
// empty-means-dev-mode reading: a MEMINI_API_KEYS_FILE that loads
// successfully but holds ZERO entries (keys: []) does NOT make auth
// mandatory — mirroring the table's own empty-table rule, where merely
// having the capability wired and unused leaves dev mode intact. Only a
// non-empty set forces auth (TestAuthenticateFileKeysEnforceAuthRegardlessOfEmptyTable).
// Locked in so the semantics can't silently flip to "file configured at all".
func TestAuthenticateEmptyFileKeysDoesNotForceAuth(t *testing.T) {
	fk, err := apiauth.LoadFileKeys(writeKeysFile(t, "keys: []\n"))
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	cfg := apiauth.New("", nil).WithFileKeys(fk)
	p, ok, err := cfg.Authenticate(context.Background(), "")
	if err != nil || !ok || p != nil {
		t.Fatalf("empty keys file, no bearer: want dev mode (nil, true, nil), got (%+v, %v, %v)", p, ok, err)
	}
}

// TestConfigFileKeysAccessors: Config exposes the loaded file keys' metadata
// (for a future K3b read-only listing) and IsFileKey by name.
func TestConfigFileKeysAccessors(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: alex
    secret: "tok-alex"
`)
	fk, err := apiauth.LoadFileKeys(path)
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	cfg := apiauth.New("", nil).WithFileKeys(fk)
	keys := cfg.FileKeys()
	if len(keys) != 1 || keys[0].Name != "alex" {
		t.Fatalf("cfg.FileKeys() = %+v, want [{alex ...}]", keys)
	}
	if !cfg.IsFileKey("alex") || cfg.IsFileKey("nobody") {
		t.Fatalf("cfg.IsFileKey: want alex=true, nobody=false")
	}

	cfgNoFile := apiauth.New("", nil)
	if cfgNoFile.FileKeys() != nil {
		t.Fatalf("no file keys configured: want nil, got %+v", cfgNoFile.FileKeys())
	}
	if cfgNoFile.IsFileKey("alex") {
		t.Fatalf("no file keys configured: IsFileKey must be false")
	}
}
