package rest

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/eleboucher/memini/internal/apiauth"
	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/store"
)

// keyStore type-asserts the backing store to store.APIKeyStore, the optional
// capability interface (linkStore's precedent — see its doc) — returns
// false (501 to the caller) for a driver that predates it.
func (h *Server) keyStore() (store.APIKeyStore, bool) {
	ks, ok := h.svc.Store().(store.APIKeyStore)
	return ks, ok
}

// requireAdminOrDev enforces the K3b admin gating rule (binding): /v1/keys
// is reachable only when the request authenticated with the admin env key,
// or when auth is disabled entirely (dev/bootstrap mode). Both of those
// cases resolve through apiauth.Config.Authenticate with NO principal
// stored on the request context (see authMiddleware) — the admin key
// authenticates with a nil principal exactly like dev mode does, and that
// existing signal already distinguishes exactly what this gate needs: a
// NAMED principal (table or file key) means the caller authenticated with
// something other than the admin key, so it is refused regardless of which
// key it is. Key management is an admin-only surface, not something a named
// credential can grant itself — writes the 403 and returns false when the
// caller must stop.
func requireAdminOrDev(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := principalFromContext(r.Context()); ok {
		httputil.Error(w, http.StatusForbidden,
			"admin key required: /v1/keys manages API keys and is not reachable with a named API key")
		return false
	}
	return true
}

// apiKeyModel maps a store.APIKey onto the spec-generated ApiKey shape,
// tagging it with source (db or file) — never the hash, and (for the
// no-secret variant used by list/get) never any credential material at all.
// CreatedAt is omitted (nil) when zero: a file-sourced key's store.APIKey
// carries no creation timestamp at all (apiauth.validateFileKeyEntry never
// sets one — MEMINI_API_KEYS_FILE is the source of truth, not a database
// row), so emitting the Go zero time would render as a nonsensical
// "0001-01-01" in the UI rather than being recognizably absent.
func apiKeyModel(k store.APIKey, source ApiKeySource) ApiKey {
	out := ApiKey{Name: k.Name, Disabled: k.Disabled, Source: source}
	if !k.CreatedAt.IsZero() {
		out.CreatedAt = &k.CreatedAt
	}
	if k.HomeNS != "" {
		out.Home = &k.HomeNS
	}
	if k.DefaultNS != "" {
		out.DefaultNamespace = &k.DefaultNS
	}
	if k.Settings != (store.ClientSettings{}) {
		s := clientSettingsToAPI(k.Settings)
		out.Settings = &s
	}
	return out
}

// apiKeyWithSecretModel is apiKeyModel plus the plaintext secret, for the
// create/rotate responses that show it exactly once. ApiKeyWithSecret is a
// flattened struct (oapi-codegen's allOf composition doesn't embed the Go
// type), so this can't just wrap apiKeyModel's result. Only ever called for
// a source=db key (create/rotate don't apply to file keys), so CreatedAt is
// always non-zero here — but the same nil-when-zero guard is applied for
// consistency with apiKeyModel.
func apiKeyWithSecretModel(k store.APIKey, source ApiKeySource, secret string) ApiKeyWithSecret {
	out := ApiKeyWithSecret{Name: k.Name, Disabled: k.Disabled, Source: source, Secret: secret}
	if !k.CreatedAt.IsZero() {
		out.CreatedAt = &k.CreatedAt
	}
	if k.HomeNS != "" {
		out.Home = &k.HomeNS
	}
	if k.DefaultNS != "" {
		out.DefaultNamespace = &k.DefaultNS
	}
	if k.Settings != (store.ClientSettings{}) {
		s := clientSettingsToAPI(k.Settings)
		out.Settings = &s
	}
	return out
}

