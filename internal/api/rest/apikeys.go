package rest

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

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

// requireAdmin enforces the admin gating rule for the admin-only surfaces
// (/v1/keys CRUD and /v1/settings/defaults): the caller must hold an admin
// credential. Admin is now a per-key capability, not the mere absence of a
// principal. Three classes pass:
//   - the admin env key and dev/bootstrap mode, which both resolve with NO
//     principal on the request context (see authMiddleware) — the nil
//     principal is still the admin/dev signal, unchanged;
//   - a NAMED principal (table or file key) whose Admin flag is set.
//
// A named principal without admin is the only refusal — key management is no
// longer a surface a named credential can never reach, only one a non-admin
// named credential cannot. Writes the 403 and returns false when the caller
// must stop; the message is deliberately different from the old
// "admin key required" so stale out-of-tree matchers fail loudly.
func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if p, ok := principalFromContext(r.Context()); ok && !p.Admin {
		httputil.Error(w, http.StatusForbidden,
			"admin credential required: this endpoint needs the admin env key (MEMINI_API_KEY) or an API key with admin=true")
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
	out := ApiKey{Name: k.Name, Disabled: k.Disabled, Admin: k.Admin, ReadOnly: k.ReadOnly, Source: source}
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
	out := ApiKeyWithSecret{Name: k.Name, Disabled: k.Disabled, Admin: k.Admin, ReadOnly: k.ReadOnly, Source: source, Secret: secret}
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
// or hash. Admin-gated, see requireAdmin.
func (h *Server) ListApiKeys(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
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
	if !requireAdmin(w, r) {
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
		Admin:     deref(req.Admin),
		ReadOnly:  deref(req.ReadOnly),
		// Stamped here rather than left zero for PutAPIKey to fill in, so the
		// echoed response is accurate without a second round trip — the same
		// reasoning as PutLink. Both drivers honour a non-zero CreatedAt.
		CreatedAt: time.Now().UTC(),
	}
	// The row is decided; a client disconnect must not roll it back. This one
	// matters more than most: the secret is returned exactly once and is
	// unrecoverable, so a write that commits while the response is lost strands
	// a key nobody can use.
	kctx, kcancel := store.DurableCtx(r.Context())
	err = ks.PutAPIKey(kctx, key)
	kcancel()
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	// Invalidate immediately: this is what makes the UI-first bootstrap flow
	// (create the first key while auth is open) enforce auth right away
	// instead of riding apiauth's ~10s table-emptiness cache — see
	// apiauth.Config.Invalidate's doc.
	h.auth.keyAuth.Invalidate()
	// Minting an admin credential is a privileged act worth auditing, the same
	// way a settings edit is; a plain key carries no such event.
	if key.Admin {
		h.logCapabilityEvent(r.Context(), name, eventDetailAdmin, true)
	}
	// Likewise for read-only: an operator auditing "when did this credential
	// stop being able to write" needs the grant recorded, not just the revoke.
	if key.ReadOnly {
		h.logCapabilityEvent(r.Context(), name, eventDetailReadOnly, true)
	}
	// Deliberately no read-back. The response is built from the row we just
	// wrote, because every field apiKeyWithSecretModel renders is already known
	// here. Reading it back could only fail, and failing here meant returning
	// 500 for a key that HAD been created — losing the one-time secret forever
	// while leaving the row behind.
	httputil.JSON(w, http.StatusCreated, apiKeyWithSecretModel(key, Db, secret))
}

// logCapabilityEvent records a per-key authorization change as a
// store.EventSettings activity entry (no new EventKind — the same reuse as the
// per-key settings edit at UpdateApiKey), with detail {key_name, <field>: value}
// where field is eventDetailAdmin or eventDetailReadOnly. Called only when a
// create grants a capability or an update flips one, so the activity log carries
// an audit trail of every change to what a credential may do.
func (h *Server) logCapabilityEvent(ctx context.Context, name, field string, value bool) {
	h.svc.LogConfigEvent(ctx, store.EventSettings, "", map[string]any{eventDetailKeyName: name, field: value})
}

// UpdateApiKey implements PATCH /v1/keys/{name}: preserve-unspecified
// semantics matching `memini key add`'s rotation contract — an omitted
// field (nil pointer) leaves the stored value unchanged; an explicitly
// passed field (including an explicit empty string, disabled=false, or
// admin=false) overrides it. Admin-gated. 404 absent, 409 for a file-sourced
// key OR when a named admin key targets itself with a self-demote/self-disable
// (the self-guard below).
//
// Lookup-then-Put is not transactional (K3 note, carried forward): two
// concurrent PATCHes to the same key could race and one's update could be
// silently lost. Acceptable for an admin-only, low-frequency operation.
func (h *Server) UpdateApiKey(w http.ResponseWriter, r *http.Request, name string) {
	if !requireAdmin(w, r) {
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
	// Self-guard: a NAMED admin key acting on ITSELF cannot demote (admin=false)
	// or disable (disabled=true) its own credential — that is the one way an
	// admin could lock the surface against itself in a single call. The env key
	// and dev mode have no principal, so "self" never matches (they are the
	// break-glass escape this points back at). A named admin may still demote or
	// disable a DIFFERENT admin key; the last-admin footgun across two keys is a
	// documented, recoverable state (env key / CLI), not one this prevents.
	if p, ok := principalFromContext(r.Context()); ok && p.Name == name {
		if (req.Admin != nil && !*req.Admin) || (req.Disabled != nil && *req.Disabled) {
			httputil.Error(w, http.StatusConflict, fmt.Sprintf(
				"api key %q cannot demote or disable itself; use the admin env key (MEMINI_API_KEY) or another admin key", name))
			return
		}
		// Imposing read_only on itself is the same class of self-inflicted
		// lockout, and strictly worse to recover from: once the flag is set, the
		// read-only gate refuses this very endpoint, so the key can never lift
		// it. Lifting read_only from itself is allowed — that direction is a
		// restoration, and is only reachable while the key is still writable.
		if req.ReadOnly != nil && *req.ReadOnly {
			httputil.Error(w, http.StatusConflict, fmt.Sprintf(
				"api key %q cannot make itself read-only; a read-only credential cannot reach this endpoint to undo it, "+
					"so use the admin env key (MEMINI_API_KEY), another admin key, or `memini key add %s --read-only`", name, name))
			return
		}
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
	if req.Admin != nil {
		updated.Admin = *req.Admin
	}
	if req.ReadOnly != nil {
		updated.ReadOnly = *req.ReadOnly
	}
	// Whether this PATCH actually flipped the admin capability (grant or
	// revoke), used below to emit an audit event only on a real change — a
	// no-op admin=true against an already-admin key writes nothing.
	adminFlipped := updated.Admin != existing.Admin
	readOnlyFlipped := updated.ReadOnly != existing.ReadOnly
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
		h.svc.LogConfigEvent(r.Context(), store.EventSettings, "", map[string]any{eventDetailKeyName: name, eventDetailLayer: settingsSourceKey})
	}
	if adminFlipped {
		h.logCapabilityEvent(r.Context(), name, eventDetailAdmin, updated.Admin)
	}
	if readOnlyFlipped {
		h.logCapabilityEvent(r.Context(), name, eventDetailReadOnly, updated.ReadOnly)
	}
	httputil.JSON(w, http.StatusOK, apiKeyModel(updated, Db))
}

// DeleteApiKey implements DELETE /v1/keys/{name}. Admin-gated. 404 absent,
// 409 for a file-sourced key OR when a named admin key targets itself (the
// self-guard below, mirroring UpdateApiKey's self-demote/self-disable guard).
func (h *Server) DeleteApiKey(w http.ResponseWriter, r *http.Request, name string) {
	if !requireAdmin(w, r) {
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
	// Self-guard: a named admin key cannot delete its own credential — same
	// break-glass rationale as UpdateApiKey (the env key / dev mode have no
	// principal, so "self" never matches them). A named admin may still delete a
	// DIFFERENT key.
	if p, ok := principalFromContext(r.Context()); ok && p.Name == name {
		httputil.Error(w, http.StatusConflict, fmt.Sprintf(
			"api key %q cannot delete itself; use the admin env key (MEMINI_API_KEY) or another admin key", name))
		return
	}
	// Look up the key before deleting so an admin-key removal can be audited:
	// deleting an admin key removes an admin credential just like a self-demote
	// does, and that capability loss must land in the activity log too.
	existing, err := findAPIKeyByName(r.Context(), ks, name)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
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
	if existing != nil && existing.Admin {
		h.logCapabilityEvent(r.Context(), name, eventDetailAdmin, false)
	}
	// Deliberately no read_only counterpart here. Deleting an admin key removes
	// a privilege and is worth recording; deleting a read-only key removes a
	// RESTRICTION on a credential that no longer exists, so a "read_only: false"
	// event would read as "this key became writable" — the opposite of what
	// happened.
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
// preserving CreatedAt, home/default namespace bindings, disabled state, and
// admin capability — same contract as `memini key add` re-run against an
// existing name. Admin-gated. 404 absent, 409 for a file-sourced key. Same
// lookup-then-Put non-transactional caveat as UpdateApiKey.
//
// Rotate-self is DELIBERATELY allowed — unlike self-demote/self-disable/
// self-delete, rotation is not a capability loss but a credential refresh
// handed back to the prover of the old secret (the response carries the new
// secret), so there is no self-guard here. The `updated := *existing` struct
// copy below preserves Admin across the hash swap for free; a test pins that.
func (h *Server) RotateApiKey(w http.ResponseWriter, r *http.Request, name string) {
	if !requireAdmin(w, r) {
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
	rctx, rcancel := store.DurableCtx(r.Context())
	err = ks.PutAPIKey(rctx, updated)
	rcancel()
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	// The old secret's hash is gone from the row; GetAPIKeyByHash for it
	// will miss on the very next request regardless of this call (it's
	// never cached — see DeleteApiKey's comment) — invalidate anyway for
	// the same reason as CreateApiKey/UpdateApiKey: consistency, and in
	// case Disabled/home changed via a future extension of this handler.
	h.auth.keyAuth.Invalidate()
	// No read-back, for the same reason as CreateApiKey: `updated` is a copy of
	// the row we just read and wrote, so it already carries the true CreatedAt
	// and every other rendered field. A failed read-back used to 500 a rotation
	// that had already happened, invalidating the caller's old secret and
	// withholding the new one.
	httputil.JSON(w, http.StatusOK, apiKeyWithSecretModel(updated, Db, secret))
}
