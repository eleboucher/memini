package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/apiauth"
	"github.com/eleboucher/memini/internal/store"
)

// noKeyStore embeds a nil store.Store so it satisfies the interface (any
// method call other than the type assertion under test would panic) while
// deliberately not implementing store.APIKeyStore — a backend that predates
// the api_keys table (mirrors link_test.go's noLinkStore).
type noKeyStore struct {
	store.Store
}

func TestKeyStoreOfDegradesGracefully(t *testing.T) {
	if _, err := keyStoreOf(noKeyStore{}); err == nil {
		t.Fatal("expected an error for a backend without api key support")
	} else if !strings.Contains(err.Error(), "api key") {
		t.Errorf("error should mention api keys, got: %v", err)
	}
}

func TestKeyStoreOfSupportedBackend(t *testing.T) {
	st := openTestStore(t)
	if _, err := keyStoreOf(st); err != nil {
		t.Fatalf("sqlitevec should support APIKeyStore: %v", err)
	}
}

func TestNormalizeOptionalNamespaceEmptyIsUnset(t *testing.T) {
	got, err := normalizeOptionalNamespace("")
	if err != nil || got != "" {
		t.Fatalf("empty input should yield empty with no error; got %q, %v", got, err)
	}
	got, err = normalizeOptionalNamespace("   ")
	if err != nil || got != "" {
		t.Fatalf("blank input should yield empty with no error; got %q, %v", got, err)
	}
}

func TestNormalizeOptionalNamespaceNormalizes(t *testing.T) {
	got, err := normalizeOptionalNamespace("//acme/phoenix/")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got != "acme/phoenix" {
		t.Errorf("got %q, want %q", got, "acme/phoenix")
	}
}

func TestNormalizeOptionalNamespaceRejectsInvalid(t *testing.T) {
	if _, err := normalizeOptionalNamespace(strings.Repeat("x", 300)); err == nil {
		t.Fatal("expected error for an over-long namespace")
	}
}

func TestGenerateAPIKeySecret(t *testing.T) {
	a, err := generateAPIKeySecret()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(a) != 64 {
		t.Fatalf("want 64 hex chars (32 random bytes), got %d: %q", len(a), a)
	}
	b, err := generateAPIKeySecret()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if a == b {
		t.Fatal("two generated secrets should not collide")
	}
}

// TestAddAPIKeyGeneratesSecretThatAuthenticates pins the round trip the
// entire feature depends on: the plaintext secret handed back to the caller
// hashes (via the canonical apiauth.HashToken helper) to exactly the row
// GetAPIKeyByHash finds — the same lookup the auth middleware performs.
func TestAddAPIKeyGeneratesSecretThatAuthenticates(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	ctx := context.Background()

	secret, key, err := addAPIKey(ctx, ks, "alice", "acme/phoenix", "acme/default", false)
	if err != nil {
		t.Fatalf("addAPIKey: %v", err)
	}
	if secret == "" {
		t.Fatal("secret must not be empty")
	}
	if key.Name != "alice" || key.HomeNS != "acme/phoenix" || key.DefaultNS != "acme/default" {
		t.Fatalf("unexpected key: %+v", key)
	}
	if key.CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be populated for a freshly created key")
	}
	if key.Disabled {
		t.Fatal("key should not be disabled by default")
	}

	found, err := ks.GetAPIKeyByHash(ctx, apiauth.HashToken(secret))
	if err != nil {
		t.Fatalf("GetAPIKeyByHash: %v", err)
	}
	if found == nil || found.Name != "alice" {
		t.Fatalf("secret should authenticate to the stored key, got %+v", found)
	}
}

func TestAddAPIKeyDisabledFlag(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	_, key, err := addAPIKey(context.Background(), ks, "frank", "", "", true)
	if err != nil {
		t.Fatalf("addAPIKey: %v", err)
	}
	if !key.Disabled {
		t.Fatal("expected key to be created disabled")
	}
}

// TestAddAPIKeyRotationPreservesCreatedAtAndInvalidatesOldSecret pins the
// re-add-to-rotate contract: same name -> new secret, old secret stops
// authenticating, CreatedAt (first-created identity) survives untouched.
func TestAddAPIKeyRotationPreservesCreatedAtAndInvalidatesOldSecret(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	ctx := context.Background()

	oldSecret, first, err := addAPIKey(ctx, ks, "bob", "", "", false)
	if err != nil {
		t.Fatalf("addAPIKey: %v", err)
	}
	time.Sleep(2 * time.Millisecond) // guard against a same-instant CreatedAt false-passing the assertion below

	newSecret, second, err := addAPIKey(ctx, ks, "bob", "", "", false)
	if err != nil {
		t.Fatalf("re-add (rotate): %v", err)
	}

	if oldSecret == newSecret {
		t.Fatal("rotation should generate a new secret")
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("rotation must preserve CreatedAt: first=%v second=%v", first.CreatedAt, second.CreatedAt)
	}

	if found, gerr := ks.GetAPIKeyByHash(ctx, apiauth.HashToken(oldSecret)); gerr != nil {
		t.Fatalf("GetAPIKeyByHash(old): %v", gerr)
	} else if found != nil {
		t.Fatalf("old secret should no longer authenticate after rotation, got %+v", found)
	}
	if found, gerr := ks.GetAPIKeyByHash(ctx, apiauth.HashToken(newSecret)); gerr != nil {
		t.Fatalf("GetAPIKeyByHash(new): %v", gerr)
	} else if found == nil || found.Name != "bob" {
		t.Fatalf("new secret should authenticate, got %+v", found)
	}

	keys, err := ks.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("rotation must upsert, not duplicate: got %d keys: %+v", len(keys), keys)
	}
}