// ListApiKeys implements GET /v1/keys: every table key (source=db) plus
// every declaratively managed file key (source=file, K2b) — never a secret
// or hash. Admin-gated, see requireAdminOrDev.
func (h *Server) ListApiKeys(w http.ResponseWriter, r *http.Request) {
	if !requireAdminOrDev(w, r) {
		return
	}
	ks, ok := h.keyStore()
	if !ok {
		httputil.Error(w, http.StatusNotImplemented, "api keys are not supported by this storage backend")
		return
	}
	dbKeys, err := ks.ListAPIKeys(r.Context())
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	fileKeys := h.auth.FileKeys.FileKeys()
	out := make([]ApiKey, 0, len(dbKeys)+len(fileKeys))
	for _, k := range dbKeys {
		out = append(out, apiKeyModel(k, Db))
	}
	for _, k := range fileKeys {
		out = append(out, apiKeyModel(k, File))
	}
	httputil.JSON(w, http.StatusOK, ApiKeysResponse{Keys: out})
}

// findAPIKeyByName returns the stored key with the given name, or nil when
// none exists. Implemented as ListAPIKeys + filter, mirroring
// cmd/memini/key.go's findAPIKeyByName: the store contract has no by-name
// getter (only by-hash, the auth path), and keys number in the handfuls.
func findAPIKeyByName(ctx context.Context, ks store.APIKeyStore, name string) (*store.APIKey, error) {
	keys, err := ks.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	for i := range keys {
		if keys[i].Name == name {
			return &keys[i], nil
		}
	}
	return nil, nil
}

// normalizeOptionalNamespace normalizes and validates ns, treating an empty
// (or all-whitespace/slash) result as "unset" rather than an error — mirrors
// cmd/memini/key.go's normalizeOptionalNamespace (unexported in that
// package, so re-implemented here rather than shared across an internal/
// boundary neither side already crosses).
func normalizeOptionalNamespace(ns string) (string, error) {
	ns = httputil.NormalizeNamespace(ns)
	if ns == "" {
		return "", nil
	}
	if err := httputil.ValidateNamespace(ns); err != nil {
		return "", fmt.Errorf("invalid namespace %q: %w", ns, err)
	}
	return ns, nil
}

// CreateApiKey implements POST /v1/keys: generates a fresh secret (the one
// canonical apiauth.GenerateSecret), stores the hash, and returns the
// secret exactly once alongside the key's metadata. Admin-gated. 409 if the
// name is already taken by a table OR file key.
func (h *Server) CreateApiKey(w http.ResponseWriter, r *http.Request) {
	if !requireAdminOrDev(w, r) {
		return
	}
	ks, ok := h.keyStore()
	if !ok {
		httputil.Error(w, http.StatusNotImplemented, "api keys are not supported by this storage backend")
		return
	}
	var req CreateApiKeyRequest
	if !decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		httputil.Error(w, http.StatusBadRequest, "api key name must not be empty")
		return
	}
	// A name containing '/' would permanently strand the key: chi's {name}
	// path param (used by UpdateApiKey/DeleteApiKey/RotateApiKey below,
	// unlike {id} on the memory routes there is no unescapeID-style
	// wildcard match here) only matches a single path segment, so
	// "/v1/keys/acme/ci-bot" would never route back to it — only direct
	// store/CLI access could remove such a key afterward. Reject it here,
	// the one place a name is minted, rather than let it become
	// unreachable through this API.
	if strings.Contains(name, "/") {
		httputil.Error(w, http.StatusBadRequest, `api key name must not contain "/"`)
		return
	}
	if h.auth.FileKeys.IsFileKey(name) {
		httputil.Error(w, http.StatusConflict, fmt.Sprintf("api key %q already exists (declared in MEMINI_API_KEYS_FILE)", name))
		return
	}
	existing, err := findAPIKeyByName(r.Context(), ks, name)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if existing != nil {
		httputil.Error(w, http.StatusConflict, fmt.Sprintf("api key %q already exists", name))
		return
	}
	home, herr := normalizeOptionalNamespace(deref(req.Home))
	if herr != nil {
		httputil.Error(w, http.StatusBadRequest, herr.Error())
		return
	}
	defaultNS, derr := normalizeOptionalNamespace(deref(req.DefaultNamespace))
	if derr != nil {
		httputil.Error(w, http.StatusBadRequest, derr.Error())
		return
	}
	secret, err := apiauth.GenerateSecret()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	key := store.APIKey{
		Name:      name,
		Hash:      apiauth.HashToken(secret),
		HomeNS:    home,
		DefaultNS: defaultNS,
		Disabled:  deref(req.Disabled),
		// CreatedAt intentionally left zero: PutAPIKey stamps "now" for a
		// brand-new row (store.APIKeyStore.PutAPIKey's doc).
	}
	if err := ks.PutAPIKey(r.Context(), key); err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	// Invalidate immediately: this is what makes the UI-first bootstrap flow
	// (create the first key while auth is open) enforce auth right away
	// instead of riding apiauth's ~10s table-emptiness cache — see
	// apiauth.Config.Invalidate's doc.
	h.auth.keyAuth.Invalidate()
	stored, err := ks.GetAPIKeyByHash(r.Context(), key.Hash)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if stored == nil {
		writeError(w, r, http.StatusInternalServerError, fmt.Errorf("api key %q: not found immediately after write", name))
		return
	}
	httputil.JSON(w, http.StatusCreated, apiKeyWithSecretModel(*stored, Db, secret))
}

