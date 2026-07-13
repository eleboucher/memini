package rest

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/apiauth"
	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/store"
)

type ctxKey int

const (
	namespaceKey ctxKey = iota
	homeKey
	principalKey
)

// AuthConfig configures the optional bearer-token auth and namespace
// resolution applied by Mount to the /v1 route group. Despite the name it also
// carries RequestTimeout: Mount's auth/namespace/timeout middleware are all
// installed together on the same route group, and adding a second options
// struct just for one field would be more ceremony than reuse — see
// internal/api/rest/rest.go's Mount for where each field is consumed.
type AuthConfig struct {
	// APIKey, when non-empty, is required as "Authorization: Bearer <key>".
	APIKey string
	// APIKeyStore, when non-nil, enables table-key auth alongside (or instead
	// of) APIKey: a bearer that doesn't match APIKey is looked up by hex
	// SHA-256 hash. nil means the backing store predates APIKeyStore, or the
	// feature is simply unused — see apiauth.Config.Authenticate for the full
	// enforcement rules, including when table auth becomes mandatory.
	APIKeyStore store.APIKeyStore
	// FileKeys, when non-nil, enables the declarative MEMINI_API_KEYS_FILE
	// keys (K2b) alongside APIKey/APIKeyStore: checked after APIKey and
	// before APIKeyStore — see apiauth.Config.Authenticate. nil (the default)
	// means the feature is unused, matching a server built before it existed.
	FileKeys *apiauth.FileKeySet
	// NamespaceHeader names the request header carrying the tenant namespace.
	NamespaceHeader string
	// DefaultNamespace is used when the header is absent and the
	// authenticated key (if any) carries no per-key default either.
	DefaultNamespace string
	// HomeHeader names the request header carrying the caller's personal
	// namespace (see service.RecallInput.Home). Unlike NamespaceHeader there
	// is no default: an absent or empty header means no home leg for the
	// request. Ignored outright for a key bound to a home namespace — see
	// homeMiddleware's doc for the deliberate asymmetry with namespace
	// resolution below.
	HomeHeader string
	// RequestTimeout bounds how long a single /v1 request may run
	// (chi/middleware.Timeout, applied only to the /v1 group Mount attaches —
	// never to /mcp, /healthz, /readyz, or /metrics). It cancels the request
	// context once the timeout elapses; handlers that don't observe
	// ctx.Done() are not forcibly aborted, they just run to completion as
	// before (see chi's Timeout doc comment). 0 disables it.
	RequestTimeout time.Duration

	// ClientDefaults, when non-nil, is the env-managed global-defaults
	// ClientSettings layer (config.Config.ClientDefaults, from
	// MEMINI_CLIENT_DEFAULTS). When set it IS the server's global-defaults
	// layer for the config-handshake surface: /v1/handshake and /v1/self
	// resolve through it, GET /v1/settings/defaults reports managed_by=env, and
	// PUT /v1/settings/defaults is refused with 409 — the KV-backed global
	// layer is never consulted. nil (the default) leaves the KV store as the
	// global-defaults layer, unchanged.
	ClientDefaults *store.ClientSettings

	// KeyAuth, when non-nil, is used verbatim as the auth policy instead of
	// New building one from APIKey/APIKeyStore/FileKeys. Set this to share
	// ONE apiauth.Config (and its cache pointer) with another surface mounted
	// in the same process — e.g. MCP's HTTPHandlerWithAuth — so a cache
	// invalidation from a key mutation here (see apikeys.go's Invalidate
	// calls) reaches that surface immediately instead of leaving it to ride
	// out apiauth's table-emptiness cache TTL. nil (the default) preserves
	// pre-existing behavior for callers that don't share a Config.
	KeyAuth *apiauth.Config

	// keyAuth is resolved by New (never set directly by callers): either
	// copied from KeyAuth above, or built from APIKey/APIKeyStore/FileKeys.
	// apiauth.Config's cache must be a pointer shared across every copy of
	// AuthConfig (see its doc), so it is resolved exactly once here rather
	// than per middleware invocation.
	keyAuth apiauth.Config
}

