package rest

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/httputil"
)

type ctxKey int

const namespaceKey ctxKey = iota

// AuthConfig configures the optional bearer-token auth and namespace
// resolution applied by Mount to the /v1 route group. Despite the name it also
// carries RequestTimeout: Mount's auth/namespace/timeout middleware are all
// installed together on the same route group, and adding a second options
// struct just for one field would be more ceremony than reuse — see
// internal/api/rest/rest.go's Mount for where each field is consumed.
type AuthConfig struct {
	// APIKey, when non-empty, is required as "Authorization: Bearer <key>".
	APIKey string
	// NamespaceHeader names the request header carrying the tenant namespace.
	NamespaceHeader string
	// DefaultNamespace is used when the header is absent.
	DefaultNamespace string
	// RequestTimeout bounds how long a single /v1 request may run
	// (chi/middleware.Timeout, applied only to the /v1 group Mount attaches —
	// never to /mcp, /healthz, /readyz, or /metrics). It cancels the request
	// context once the timeout elapses; handlers that don't observe
	// ctx.Done() are not forcibly aborted, they just run to completion as
	// before (see chi's Timeout doc comment). 0 disables it.
	RequestTimeout time.Duration
}

// namespaceMiddleware resolves the tenant namespace from the configured header
// (falling back to the default) and stores it on the request context. Returns
// 400 when the header is present but contains an invalid value.
func (a AuthConfig) namespaceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ns := strings.TrimSpace(r.Header.Get(a.NamespaceHeader))
		if ns != "" {
			if err := httputil.ValidateNamespace(ns); err != nil {
				httputil.Error(w, http.StatusBadRequest, "invalid namespace: "+err.Error())
				return
			}
		}
		if ns == "" {
			ns = a.DefaultNamespace
		}
		ctx := context.WithValue(r.Context(), namespaceKey, ns)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authMiddleware enforces the bearer token when an APIKey is configured.
func (a AuthConfig) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.APIKey != "" {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(token), []byte(a.APIKey)) != 1 {
				httputil.Error(w, http.StatusUnauthorized, "missing or invalid bearer token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// namespaceFromContext returns the resolved namespace for the request.
func namespaceFromContext(ctx context.Context) string {
	ns, _ := ctx.Value(namespaceKey).(string)
	return ns
}
