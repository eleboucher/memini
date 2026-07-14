package apiauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/apiauth"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

func hashOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func openKeyStore(t *testing.T) store.APIKeyStore {
	t.Helper()
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "apiauth.db"), 8)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var ks store.APIKeyStore = st
	return ks
}

// TestAuthenticateAdminKeyUnchanged: the admin env key still authenticates
// (constant-time match) with no table configured — today's only auth mode,
// must keep working byte-for-byte.
func TestAuthenticateAdminKeyUnchanged(t *testing.T) {
	cfg := apiauth.New("s3cret", nil)
	p, ok, err := cfg.Authenticate(context.Background(), "s3cret")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !ok {
		t.Fatalf("admin key: want authenticated")
	}
	if p != nil {
		t.Fatalf("admin key: want nil principal, got %+v", p)
	}
}

func TestAuthenticateAdminKeyWrongTokenRejected(t *testing.T) {
	cfg := apiauth.New("s3cret", nil)
	_, ok, err := cfg.Authenticate(context.Background(), "wrong")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ok {
		t.Fatalf("wrong token against admin key: want rejected")
	}
}

func TestAuthenticateNoAdminNoStoreAllowsEverything(t *testing.T) {
	cfg := apiauth.New("", nil)
	p, ok, err := cfg.Authenticate(context.Background(), "")
	if err != nil || !ok || p != nil {
		t.Fatalf("dev mode (no admin key, no store): want (nil, true, nil), got (%+v, %v, %v)", p, ok, err)
	}
	// Even a garbage bearer must not be rejected: today's behavior is "auth
	// entirely disabled", not "any non-matching token is invalid".
	p, ok, err = cfg.Authenticate(context.Background(), "garbage")
	if err != nil || !ok || p != nil {
		t.Fatalf("dev mode with garbage token: want (nil, true, nil), got (%+v, %v, %v)", p, ok, err)
	}
}

