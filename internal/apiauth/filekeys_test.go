package apiauth_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eleboucher/memini/internal/apiauth"
)

func writeKeysFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	return path
}

// TestLoadFileKeysHashAndSecretEquivalent: a key declared via a pre-hashed
// "hash" field and one declared via a plaintext "secret" field (hashed at
// load) must resolve identically — same lookup, same principal shape.
func TestLoadFileKeysHashAndSecretEquivalent(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: alex
    hash: "`+hashOf("tok-alex")+`"
    home: personal/alex
  - name: ci
    secret: "tok-ci"
    default_namespace: acme
`)
	fk, err := apiauth.LoadFileKeys(path)
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	keys := fk.FileKeys()
	if len(keys) != 2 {
		t.Fatalf("want 2 file keys, got %d: %+v", len(keys), keys)
	}
	byName := map[string]bool{}
	for _, k := range keys {
		byName[k.Name] = true
		if k.Hash == "" {
			t.Fatalf("key %q: want a non-empty hash, got empty (secret must be hashed at load)", k.Name)
		}
	}
	if !byName["alex"] || !byName["ci"] {
		t.Fatalf("want both alex and ci present, got %+v", keys)
	}
	if !fk.IsFileKey("alex") || !fk.IsFileKey("ci") {
		t.Fatalf("IsFileKey: want both alex and ci recognized")
	}
	if fk.IsFileKey("nobody") {
		t.Fatalf("IsFileKey(%q): want false", "nobody")
	}
}

func TestLoadFileKeysParsesHomeAndDefaultNamespace(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: alex
    hash: "`+hashOf("tok-alex")+`"
    home: personal/alex
    default_namespace: acme
    disabled: false
`)
	fk, err := apiauth.LoadFileKeys(path)
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	keys := fk.FileKeys()
	if len(keys) != 1 {
		t.Fatalf("want 1 key, got %d", len(keys))
	}
	k := keys[0]
	if k.HomeNS != "personal/alex" || k.DefaultNS != "acme" || k.Disabled {
		t.Fatalf("parsed key = %+v, want home=personal/alex default=acme disabled=false", k)
	}
}

// TestLoadFileKeysParsesAdmin covers the admin YAML field parsing into
// store.APIKey.Admin, both explicitly true and the false-by-default case for
// an entry that omits it entirely.
func TestLoadFileKeysParsesAdmin(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: root-alex
    hash: "`+hashOf("tok-root-alex")+`"
    admin: true
  - name: plain-bob
    hash: "`+hashOf("tok-plain-bob")+`"
`)
	fk, err := apiauth.LoadFileKeys(path)
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	keys := fk.FileKeys()
	byName := map[string]bool{}
	for _, k := range keys {
		byName[k.Name] = k.Admin
	}
	if admin, ok := byName["root-alex"]; !ok || !admin {
		t.Fatalf("root-alex: want admin=true, got %+v", keys)
	}
	if admin, ok := byName["plain-bob"]; !ok || admin {
		t.Fatalf("plain-bob (no admin field): want admin=false, got %+v", keys)
	}
}

// TestLoadFileKeysParsesReadOnly covers the read_only YAML field parsing into
// store.APIKey.ReadOnly, both explicitly true and the false-by-default case.
// The declarative file is the primary provisioning path for a CI credential
// (a mounted Secret), so silently dropping the field here would hand that
// credential write access.
func TestLoadFileKeysParsesReadOnly(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: ci-agent
    hash: "`+hashOf("tok-ci-agent")+`"
    read_only: true
  - name: plain-bob
    hash: "`+hashOf("tok-plain-bob-ro")+`"
`)
	fk, err := apiauth.LoadFileKeys(path)
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	keys := fk.FileKeys()
	byName := map[string]bool{}
	for _, k := range keys {
		byName[k.Name] = k.ReadOnly
	}
	if ro, ok := byName["ci-agent"]; !ok || !ro {
		t.Fatalf("ci-agent: want read_only=true, got %+v", keys)
	}
	if ro, ok := byName["plain-bob"]; !ok || ro {
		t.Fatalf("plain-bob (no read_only field): want read_only=false, got %+v", keys)
	}
}

func TestLoadFileKeysDisabledEntry(t *testing.T) {
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
	keys := fk.FileKeys()
	if len(keys) != 1 || !keys[0].Disabled {
		t.Fatalf("want 1 disabled key, got %+v", keys)
	}
}

// --- Fail-loud validation ---

func TestLoadFileKeysMalformedYAML(t *testing.T) {
	path := writeKeysFile(t, "keys: [not: valid: yaml:")
	_, err := apiauth.LoadFileKeys(path)
	if err == nil {
		t.Fatalf("want an error for malformed YAML")
	}
	assertErrNamesFile(t, err, path)
}

func TestLoadFileKeysMissingName(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - hash: "`+hashOf("tok")+`"
`)
	_, err := apiauth.LoadFileKeys(path)
	if err == nil {
		t.Fatalf("want an error for a missing name")
	}
	assertErrNamesFile(t, err, path)
}

func TestLoadFileKeysNeitherHashNorSecret(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: alex
`)
	_, err := apiauth.LoadFileKeys(path)
	if err == nil {
		t.Fatalf("want an error when neither hash nor secret is set")
	}
	assertErrNamesFile(t, err, path)
}

