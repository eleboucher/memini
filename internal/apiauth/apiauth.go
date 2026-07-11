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
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"sync"
	"time"

	"github.com/eleboucher/memini/internal/store"
)

// Principal identifies a request as authenticated by a NAMED table key —
// never the admin env key, which authenticates with no principal at all (see
// Config.Authenticate). Name is the attribution identity consumed by
// RememberInput.Author; HomeNS/DefaultNS carry the key's bound home and
// per-key default namespace, consumed by the caller's home/namespace
// resolution.
type Principal struct {
	Name      string
	HomeNS    string
	DefaultNS string
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
// first, constant-time) and an optional table of named keys (SHA-256 hash
// lookup). Copy freely — the embedded cache pointer is shared across copies,
// which is what lets the emptiness cache persist across requests even though
// each middleware invocation works from its own copy of Config.
type Config struct {
	APIKey   string
	KeyStore store.APIKeyStore // nil disables table auth entirely
	cache    *tableCache
}

// New builds a Config ready to Authenticate. ks may be nil — no table
// capability, e.g. a store predating APIKeyStore, or the feature unused.
func New(apiKey string, ks store.APIKeyStore) Config {
	return Config{APIKey: apiKey, KeyStore: ks, cache: &tableCache{}}
}

// Authenticate resolves token (the raw bearer, "" when absent) against the
// admin key then the table. Semantics (binding, K2 brief):
//
//   - Admin key configured and token matches it (constant-time) → allowed,
//     principal nil.
//   - A bearer is presented AND KeyStore is set → looked up by hex SHA-256
//     hash. Found and enabled → allowed, principal identifies the key.
//     Found-but-disabled, or not found → REJECTED outright; this never falls
//     through to the "no usable token" allowance below, because a wrong
//     credential is not the same as no credential.
//   - No usable token (absent, or a KeyStore that can't be consulted because
//     it's nil): allowed, principal nil, IFF nothing requires auth — no admin
//     key AND (no KeyStore, or its table is empty). An admin key configured
//     with no/wrong token is rejected (unchanged pre-existing behavior). A
//     configured, non-empty table with no/wrong token is rejected too —
//     table auth becomes mandatory the instant any key exists, not merely
//     additive to dev-mode — checked via the cached, possibly-stale
//     tableNonEmpty read.
//
// A KeyStore error while looking up a presented token surfaces as err (the
// caller should respond 500, not 401 — an inability to check the table is
// not the same as an invalid credential). A KeyStore error while merely
// probing table emptiness does NOT surface as err; it fails closed (treated
// as non-empty, i.e. auth required) and is absorbed — see tableNonEmpty.
func (c Config) Authenticate(ctx context.Context, token string) (p *Principal, ok bool, err error) {
	if c.APIKey != "" && subtle.ConstantTimeCompare([]byte(token), []byte(c.APIKey)) == 1 {
		return nil, true, nil
	}
	if token != "" && c.KeyStore != nil {
		key, gerr := c.KeyStore.GetAPIKeyByHash(ctx, hashToken(token))
		if gerr != nil {
			return nil, false, gerr
		}
		if key != nil && !key.Disabled {
			return &Principal{Name: key.Name, HomeNS: key.HomeNS, DefaultNS: key.DefaultNS}, true, nil
		}
		return nil, false, nil
	}
	if c.APIKey != "" {
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

// hashToken hashes a presented bearer secret with hex SHA-256, matching
// store.APIKey.Hash's format (see its doc).
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