func TestAuthenticateTableKeyPrincipal(t *testing.T) {
	ks := openKeyStore(t)
	ctx := context.Background()
	if err := ks.PutAPIKey(ctx, store.APIKey{
		Name: "ci-bot", Hash: hashOf("tok-ci"), HomeNS: "acme/ci", DefaultNS: "acme/ci/logs", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	cfg := apiauth.New("", ks)
	p, ok, err := cfg.Authenticate(ctx, "tok-ci")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !ok {
		t.Fatalf("table key: want authenticated")
	}
	if p == nil || p.Name != "ci-bot" || p.HomeNS != "acme/ci" || p.DefaultNS != "acme/ci/logs" {
		t.Fatalf("table key: want principal{ci-bot,acme/ci,acme/ci/logs}, got %+v", p)
	}
}

// TestAuthenticateTableKeyAdminTruePropagates: a table key with Admin=true
// yields a Principal with Admin=true — the per-key capability replacing the
// old nil-principal-means-admin rule.
func TestAuthenticateTableKeyAdminTruePropagates(t *testing.T) {
	ks := openKeyStore(t)
	ctx := context.Background()
	if err := ks.PutAPIKey(ctx, store.APIKey{
		Name: "root-bot", Hash: hashOf("tok-root"), CreatedAt: time.Now().UTC(), Admin: true,
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	cfg := apiauth.New("", ks)
	p, ok, err := cfg.Authenticate(ctx, "tok-root")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !ok {
		t.Fatalf("table key: want authenticated")
	}
	if p == nil || !p.Admin {
		t.Fatalf("table key admin=true: want Principal.Admin=true, got %+v", p)
	}
}

// TestAuthenticateTableKeyAdminFalsePropagates: a table key with Admin=false
// (the default) yields a Principal with Admin=false.
func TestAuthenticateTableKeyAdminFalsePropagates(t *testing.T) {
	ks := openKeyStore(t)
	ctx := context.Background()
	if err := ks.PutAPIKey(ctx, store.APIKey{
		Name: "plain-bot", Hash: hashOf("tok-plain"), CreatedAt: time.Now().UTC(), Admin: false,
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	cfg := apiauth.New("", ks)
	p, ok, err := cfg.Authenticate(ctx, "tok-plain")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !ok {
		t.Fatalf("table key: want authenticated")
	}
	if p == nil || p.Admin {
		t.Fatalf("table key admin=false: want Principal.Admin=false, got %+v", p)
	}
}

// TestAuthenticateAdminKeyStaysNilPrincipal: the env admin key still resolves
// to a nil Principal (never a named admin principal) — adminness for the env
// key is expressed entirely by principal-nil-ness, unchanged by this
// per-key-Admin addition.
func TestAuthenticateAdminKeyStaysNilPrincipal(t *testing.T) {
	cfg := apiauth.New("s3cret", nil)
	p, ok, err := cfg.Authenticate(context.Background(), "s3cret")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !ok || p != nil {
		t.Fatalf("env admin key: want (nil principal, true), got (%+v, %v)", p, ok)
	}
}

// TestAuthenticateDevModeStaysNilPrincipal: dev mode (no admin key
// configured, no store, no bearer) still resolves to a nil Principal.
func TestAuthenticateDevModeStaysNilPrincipal(t *testing.T) {
	cfg := apiauth.New("", nil)
	p, ok, err := cfg.Authenticate(context.Background(), "")
	if err != nil || !ok || p != nil {
		t.Fatalf("dev mode: want (nil principal, true, nil), got (%+v, %v, %v)", p, ok, err)
	}
}

func TestAuthenticateDisabledKeyRejected(t *testing.T) {
	ks := openKeyStore(t)
	ctx := context.Background()
	if err := ks.PutAPIKey(ctx, store.APIKey{
		Name: "old-bot", Hash: hashOf("tok-old"), CreatedAt: time.Now().UTC(), Disabled: true,
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	cfg := apiauth.New("", ks)
	_, ok, err := cfg.Authenticate(ctx, "tok-old")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ok {
		t.Fatalf("disabled key: want rejected")
	}
}

func TestAuthenticateUnknownKeyRejected(t *testing.T) {
	ks := openKeyStore(t)
	ctx := context.Background()
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "real-bot", Hash: hashOf("tok-real"), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	cfg := apiauth.New("", ks)
	_, ok, err := cfg.Authenticate(ctx, "tok-does-not-exist")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ok {
		t.Fatalf("unknown key: want rejected")
	}
}

// TestAuthenticateEmptyTableStillAllowsDevMode: cfg.APIKey empty and an
// APIKeyStore is wired but holds zero rows — a request with no bearer must
// still be allowed (today's dev-mode behavior is unaffected by merely having
// the capability available and unused).
func TestAuthenticateEmptyTableStillAllowsDevMode(t *testing.T) {
	ks := openKeyStore(t)
	cfg := apiauth.New("", ks)
	p, ok, err := cfg.Authenticate(context.Background(), "")
	if err != nil || !ok || p != nil {
		t.Fatalf("empty table, no token: want (nil, true, nil), got (%+v, %v, %v)", p, ok, err)
	}
}

// TestAuthenticateNonEmptyTableEnablesTableOnlyAuth: the moment any table key
// exists, an unauthenticated request (no admin key, no bearer) must be
// rejected — table auth becomes mandatory, not merely additive.
func TestAuthenticateNonEmptyTableEnablesTableOnlyAuth(t *testing.T) {
	ks := openKeyStore(t)
	ctx := context.Background()
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "only-bot", Hash: hashOf("tok-only"), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	cfg := apiauth.New("", ks)
	_, ok, err := cfg.Authenticate(ctx, "")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ok {
		t.Fatalf("non-empty table, no token: want rejected")
	}
}

// countingKeyStore wraps a real APIKeyStore but counts ListAPIKeys calls, to
// verify the emptiness check is cached rather than hitting the store on
// every request.
type countingKeyStore struct {
	store.APIKeyStore
	listCalls int
}

func (c *countingKeyStore) ListAPIKeys(ctx context.Context) ([]store.APIKey, error) {
	c.listCalls++
	return c.APIKeyStore.ListAPIKeys(ctx)
}

func TestAuthenticateTableEmptinessCached(t *testing.T) {
	ks := &countingKeyStore{APIKeyStore: openKeyStore(t)}
	cfg := apiauth.New("", ks)
	ctx := context.Background()
	for range 5 {
		if _, ok, err := cfg.Authenticate(ctx, ""); err != nil || !ok {
			t.Fatalf("Authenticate: (%v, %v)", ok, err)
		}
	}
	if ks.listCalls != 1 {
		t.Fatalf("want exactly 1 ListAPIKeys call across 5 requests within the cache TTL, got %d", ks.listCalls)
	}
}

// failingKeyStore always errors on ListAPIKeys, to exercise the fail-closed
// behavior of the emptiness cache.
type failingKeyStore struct {
	store.APIKeyStore
}

func (failingKeyStore) ListAPIKeys(context.Context) ([]store.APIKey, error) {
	return nil, errors.New("boom")
}

func TestAuthenticateTableCheckFailsClosed(t *testing.T) {
	cfg := apiauth.New("", failingKeyStore{})
	_, ok, err := cfg.Authenticate(context.Background(), "")
	if err != nil {
		t.Fatalf("Authenticate: want no error (the failure is absorbed into fail-closed ok=false), got %v", err)
	}
	if ok {
		t.Fatalf("store error probing table emptiness: want fail-closed (rejected), got allowed")
	}
}

// TestAuthenticateStoreLookupErrorSurfaces: a GetAPIKeyByHash failure (a
// presented bearer, store reachable but erroring) must surface as err so the
// caller can 500 rather than silently 401ing a legitimate credential.
func TestAuthenticateStoreLookupErrorSurfaces(t *testing.T) {
	cfg := apiauth.New("", failingHashLookupStore{})
	_, _, err := cfg.Authenticate(context.Background(), "some-token")
	if err == nil {
		t.Fatalf("want the GetAPIKeyByHash error to surface")
	}
}

// TestInvalidateForcesImmediateRecheck pins the K3b cache-invalidation hook:
// without calling Invalidate, a key inserted after the first Authenticate
// call rides the TTL (the cached "empty" reading is stale, up to
// keyTableCacheTTL); Invalidate must force the very next Authenticate call to
// re-query the store rather than waiting out the cache, so a REST-driven
// create (the UI-first bootstrap flow) or delete (revocation) takes effect
// immediately in the SAME process.
func TestInvalidateForcesImmediateRecheck(t *testing.T) {
	ks := openKeyStore(t)
	cfg := apiauth.New("", ks)
	ctx := context.Background()

	// Prime the cache while the table is still empty: dev mode, allowed.
	if _, ok, err := cfg.Authenticate(ctx, ""); err != nil || !ok {
		t.Fatalf("priming Authenticate: (%v, %v)", ok, err)
	}

	// Insert a key directly (bypassing the cache) — the cached "empty"
	// reading is now stale but still within TTL.
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "bootstrap-bot", Hash: hashOf("tok-bootstrap"), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}

	// Without invalidation the stale cache would still say "empty" and allow
	// an unauthenticated request; Invalidate must flip that immediately.
	cfg.Invalidate()

	if _, ok, err := cfg.Authenticate(ctx, ""); err != nil {
		t.Fatalf("Authenticate after Invalidate: %v", err)
	} else if ok {
		t.Fatalf("after Invalidate + a key exists: no-bearer request must be rejected immediately, not after the TTL")
	}
}

type failingHashLookupStore struct {
	store.APIKeyStore
}

func (failingHashLookupStore) GetAPIKeyByHash(context.Context, string) (*store.APIKey, error) {
	return nil, errors.New("db down")
}
