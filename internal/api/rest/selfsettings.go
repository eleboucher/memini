package rest

import (
	"context"
	"fmt"
	"net/http"

	"github.com/eleboucher/memini/internal/apiauth"
	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/store"
)

// GetSelf implements GET /v1/self — the caller's identity and fully-merged
// behavioral settings, for a client that already has a resolved namespace and
// only wants current identity/settings (no project facts, no read-set). Works
// for every credential class: admin key, dev mode, named table key, file key.
func (h *Server) GetSelf(w http.ResponseWriter, r *http.Request) {
	resp, err := h.selfResponse(r.Context(), r, principalPtr(r.Context()))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	httputil.JSON(w, http.StatusOK, resp)
}

// PutSelfSettings implements PUT /v1/self/settings — full-replace of the
// caller's own per-key settings. Requires a NAMED TABLE key: the admin key and
// dev mode authenticate with no principal (403, pointing at the global-defaults
// endpoint instead), a MEMINI_API_KEYS_FILE key is immutable at runtime (409),
// and a backend that cannot persist per-key settings answers 501. The body
// REPLACES the stored blob — a field omitted from it returns to inheriting the
// global/built-in layers.
func (h *Server) PutSelfSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFromContext(r.Context())
	if !ok {
		httputil.Error(w, http.StatusForbidden,
			"self settings require a named API key; the admin key and dev mode carry no per-key "+
				"settings — use PUT /v1/settings/defaults for the global layer instead")
		return
	}
	if h.auth.FileKeys.IsFileKey(p.Name) {
		httputil.Error(w, http.StatusConflict,
			fmt.Sprintf("api key %q is managed declaratively via MEMINI_API_KEYS_FILE; its settings come from that file, not this API", p.Name))
		return
	}
	ks, ok := h.keyStore()
	if !ok {
		httputil.Error(w, http.StatusNotImplemented, "per-key settings are not supported by this storage backend")
		return
	}

	var req ClientSettings
	if !decode(w, r, &req) {
		return
	}
	settings := clientSettingsFromAPI(req)
	if err := settings.Validate(); err != nil {
		httputil.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	// Read-modify-write to preserve the key's hash/home/default/created_at while
	// replacing only its settings blob. The lookup-then-Put is not
	// transactional — the same documented race as PATCH /v1/keys/{name}
	// (apikeys.go: two concurrent writers could lose one update) — and is
	// acceptable for this low-frequency, self-scoped operation.
	existing, err := findAPIKeyByName(r.Context(), ks, p.Name)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if existing == nil {
		// The request authenticated as this key, so its row existed moments ago;
		// a nil here means it was deleted mid-request. Nothing safe to write.
		writeError(w, r, http.StatusInternalServerError,
			fmt.Errorf("api key %q: not found while saving its settings", p.Name))
		return
	}
	updated := *existing
	updated.Settings = settings
	if err := ks.PutAPIKey(r.Context(), updated); err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	// Settings never affect the auth path (GetAPIKeyByHash ignores them) and
	// resolveSettingsFor reads them fresh from the store on every request, so —
	// unlike the /v1/keys mutations — there is no auth cache to invalidate.
	h.svc.LogConfigEvent(r.Context(), store.EventSettings, "", map[string]any{"key_name": p.Name, "layer": settingsSourceKey})

	resp, err := h.selfResponse(r.Context(), r, &p)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	httputil.JSON(w, http.StatusOK, resp)
}

// selfResponse builds the SelfResponse (identity + merged settings + provenance)
// for a principal, shared by GetSelf and PutSelfSettings (which re-reads after
// the write so the response reflects the update).
func (h *Server) selfResponse(ctx context.Context, r *http.Request, principal *apiauth.Principal) (SelfResponse, error) {
	merged, sources, err := h.resolveSettingsFor(ctx, principal)
	if err != nil {
		return SelfResponse{}, err
	}
	return SelfResponse{
		Identity:        h.identityFor(r),
		Settings:        clientSettingsToAPI(merged),
		SettingsSources: selfSources(sources),
	}, nil
}

// selfSources re-types the merge provenance map onto the generated
// SelfResponse.SettingsSources value type.
func selfSources(m map[string]string) map[string]SelfResponseSettingsSources {
	out := make(map[string]SelfResponseSettingsSources, len(m))
	for k, v := range m {
		out[k] = SelfResponseSettingsSources(v)
	}
	return out
}
