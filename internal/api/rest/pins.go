package rest

import (
	"net/http"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/nsresolve"
	"github.com/eleboucher/memini/internal/store"
)

// The pins surface: GET/PUT/DELETE /v1/pins. A pin is an
// operator-created binding from a project's identity (canonical git remote
// and/or absolute toplevel path) to a namespace, and it beats every other
// namespace_source at handshake time. Any caller may write one — the audit
// trail (activity kind pin/unpin, recording the author) is the control, not an
// authorization gate.

// ListPins implements GET /v1/pins — every explicit project→namespace pin. Open
// to every credential class: pins are machine-wide derivation state,
// not scoped to one namespace. 501 against a backend with no pin store.
func (h *Server) ListPins(w http.ResponseWriter, r *http.Request) {
	pms, ok := h.pinStore()
	if !ok {
		httputil.Error(w, http.StatusNotImplemented, "pins are not supported by this storage backend")
		return
	}
	entries, err := pms.ListPins(r.Context())
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	out := ProjectMapListResponse{Entries: make([]ProjectMapEntry, len(entries))}
	for i, e := range entries {
		out.Entries[i] = apiPin(e)
	}
	httputil.JSON(w, http.StatusOK, out)
}

// PutPin implements PUT /v1/pins — create or replace a pin. The body carries the
// target namespace and at least one key fact (remote_url and/or toplevel_path,
// 400 if neither); the pin is stored under every key the facts yield, so a
// project pinned by both its remote and its path resolves either way. The write
// is activity-logged with its author (kind pin). 501 against a backend with no
// pin store.
func (h *Server) PutPin(w http.ResponseWriter, r *http.Request) {
	pms, ok := h.pinStore()
	if !ok {
		httputil.Error(w, http.StatusNotImplemented, "pins are not supported by this storage backend")
		return
	}
	var req ProjectMapPutRequest
	if !decode(w, r, &req) {
		return
	}
	ns := httputil.NormalizeNamespace(req.Namespace)
	if err := httputil.ValidateNamespace(ns); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid namespace: "+err.Error())
		return
	}
	// A pin's namespace is a real write target for every handshake it resolves,
	// so "*" (reserved for read-set patterns) must never become one — reject it
	// exactly like the move/link handlers do.
	if strings.Contains(ns, "*") {
		httputil.Error(w, http.StatusBadRequest, `invalid namespace: "*" is reserved for read-set patterns`)
		return
	}
	keys := nsresolve.PinKeys(nsresolve.Facts{RemoteURL: deref(req.RemoteUrl), ToplevelPath: deref(req.ToplevelPath)})
	if len(keys) == 0 {
		httputil.Error(w, http.StatusBadRequest, "at least one of remote_url or toplevel_path is required")
		return
	}
	createdBy := ""
	if p, ok := principalFromContext(r.Context()); ok {
		createdBy = p.Name
	}
	note := deref(req.Note)
	now := time.Now().UTC()
	entries := make([]store.Pin, len(keys))
	for i, k := range keys {
		entries[i] = store.Pin{
			Key: k, Namespace: ns, Note: note, CreatedBy: createdBy, CreatedAt: now, UpdatedAt: now,
		}
	}
	if err := pms.PutPins(r.Context(), entries); err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	// The actor now lives on the event row (stamped from the request context),
	// so the detail no longer carries a redundant "by"; key_name-style detail
	// stays the target, never the actor.
	h.svc.LogConfigEvent(r.Context(), store.EventPin, ns, map[string]any{
		"keys": keys, "note": note,
	})
	// Echo the stored row for the first (remote-preferred) key: PutPins
	// preserves an existing pin's created_at/created_by on update, so re-reading
	// reflects the true provenance rather than the "now" we just wrote.
	stored, err := pms.GetPins(r.Context(), keys[:1])
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	out := entries[0]
	if len(stored) > 0 {
		out = stored[0]
	}
	httputil.JSON(w, http.StatusOK, apiPin(out))
}

// DeletePin implements DELETE /v1/pins — remove a pin by its key facts
// (remote_url and/or toplevel_path, 400 if neither). 404 when no matching pin
// exists, 204 on success; the delete is activity-logged (kind unpin). 501
// against a backend with no pin store.
func (h *Server) DeletePin(w http.ResponseWriter, r *http.Request) {
	pms, ok := h.pinStore()
	if !ok {
		httputil.Error(w, http.StatusNotImplemented, "pins are not supported by this storage backend")
		return
	}
	var req ProjectMapDeleteRequest
	if !decode(w, r, &req) {
		return
	}
	keys := nsresolve.PinKeys(nsresolve.Facts{RemoteURL: deref(req.RemoteUrl), ToplevelPath: deref(req.ToplevelPath)})
	if len(keys) == 0 {
		httputil.Error(w, http.StatusBadRequest, "at least one of remote_url or toplevel_path is required")
		return
	}
	// Read the matching pins first: it is the 404 signal, and it captures the
	// pinned namespace + the keys that actually existed for the unpin audit
	// event (DeletePins only returns a count).
	existing, err := pms.GetPins(r.Context(), keys)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if len(existing) == 0 {
		httputil.Error(w, http.StatusNotFound, "no pin matches the given remote_url/toplevel_path")
		return
	}
	n, err := pms.DeletePins(r.Context(), keys)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if n == 0 {
		httputil.Error(w, http.StatusNotFound, "no pin matches the given remote_url/toplevel_path")
		return
	}
	deletedKeys := make([]string, len(existing))
	for i, e := range existing {
		deletedKeys[i] = e.Key
	}
	// The actor lives on the event row now (see PutPin), so the unpin detail
	// carries only the keys it removed — no redundant "by".
	h.svc.LogConfigEvent(r.Context(), store.EventUnpin, existing[0].Namespace, map[string]any{
		"keys": deletedKeys,
	})
	w.WriteHeader(http.StatusNoContent)
}