// namespaceMiddleware resolves the tenant namespace and stores it on the
// request context. Returns 400 when the header is present but contains an
// invalid value.
//
// Precedence: X-Memini-Namespace header > the authenticated key's DefaultNS >
// AuthConfig.DefaultNamespace. This is the deliberate OPPOSITE of
// homeMiddleware's precedence below: namespace resolution is CONTEXT — the
// caller picks it per request, and a key's DefaultNS only fills the absence
// of an explicit choice — whereas home resolution is IDENTITY — a bound key's
// home is who the caller is, so it overrides the header outright rather than
// merely defaulting it. See the K2 brief's "SCOPE ADDITION" for the full
// rationale; both middlewares exist because this asymmetry can't be expressed
// as one shared code path without obscuring it.
//
// Must run after authMiddleware: it reads the principal authMiddleware put on
// the context to consult the key's DefaultNS.
func (a AuthConfig) namespaceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Canonicalize the header (trim spaces, strip surrounding slashes,
		// collapse "//") so "work/_shared/" and "work/_shared" address the same
		// rows, ListNamespaces never splits into non-canonical duplicates, and
		// the tenant-shared self-merge guard (which compares against the
		// canonical "<tenant>/_shared") holds. Matches how the server derives
		// its own default namespace (config.sanitizeNamespacePath).
		ns := httputil.NormalizeNamespace(r.Header.Get(a.NamespaceHeader))
		if ns != "" {
			if err := httputil.ValidateNamespace(ns); err != nil {
				httputil.Error(w, http.StatusBadRequest, "invalid namespace: "+err.Error())
				return
			}
		}
		if ns == "" {
			ns = a.DefaultNamespace
			if p, ok := principalFromContext(r.Context()); ok && p.DefaultNS != "" {
				ns = p.DefaultNS
			}
		}
		ctx := context.WithValue(r.Context(), namespaceKey, ns)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// homeMiddleware resolves the caller's personal namespace and stores it on
// the request context. Unlike namespaceMiddleware there is no default when
// the header is absent AND no key is bound — an unset home simply means no
// home leg for the request's read set (RecallInput.Home / BriefingOpts.Home
// stay empty). Returns 400 when the header is present but contains an invalid
// value — EXCEPT for a bound key, which never validates or consults the
// header at all (see below).
//
// Precedence for a key bound to a home namespace (APIKey.HomeNS != ""): the
// key's home namespace ALWAYS wins, identity, not context — X-Memini-Home is
// ignored outright, even when present and even when it names a different,
// perfectly valid namespace. A conflicting header is logged once at debug
// level (never 400: sending a header a bound key can't honor is not a caller
// error, e.g. a shared client config that always sets it). This is the
// deliberate opposite of namespaceMiddleware's header-wins precedence above —
// see its doc comment for the rationale. An unbound key (HomeNS == "", same
// as the admin key / no principal at all) falls through to the ordinary
// header-driven resolution unchanged.
//
// Must run after authMiddleware: it reads the principal authMiddleware put on
// the context.
func (a AuthConfig) homeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := principalFromContext(r.Context()); ok && p.HomeNS != "" {
			if raw := strings.TrimSpace(r.Header.Get(a.HomeHeader)); raw != "" {
				hdr := httputil.NormalizeNamespace(raw)
				if warn := httputil.HomeConflictWarning(p.Name, p.HomeNS, hdr); warn != "" {
					// The override is silent from the caller's side — they asked for
					// one home and got another — so say so loudly enough to be
					// noticed: a warn-level log for the operator, and a response
					// header the client (and the admin UI) can surface. Still never
					// a 400: a shared client config that always sets the header is
					// not a caller error.
					slog.WarnContext(r.Context(), "X-Memini-Home ignored: request key is bound to a home namespace",
						"key", p.Name, "key_home", p.HomeNS, "header_home", hdr)
					w.Header().Set(httputil.WarningHeader, warn)
				}
			}
			ctx := context.WithValue(r.Context(), homeKey, p.HomeNS)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		home := httputil.NormalizeNamespace(r.Header.Get(a.HomeHeader))
		if home != "" {
			if err := httputil.ValidateNamespace(home); err != nil {
				httputil.Error(w, http.StatusBadRequest, "invalid home namespace: "+err.Error())
				return
			}
		}
		ctx := context.WithValue(r.Context(), homeKey, home)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authMiddleware enforces the bearer token via apiauth.Config.Authenticate
// (admin env key first, then a table-key lookup — see its doc for the full
// enforcement rules) and, for a table key, stores the resolved principal on
// the request context for namespaceMiddleware/homeMiddleware and the write
// handlers (attribution) to consume. The admin key authenticates with no
// principal at all, matching its unchanged pre-K2 semantics.
func (a AuthConfig) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		p, ok, err := a.keyAuth.Authenticate(r.Context(), token)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			httputil.Error(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		if p == nil {
			next.ServeHTTP(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), principalKey, *p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// namespaceFromContext returns the resolved namespace for the request.
func namespaceFromContext(ctx context.Context) string {
	ns, _ := ctx.Value(namespaceKey).(string)
	return ns
}

// homeFromContext returns the resolved home namespace for the request, or ""
// when the caller sent no X-Memini-Home header and no bound key applies.
func homeFromContext(ctx context.Context) string {
	home, _ := ctx.Value(homeKey).(string)
	return home
}

// principalFromContext returns the table key that authenticated the request,
// or ok=false for the admin key or auth-disabled dev mode (neither sets a
// principal — see authMiddleware).
func principalFromContext(ctx context.Context) (apiauth.Principal, bool) {
	p, ok := ctx.Value(principalKey).(apiauth.Principal)
	return p, ok
}