// UpdateApiKey implements PATCH /v1/keys/{name}: preserve-unspecified
// semantics matching `memini key add`'s rotation contract — an omitted
// field (nil pointer) leaves the stored value unchanged; an explicitly
// passed field (including an explicit empty string, or disabled=false)
// overrides it. Admin-gated. 404 absent, 409 for a file-sourced key.
//
// Lookup-then-Put is not transactional (K3 note, carried forward): two
// concurrent PATCHes to the same key could race and one's update could be
// silently lost. Acceptable for an admin-only, low-frequency operation.
func (h *Server) UpdateApiKey(w http.ResponseWriter, r *http.Request, name string) {
	if !requireAdminOrDev(w, r) {
		return
	}
	ks, ok := h.keyStore()
	if !ok {
		httputil.Error(w, http.StatusNotImplemented, "api keys are not supported by this storage backend")
		return
	}
	if h.auth.FileKeys.IsFileKey(name) {
		httputil.Error(w, http.StatusConflict,
			fmt.Sprintf("api key %q is managed declaratively via MEMINI_API_KEYS_FILE, not this API", name))
		return
	}
	var req UpdateApiKeyRequest
	if !decode(w, r, &req) {
		return
	}
	existing, err := findAPIKeyByName(r.Context(), ks, name)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if existing == nil {
		httputil.Error(w, http.StatusNotFound, fmt.Sprintf("no api key named %q", name))
		return
	}
	updated := *existing
	if req.Home != nil {
		home, herr := normalizeOptionalNamespace(*req.Home)
		if herr != nil {
			httputil.Error(w, http.StatusBadRequest, herr.Error())
			return
		}
		updated.HomeNS = home
	}
	if req.DefaultNamespace != nil {
		defaultNS, derr := normalizeOptionalNamespace(*req.DefaultNamespace)
		if derr != nil {
			httputil.Error(w, http.StatusBadRequest, derr.Error())
			return
		}
		updated.DefaultNS = defaultNS
	}
	if req.Disabled != nil {
		updated.Disabled = *req.Disabled
	}
	// Settings: absent (nil) preserves the existing blob untouched (the same
	// preserve-unspecified convention as home/default_namespace/disabled
	// above); present REPLACES it wholesale -- full-replace, not a merge, so
	// a field left out of the new blob re-inherits the global/default layers
	// rather than surviving from the old blob -- matching PUT
	// /v1/self/settings's full-replace semantics (selfsettings.go).
	settingsChanged := req.Settings != nil
	if settingsChanged {
		settings := clientSettingsFromAPI(*req.Settings)
		if err := settings.Validate(); err != nil {
			httputil.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		updated.Settings = settings
	}
	if err := ks.PutAPIKey(r.Context(), updated); err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	// See CreateApiKey's Invalidate comment: a PATCH can flip Disabled,
	// which changes who is currently authenticated as this key's Principal;
	// there's no reason to let that ride the table-emptiness cache either,
	// even though the emptiness reading itself is unaffected by an update
	// that neither adds nor removes a row.
	h.auth.keyAuth.Invalidate()
	if settingsChanged {
		// Same detail shape and namespace attribution ("" -- a per-key
		// settings edit isn't scoped to a namespace) as PUT
		// /v1/self/settings's own LogConfigEvent call; the "key_name" here is
		// the admin-supplied target rather than the caller's own name, since
		// the two endpoints edit different keys' settings.
		h.svc.LogConfigEvent(r.Context(), store.EventSettings, "", map[string]any{"key_name": name, eventDetailLayer: settingsSourceKey})
	}
	httputil.JSON(w, http.StatusOK, apiKeyModel(updated, Db))
}

// DeleteApiKey implements DELETE /v1/keys/{name}. Admin-gated. 404 absent,
// 409 for a file-sourced key.
func (h *Server) DeleteApiKey(w http.ResponseWriter, r *http.Request, name string) {
	if !requireAdminOrDev(w, r) {
		return
	}
	ks, ok := h.keyStore()
	if !ok {
		httputil.Error(w, http.StatusNotImplemented, "api keys are not supported by this storage backend")
		return
	}
	if h.auth.FileKeys.IsFileKey(name) {
		httputil.Error(w, http.StatusConflict,
			fmt.Sprintf("api key %q is managed declaratively via MEMINI_API_KEYS_FILE, not this API", name))
		return
	}
	found, err := ks.DeleteAPIKey(r.Context(), name)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if !found {
		httputil.Error(w, http.StatusNotFound, fmt.Sprintf("no api key named %q", name))
		return
	}
	// Invalidate immediately: deleting the last row must relax the
	// table-emptiness reading back to dev mode right away when no admin key
	// is configured — see apiauth.Config.Invalidate's doc. (In practice this
	// specific row's own credential was never cached in the first place:
	// GetAPIKeyByHash is queried live on every request.)
	h.auth.keyAuth.Invalidate()
	w.WriteHeader(http.StatusNoContent)
}

// RotateApiKey implements POST /v1/keys/{name}/rotate: generates a fresh
// secret (apiauth.GenerateSecret), replacing the stored hash while
// preserving CreatedAt, home/default namespace bindings, and disabled
// state — same contract as `memini key add` re-run against an existing
// name. Admin-gated. 404 absent, 409 for a file-sourced key. Same
// lookup-then-Put non-transactional caveat as UpdateApiKey.
func (h *Server) RotateApiKey(w http.ResponseWriter, r *http.Request, name string) {
	if !requireAdminOrDev(w, r) {
		return
	}
	ks, ok := h.keyStore()
	if !ok {
		httputil.Error(w, http.StatusNotImplemented, "api keys are not supported by this storage backend")
		return
	}
	if h.auth.FileKeys.IsFileKey(name) {
		httputil.Error(w, http.StatusConflict,
			fmt.Sprintf("api key %q is managed declaratively via MEMINI_API_KEYS_FILE, not this API", name))
		return
	}
	existing, err := findAPIKeyByName(r.Context(), ks, name)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if existing == nil {
		httputil.Error(w, http.StatusNotFound, fmt.Sprintf("no api key named %q", name))
		return
	}
	secret, err := apiauth.GenerateSecret()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	updated := *existing
	updated.Hash = apiauth.HashToken(secret)
	if err := ks.PutAPIKey(r.Context(), updated); err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	// The old secret's hash is gone from the row; GetAPIKeyByHash for it
	// will miss on the very next request regardless of this call (it's
	// never cached — see DeleteApiKey's comment) — invalidate anyway for
	// the same reason as CreateApiKey/UpdateApiKey: consistency, and in
	// case Disabled/home changed via a future extension of this handler.
	h.auth.keyAuth.Invalidate()
	stored, err := ks.GetAPIKeyByHash(r.Context(), updated.Hash)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if stored == nil {
		writeError(w, r, http.StatusInternalServerError, fmt.Errorf("api key %q: not found immediately after rotate", name))
		return
	}
	httputil.JSON(w, http.StatusOK, apiKeyWithSecretModel(*stored, Db, secret))
}