// TestAddAPIKeyRejectsEmptyName pins that an empty (or all-whitespace) name
// is refused rather than silently stored: the name is the key's primary
// label/identifier, and an empty one would be an unusable, easily-collided
// row (found via manual smoke testing of the actual CLI binary).
func TestAddAPIKeyRejectsEmptyName(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	if _, _, err := addAPIKey(context.Background(), ks, "", "", "", false); err == nil {
		t.Fatal("expected an empty name to be rejected")
	}
	if _, _, err := addAPIKey(context.Background(), ks, "   ", "", "", false); err == nil {
		t.Fatal("expected an all-whitespace name to be rejected")
	}
}

func TestAddAPIKeyRejectsInvalidHome(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	if _, _, err := addAPIKey(context.Background(), ks, "carol", strings.Repeat("x", 300), "", false); err == nil {
		t.Fatal("expected invalid --home namespace to be rejected")
	}
}

func TestAddAPIKeyRejectsInvalidDefaultNamespace(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	if _, _, err := addAPIKey(context.Background(), ks, "dave", "", strings.Repeat("x", 300), false); err == nil {
		t.Fatal("expected invalid --default-namespace to be rejected")
	}
}

// putErrKeyStore wraps a real store.APIKeyStore but forces PutAPIKey to fail,
// simulating the (astronomically unlikely) duplicate-hash unique-constraint
// error, without string-sniffing any particular driver's error text.
type putErrKeyStore struct {
	store.APIKeyStore
	err error
}

func (p putErrKeyStore) PutAPIKey(context.Context, store.APIKey) error {
	return p.err
}

func TestAddAPIKeyWrapsPutAPIKeyError(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	wrapped := putErrKeyStore{APIKeyStore: ks, err: errors.New("unique constraint failed: api_keys.key_hash")}
	if _, _, err := addAPIKey(context.Background(), wrapped, "grace", "", "", false); err == nil {
		t.Fatal("expected the store error to propagate")
	} else if !strings.Contains(err.Error(), "grace") {
		t.Errorf("wrapped error should name the key, got: %v", err)
	} else if !errors.Is(err, wrapped.err) {
		t.Errorf("wrapped error should wrap the underlying store error, got: %v", err)
	}
}

func TestRemoveAPIKeyNotFound(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	if err := removeAPIKey(context.Background(), ks, "ghost"); err == nil {
		t.Fatal("expected an error for a name that does not exist")
	}
}

func TestRemoveAPIKeyDeletesExisting(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	ctx := context.Background()
	if _, _, err := addAPIKey(ctx, ks, "erin", "", "", false); err != nil {
		t.Fatalf("addAPIKey: %v", err)
	}
	if err := removeAPIKey(ctx, ks, "erin"); err != nil {
		t.Fatalf("removeAPIKey: %v", err)
	}
	keys, err := ks.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("key should be gone, got %+v", keys)
	}
}

func TestPrintAPIKeysEmpty(t *testing.T) {
	var buf bytes.Buffer
	printAPIKeys(&buf, nil)
	if got := buf.String(); !strings.Contains(got, "no api keys") {
		t.Errorf("expected an empty-state message, got %q", got)
	}
}

// TestPrintAPIKeysTableNeverShowsHash pins the ls contract: the table shows
// name/home/default-ns/created/disabled but never the stored hash (and the
// APIKey struct never carries a plaintext secret at all, so there is nothing
// to leak on that front).
func TestPrintAPIKeysTableNeverShowsHash(t *testing.T) {
	var buf bytes.Buffer
	hashLike := strings.Repeat("deadbeef", 8) // 64 hex chars, same shape as a real hash
	printAPIKeys(&buf, []store.APIKey{
		{
			Name: "alice", Hash: hashLike, HomeNS: "acme/phoenix", DefaultNS: "acme/default",
			CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Disabled: false,
		},
		{Name: "bob", Disabled: true},
	})
	out := buf.String()
	for _, want := range []string{
		"NAME", "HOME", "DEFAULT NS", "CREATED", "DISABLED",
		"alice", "acme/phoenix", "acme/default", "bob", "true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, hashLike) {
		t.Errorf("table must never print the key hash, got:\n%s", out)
	}
}
