package rest

import (
	"net/http"

	"github.com/eleboucher/memini/internal/httputil"
)

// STUBS — config-handshake redesign, Phase 1 (contract only).
//
// This file implements the eight ServerInterface methods the config-handshake
// wire contract (api/openapi.yaml's /v1/handshake, /v1/self, /v1/self/settings,
// /v1/settings/defaults, /v1/project-map) adds, so `var _ ServerInterface =
// (*Server)(nil)` in rest.go keeps compiling and the generated router has
// something to dispatch to. Every method here returns 501 unconditionally —
// there is no service-layer method, no storage, and no identity/settings
// resolution behind them yet.
//
// A later phase (storage: project_map + api_keys.settings; server logic: the
// actual handshake/self/settings/project-map handlers) replaces this entire
// file. Nothing here should be extended in place — add the real
// implementation elsewhere and delete the corresponding stub.
const configHandshakeNotImplemented = "config-handshake redesign: not implemented yet " +
	"(contract only, see internal/api/rest/config_stubs.go)"

// Handshake implements POST /v1/handshake.
func (h *Server) Handshake(w http.ResponseWriter, r *http.Request) {
	httputil.Error(w, http.StatusNotImplemented, configHandshakeNotImplemented)
}

// GetSelf implements GET /v1/self.
func (h *Server) GetSelf(w http.ResponseWriter, r *http.Request) {
	httputil.Error(w, http.StatusNotImplemented, configHandshakeNotImplemented)
}

// PutSelfSettings implements PUT /v1/self/settings.
func (h *Server) PutSelfSettings(w http.ResponseWriter, r *http.Request) {
	httputil.Error(w, http.StatusNotImplemented, configHandshakeNotImplemented)
}

// GetSettingsDefaults implements GET /v1/settings/defaults.
func (h *Server) GetSettingsDefaults(w http.ResponseWriter, r *http.Request) {
	httputil.Error(w, http.StatusNotImplemented, configHandshakeNotImplemented)
}

// PutSettingsDefaults implements PUT /v1/settings/defaults.
func (h *Server) PutSettingsDefaults(w http.ResponseWriter, r *http.Request) {
	httputil.Error(w, http.StatusNotImplemented, configHandshakeNotImplemented)
}

// ListProjectMap implements GET /v1/project-map.
func (h *Server) ListProjectMap(w http.ResponseWriter, r *http.Request) {
	httputil.Error(w, http.StatusNotImplemented, configHandshakeNotImplemented)
}

// PutProjectMapPin implements PUT /v1/project-map.
func (h *Server) PutProjectMapPin(w http.ResponseWriter, r *http.Request) {
	httputil.Error(w, http.StatusNotImplemented, configHandshakeNotImplemented)
}

// DeleteProjectMapPin implements DELETE /v1/project-map.
func (h *Server) DeleteProjectMapPin(w http.ResponseWriter, r *http.Request) {
	httputil.Error(w, http.StatusNotImplemented, configHandshakeNotImplemented)
}
