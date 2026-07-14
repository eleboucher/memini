package rest

import (
	"encoding/json"
	"net/http"

	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/store"
)

// GetSettingsDefaults implements GET /v1/settings/defaults — the server's global
// default ClientSettings (merged over the built-ins so every field is present)
// plus managed_by, so the admin UI can render an env-managed layer read-only.
// Admin-gated like /v1/keys: only an admin credential passes — the env key, dev
// mode, or a named key with admin=true. A NON-admin named key gets 403 and sees
// its own merged result via /v1/handshake or /v1/self instead.
func (h *Server) GetSettingsDefaults(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	global, env, capable, err := h.globalDefaults(r.Context())
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if !capable {
		httputil.Error(w, http.StatusNotImplemented, "global default settings are not supported by this storage backend")
		return
	}
	merged, _ := store.MergeClientSettings(
		store.SettingsLayer{Source: settingsSourceDefault, S: store.DefaultClientSettings()},
		store.SettingsLayer{Source: settingsSourceGlobal, S: global},
	)
	managedBy := "api"
	if env {
		managedBy = "env"
	}
	body, err := settingsDefaultsBody(merged, managedBy)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	httputil.JSON(w, http.StatusOK, body)
}

// PutSettingsDefaults implements PUT /v1/settings/defaults — replace the server's
// global default ClientSettings. Admin-gated. Refused with 409 when the layer is
// pinned by MEMINI_CLIENT_DEFAULTS (env-managed: the environment is the single
// source of truth), and 501 against a backend that cannot persist it.
func (h *Server) PutSettingsDefaults(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if h.auth.ClientDefaults != nil {
		httputil.Error(w, http.StatusConflict, "global defaults are managed by MEMINI_CLIENT_DEFAULTS")
		return
	}
	ss, ok := h.settingsStore()
	if !ok {
		httputil.Error(w, http.StatusNotImplemented, "global default settings are not supported by this storage backend")
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
	if err := ss.SetGlobalClientSettings(r.Context(), settings); err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	h.svc.LogConfigEvent(r.Context(), store.EventSettings, "", map[string]any{eventDetailLayer: settingsSourceGlobal})
	// Echo the stored globals merged over the built-ins so the response is a
	// fully resolved ClientSettings (every field present), matching GET minus
	// managed_by (the PUT response schema is a plain ClientSettings).
	merged, _ := store.MergeClientSettings(
		store.SettingsLayer{Source: settingsSourceDefault, S: store.DefaultClientSettings()},
		store.SettingsLayer{Source: settingsSourceGlobal, S: settings},
	)
	httputil.JSON(w, http.StatusOK, clientSettingsToAPI(merged))
}

// settingsDefaultsBody renders a fully-resolved ClientSettings plus managed_by
// as the SettingsDefaultsResponse wire shape. It reuses clientSettingsToAPI via
// a JSON round-trip (the same technique RememberMemory uses to bolt an optional
// field onto a generated model) rather than hand-copying every field into the
// codegen'd allOf struct, which carries its own parallel enum types.
func settingsDefaultsBody(merged store.ClientSettings, managedBy string) (map[string]any, error) {
	raw, err := json.Marshal(clientSettingsToAPI(merged))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	m["managed_by"] = managedBy
	return m, nil
}