func TestLoadFileKeysBothHashAndSecret(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: alex
    hash: "`+hashOf("tok-alex")+`"
    secret: "tok-alex"
`)
	_, err := apiauth.LoadFileKeys(path)
	if err == nil {
		t.Fatalf("want an error when both hash and secret are set")
	}
	assertErrNamesFile(t, err, path)
}

func TestLoadFileKeysInvalidHashHex(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: alex
    hash: "not-hex-at-all"
`)
	_, err := apiauth.LoadFileKeys(path)
	if err == nil {
		t.Fatalf("want an error for invalid hash hex")
	}
	assertErrNamesFile(t, err, path)
}

func TestLoadFileKeysWrongLengthHash(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: alex
    hash: "deadbeef"
`)
	_, err := apiauth.LoadFileKeys(path)
	if err == nil {
		t.Fatalf("want an error for a hash that isn't a 32-byte SHA-256")
	}
	assertErrNamesFile(t, err, path)
}

func TestLoadFileKeysDuplicateNames(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: alex
    secret: "tok-alex-1"
  - name: alex
    secret: "tok-alex-2"
`)
	_, err := apiauth.LoadFileKeys(path)
	if err == nil {
		t.Fatalf("want an error for a duplicate name within the file")
	}
	assertErrNamesFile(t, err, path)
}

// TestLoadFileKeysDuplicateHashes: two entries sharing the same underlying
// secret (one declared via hash, one via plaintext secret — the sneakiest
// variant of the collision) must refuse the load. A silent last-wins
// collision in the hash map would make attribution non-deterministic and
// leave one key unreachable while still LOOKING live in
// FileKeys()/IsFileKey(). The error must name both entries by NAME and never
// echo the secret or the hash (the hash is retrievable from the file itself;
// repeating it in a log-bound error buys nothing).
func TestLoadFileKeysDuplicateHashes(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: first
    hash: "`+hashOf("shared-collision-secret")+`"
  - name: second
    secret: "shared-collision-secret"
`)
	_, err := apiauth.LoadFileKeys(path)
	if err == nil {
		t.Fatalf("want an error for two entries sharing a hash")
	}
	assertErrNamesFile(t, err, path)
	msg := err.Error()
	if !contains(msg, `"first"`) || !contains(msg, `"second"`) {
		t.Fatalf("error %q must name both colliding entries (first, second)", msg)
	}
	if contains(msg, "shared-collision-secret") || contains(msg, hashOf("shared-collision-secret")) {
		t.Fatalf("error %q must echo neither the secret nor the hash", msg)
	}
}

func TestLoadFileKeysInvalidHomeNamespace(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: alex
    secret: "tok-alex"
    home: "bad`+"\x00"+`ns"
`)
	_, err := apiauth.LoadFileKeys(path)
	if err == nil {
		t.Fatalf("want an error for an invalid home namespace")
	}
	assertErrNamesFile(t, err, path)
}

func TestLoadFileKeysInvalidDefaultNamespace(t *testing.T) {
	path := writeKeysFile(t, `
keys:
  - name: alex
    secret: "tok-alex"
    default_namespace: "bad`+"\x00"+`ns"
`)
	_, err := apiauth.LoadFileKeys(path)
	if err == nil {
		t.Fatalf("want an error for an invalid default_namespace")
	}
	assertErrNamesFile(t, err, path)
}

func TestLoadFileKeysMissingFile(t *testing.T) {
	_, err := apiauth.LoadFileKeys(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatalf("want an error for a missing file")
	}
}

// TestLoadFileKeysExampleFile loads the runnable example referenced by
// config.Config.APIKeysFile's doc comment (testdata/api_keys.example.yaml),
// pinning that it stays valid and its documented hash/secret pair actually
// resolves the way the comment claims.
func TestLoadFileKeysExampleFile(t *testing.T) {
	fk, err := apiauth.LoadFileKeys(filepath.Join("testdata", "api_keys.example.yaml"))
	if err != nil {
		t.Fatalf("LoadFileKeys(testdata/api_keys.example.yaml): %v", err)
	}
	keys := fk.FileKeys()
	if len(keys) != 3 {
		t.Fatalf("want 3 example keys, got %d: %+v", len(keys), keys)
	}
	if !fk.IsFileKey("alex") || !fk.IsFileKey("ci") || !fk.IsFileKey("retired-bot") {
		t.Fatalf("want alex, ci, and retired-bot all recognized, got %+v", keys)
	}
}

func TestLoadFileKeysEmptyFileYieldsNoKeys(t *testing.T) {
	path := writeKeysFile(t, "keys: []\n")
	fk, err := apiauth.LoadFileKeys(path)
	if err != nil {
		t.Fatalf("LoadFileKeys: %v", err)
	}
	if len(fk.FileKeys()) != 0 {
		t.Fatalf("want 0 keys, got %+v", fk.FileKeys())
	}
}

// assertErrNamesFile checks the error message names the offending file, so
// an operator can find it without hunting (K2b boot-validation requirement).
func assertErrNamesFile(t *testing.T, err error, path string) {
	t.Helper()
	if err == nil {
		t.Fatalf("assertErrNamesFile: nil error")
	}
	if got := err.Error(); !contains(got, path) {
		t.Fatalf("error %q does not name the file %q", got, path)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (needle == "" || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
