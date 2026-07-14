// Package apiauth resolves the bearer-token principal for a request. It is
// shared by the REST (internal/api/rest) and MCP (internal/api/mcp) HTTP
// surfaces so the two transports authenticate identically for the same
// credentials — admin-key semantics, table-key lookup, disabled-key
// rejection, and the auth-enforcement edge cases around an empty admin key
// are implemented exactly once here rather than risking the two copies
// drifting (see the K2 brief's cross-surface consistency requirement).
package apiauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/eleboucher/memini/internal/store"
)

// Principal identifies a request as authenticated by a NAMED key (table or
// file) — never the admin env key, which authenticates with no principal at
// all (see Config.Authenticate). Name is the attribution identity consumed
// by RememberInput.Author; HomeNS/DefaultNS carry the key's bound home and
// per-key default namespace, consumed by the caller's home/namespace
// resolution. Admin carries the key's per-key admin capability
// (store.APIKey.Admin) — adminness is now a capability any named principal
// can hold, no longer solely the nil-principal env key's exclusive property.
type Principal struct {
	Name      string
	HomeNS    string
	DefaultNS string
	Admin     bool
}

// keyTableCacheTTL bounds how stale the "does the api_keys table hold any
// row" reading can be. Consulting the table on every unauthenticated request
// would cost a query per request in the default (no-admin-key, dev-mode)
// case; caching for this long means a key inserted moments ago can take up
// to this long to start being enforced when no admin key is configured — an
// accepted revocation-lag tradeoff.
const keyTableCacheTTL = 10 * time.Second

// tableCache memoizes the table-emptiness probe across requests. Config is
// copied by value at each call site (mirrors rest.AuthConfig's existing
// convention), so the cache must live behind a pointer shared by every copy.
type tableCache struct {
	mu       sync.Mutex
	at       time.Time
	nonEmpty bool
}

// Config is the shared bearer-token auth policy: the admin env key (checked
// first, constant-time), an optional immutable set of declaratively managed
// keys loaded once at boot from MEMINI_API_KEYS_FILE (checked second), and an
// optional table of named keys (SHA-256 hash lookup, checked last). Copy
// freely — the embedded cache pointer is shared across copies, which is what
// lets the emptiness cache persist across requests even though each
// middleware invocation works from its own copy of Config. fileKeys is safe
// to share across copies too: FileKeySet is immutable once loaded.
type Config struct {
	APIKey   string
	KeyStore store.APIKeyStore // nil disables table auth entirely
	fileKeys *FileKeySet       // nil disables file-key auth entirely (MEMINI_API_KEYS_FILE unset)
	cache    *tableCache
}

// New builds a Config ready to Authenticate. ks may be nil — no table
// capability, e.g. a store predating APIKeyStore, or the feature unused. Use
// WithFileKeys to additionally wire in a declarative keys file.
func New(apiKey string, ks store.APIKeyStore) Config {
	return Config{APIKey: apiKey, KeyStore: ks, cache: &tableCache{}}
}

// WithFileKeys returns a copy of c with fk attached as the declaratively
// managed key set (see FileKeySet's doc), consulted after the admin key and
// before the table — see Authenticate. fk may be nil, e.g. MEMINI_API_KEYS_FILE
// is unset: this leaves file-key auth disabled with zero behavior change,
// same as never calling WithFileKeys at all.
func (c Config) WithFileKeys(fk *FileKeySet) Config {
	c.fileKeys = fk
	return c
}

// FileKeys returns metadata (including hash, never a plaintext secret) for
// every key loaded from MEMINI_API_KEYS_FILE, ordered by name — nil when no
// file is configured. This is the seam a future read-only /v1/keys listing
// (K3b) uses to fold file keys into its output alongside table keys.
func (c Config) FileKeys() []store.APIKey {
	return c.fileKeys.FileKeys()
}

// IsFileKey reports whether name identifies a key sourced from
// MEMINI_API_KEYS_FILE. K3b's future key-mutation endpoints must consult this
// and refuse to rename/rotate/delete a file-sourced key by name — the file
// owns that identity, not the API.
func (c Config) IsFileKey(name string) bool {
	return c.fileKeys.IsFileKey(name)
}

// Authenticate resolves token (the raw bearer, "" when absent) against the
// admin key, then the file keys (MEMINI_API_KEYS_FILE, K2b), then the table.
// Semantics (binding, K2/K2b briefs):
//
//   - Admin key configured and token matches it (constant-time) → allowed,
//     principal nil.
//   - A bearer is presented AND fileKeys is set → looked up by hex SHA-256
//     hash. Found (enabled or not) → resolved here, final: enabled means
//     allowed with a principal identifying the key; disabled means REJECTED
//     outright, same as a disabled table key below. Found-but-disabled never
//     falls through to the table — a file entry that names this hash is
//     authoritative. Not found in the file falls through to the table below
//     (a token merely not being a file key doesn't make it invalid — it
//     might still be a DB key).
//   - A bearer is presented AND KeyStore is set → looked up by hex SHA-256
//     hash. Found and enabled → allowed, principal identifies the key.
//     Found-but-disabled, or not found → REJECTED outright; this never falls
//     through to the "no usable token" allowance below, because a wrong
//     credential is not the same as no credential.
//   - No usable token (absent, or fileKeys/KeyStore that can't be consulted
//     because they're nil): allowed, principal nil, IFF nothing requires
//     auth — no admin key AND fileKeys is empty/nil AND (no KeyStore, or its
//     table is empty). An admin key configured with no/wrong token is
//     rejected (unchanged pre-existing behavior). A non-empty file key set,
//     OR a configured non-empty table, with no/wrong token is rejected too —
//     either one makes auth mandatory the instant it holds any key, not
//     merely additive to dev-mode. The file set's emptiness is a plain field
//     read (it's immutable once loaded, unlike the table); the table's is the
//     cached, possibly-stale tableNonEmpty read.
//
// A KeyStore error while looking up a presented token surfaces as err (the
// caller should respond 500, not 401 — an inability to check the table is
// not the same as an invalid credential). A KeyStore error while merely
// probing table emptiness does NOT surface as err; it fails closed (treated
// as non-empty, i.e. auth required) and is absorbed — see tableNonEmpty.
// FileKeySet lookups never error (it's an in-memory map), so there is no
// equivalent failure mode on the file side.
func (c Config) Authenticate(ctx context.Context, token string) (p *Principal, ok bool, err error) {
	if c.APIKey != "" && subtle.ConstantTimeCompare([]byte(token), []byte(c.APIKey)) == 1 {
		return nil, true, nil
	}
	if token != "" && c.fileKeys != nil {
		if key, found := c.fileKeys.lookup(HashToken(token)); found {
			if key.Disabled {
				return nil, false, nil
			}
			return &Principal{Name: key.Name, HomeNS: key.HomeNS, DefaultNS: key.DefaultNS, Admin: key.Admin}, true, nil
		}
	}
	if token != "" && c.KeyStore != nil {
		key, gerr := c.KeyStore.GetAPIKeyByHash(ctx, HashToken(token))
		if gerr != nil {
			return nil, false, gerr
		}
		if key != nil && !key.Disabled {
			return &Principal{Name: key.Name, HomeNS: key.HomeNS, DefaultNS: key.DefaultNS, Admin: key.Admin}, true, nil
		}
		return nil, false, nil
	}
	if c.APIKey != "" {
		return nil, false, nil
	}
	if c.fileKeys.enforced() {
		return nil, false, nil
	}
	if c.KeyStore != nil && c.tableNonEmpty(ctx) {
		return nil, false, nil
	}
	return nil, true, nil
}

// tableNonEmpty reports whether the api_keys table holds at least one row,
// cached for keyTableCacheTTL (see its doc). A store error while refreshing
// fails CLOSED — it reports non-empty (auth required) rather than silently
// reopening dev-mode access on a transient backend hiccup — and does not
// update the cached timestamp, so the next call retries rather than pinning
// the failure for the full TTL.
func (c Config) tableNonEmpty(ctx context.Context) bool {
	c.cache.mu.Lock()
	defer c.cache.mu.Unlock()
	if time.Since(c.cache.at) < keyTableCacheTTL {
		return c.cache.nonEmpty
	}
	keys, err := c.KeyStore.ListAPIKeys(ctx)
	if err != nil {
		c.cache.nonEmpty = true
		return true
	}
	c.cache.at = time.Now()
	c.cache.nonEmpty = len(keys) > 0
	return c.cache.nonEmpty
}

// HashToken hashes a presented bearer secret with hex SHA-256, matching
// store.APIKey.Hash's format (see its doc). Exported as the one canonical
// hashing helper: the CLI (cmd/memini/key.go, generating and rotating table
// keys) and this package's own auth-path lookups must never risk drifting
// onto two different hash implementations.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// GenerateSecret returns a fresh 32-byte hex-encoded (64 char) random secret,
// the plaintext credential handed to a caller exactly once — over the CLI
// (cmd/memini/key.go) or the REST key-management API (K3b) — and never
// stored; only HashToken's digest of it is persisted. Exported as the one
// canonical secret generator, mirroring HashToken: both surfaces must mint
// secrets identically rather than risk two implementations drifting apart.
func GenerateSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate api key secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Invalidate clears the cached table-emptiness reading so the very next
// Authenticate call re-queries the store instead of riding out
// keyTableCacheTTL. Callers that mutate the api_keys table in the SAME
// process as a running server (K3b's REST create/update/delete handlers)
// must call this immediately after a successful write: without it, a
// just-created first key would not enforce auth for up to keyTableCacheTTL
// (breaking the UI-first bootstrap flow's "create key → auth enforced NOW"
// guarantee) and a just-deleted last key would keep requiring auth for the
// same window. The CLI, which writes to the store directly and is very
// possibly a different OS process than any running server, cannot invalidate
// a live server's cache this way — that revocation/bootstrap lag against a
// separately-running server is an accepted, documented TTL tradeoff, not a
// bug this method can fix. Safe to call even when cache is shared across
// copies of Config (see the struct doc); resets to the zero Time, which
// Since() always reports as >= TTL, forcing a re-check on the next read.
func (c Config) Invalidate() {
	c.cache.mu.Lock()
	defer c.cache.mu.Unlock()
	c.cache.at = time.Time{}
}
