package rest

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/nsresolve"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/version"
)

// Handshake implements POST /v1/handshake — the single source of truth for
// namespace resolution. The client sends what it knows about the project (git
// remote/toplevel/cwd, an optional agent suffix, any namespace it already has
// an opinion about); the server resolves the effective namespace (pin > env >
// declared > derive > key-default > server-default), the caller's identity, the
// fully-merged behavioral settings, and the read-set the resolved namespace
// draws from.
//
// It is deterministic and side-effect-free: the same (credential, facts) in
// yields the same response out, and it performs no writes — only reads (the pin
// lookup, the settings layers, the structural read-set). Clients no longer
// derive or cache the namespace locally.
func (h *Server) Handshake(w http.ResponseWriter, r *http.Request) {
	var req HandshakeRequest
	if !decode(w, r, &req) {
		return
	}
	// cwd_basename is required by the spec but the strict JSON decode only
	// rejects unknown fields, not missing required ones — enforce it here so a
	// caller that sends none gets a clear 400 rather than an empty last-resort
	// derivation.
	if strings.TrimSpace(req.Project.CwdBasename) == "" {
		httputil.Error(w, http.StatusBadRequest, "project.cwd_basename is required")
		return
	}
	facts := nsresolve.Facts{
		RemoteURL:         deref(req.Project.RemoteUrl),
		ToplevelPath:      deref(req.Project.ToplevelPath),
		ToplevelBasename:  deref(req.Project.ToplevelBasename),
		CwdBasename:       req.Project.CwdBasename,
		Agent:             deref(req.Project.Agent),
		EnvNamespace:      deref(req.Project.EnvNamespace),
		DeclaredNamespace: deref(req.Project.DeclaredNamespace),
	}

	ctx := r.Context()
	principal := principalPtr(ctx)

	// One settings resolution feeds both the response and derivation: the
	// caller's effective namespace_scope/namespace_prefix shape the derived
	// name, so they must come from the same merged settings the response
	// reports.
	merged, sources, err := h.resolveSettingsFor(ctx, principal)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}

	// A client-side MEMINI_NAMESPACE_PREFIX overrides the merged
	// namespace_prefix for this request, so one credential (even the admin
	// env key, which has no per-key settings) can serve several namespace trees
	// selected per shell/directory — set the prefix, and derivation composes
	// <prefix>/<repo>. Debug-override semantics: the client env wins over the
	// server-merged value, the same way env_namespace does. Overriding merged
	// here (before Resolve) makes both the derived namespace and the response's
	// reported settings reflect it, from the one merged value.
	if p := strings.TrimSpace(deref(req.Project.EnvNamespacePrefix)); p != "" {
		prefix := p
		merged.NamespacePrefix = &prefix
		sources["namespace_prefix"] = settingsSourceEnv
	}

	// Pins are backed by PinStore; a backend without that capability
	// resolves derived-only (a nil PinLookup skips the pin step). Fetch the
	// candidate pins once, up front — the same entries answer both the lookup
	// and, on a hit, the response's pin block.
	pinLookup, pinEntries, err := h.pinLookup(ctx, facts)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}

	keyDefault := ""
	if principal != nil {
		keyDefault = principal.DefaultNS
	}
	res, err := nsresolve.Resolve(ctx, facts, pinLookup, merged, keyDefault, h.auth.DefaultNamespace)
	if err != nil {
		if errors.Is(err, nsresolve.ErrInvalidInput) {
			httputil.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}

	keyName := ""
	if principal != nil {
		keyName = principal.Name
	}
	logNamespaceResolved(ctx, facts, merged, sources, keyName, res)

	entries, err := h.svc.ResolveReadSetInfo(ctx, res.Namespace, homeFromContext(ctx))
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}

	resp := HandshakeResponse{
		Namespace:       res.Namespace,
		NamespaceSource: HandshakeResponseNamespaceSource(res.Source),
		Identity:        h.identityFor(r),
		Settings:        clientSettingsToAPI(merged),
		SettingsSources: handshakeSources(sources),
		ReadSet:         apiReadSet(entries),
	}
	resp.Server.Version = version.Version
	resp.Server.DefaultNamespace = h.auth.DefaultNamespace
	if res.Source == nsresolve.SourcePin {
		if e, ok := pinEntries[res.PinKey]; ok {
			resp.Pin = &struct {
				CreatedBy *string   `json:"created_by,omitempty"`
				Key       string    `json:"key"`
				Note      *string   `json:"note,omitempty"`
				UpdatedAt time.Time `json:"updated_at"`
			}{Key: e.Key, UpdatedAt: e.UpdatedAt}
			if e.Note != "" {
				note := e.Note
				resp.Pin.Note = &note
			}
			if e.CreatedBy != "" {
				by := e.CreatedBy
				resp.Pin.CreatedBy = &by
			}
		}
	}
	httputil.JSON(w, http.StatusOK, resp)
}

// logNamespaceResolved emits the operator-side record of a handshake's
// namespace decision: who asked (key), what they got (namespace), why
// (source, plus the winning facts and any prefix with its settings layer).
// The handshake response reports all of this too, but only to the client that
// asked — this line is what lets an operator answer "why did that session
// write there?" from the server logs alone, without asking the person whose
// session it was. One line per handshake, so volume tracks session starts,
// not requests.
func logNamespaceResolved(ctx context.Context, f nsresolve.Facts, merged store.ClientSettings,
	sources map[string]string, keyName string, res nsresolve.Result,
) {
	attrs := make([]slog.Attr, 0, 8)
	if keyName != "" {
		attrs = append(attrs, slog.String("key", keyName))
	}
	attrs = append(attrs,
		slog.String("namespace", res.Namespace),
		slog.String("source", res.Source),
		slog.String("cwd", f.CwdBasename),
	)
	if f.RemoteURL != "" {
		attrs = append(attrs, slog.String("remote_url", f.RemoteURL))
	}
	if f.Agent != "" {
		attrs = append(attrs, slog.String("agent", f.Agent))
	}
	if merged.NamespacePrefix != nil && *merged.NamespacePrefix != "" {
		attrs = append(attrs,
			slog.String("prefix", *merged.NamespacePrefix),
			slog.String("prefix_source", sources["namespace_prefix"]))
	}
	if res.Source == nsresolve.SourcePin {
		attrs = append(attrs, slog.String("pin_key", res.PinKey))
	}
	slog.LogAttrs(ctx, slog.LevelInfo, "handshake: namespace resolved", attrs...)
}

// pinLookup builds an nsresolve.PinLookup over the pins table, fetching the
// candidate pins for facts once and returning them by key so the caller can
// also render the matched pin. Returns (nil, nil, nil) — "no pins" — when the
// backend has no PinStore or the facts carry no pin key, so Resolve
// falls through to derivation gracefully.
func (h *Server) pinLookup(ctx context.Context, facts nsresolve.Facts) (nsresolve.PinLookup, map[string]store.Pin, error) {
	pms, ok := h.pinStore()
	if !ok {
		return nil, nil, nil
	}
	keys := nsresolve.PinKeys(facts)
	if len(keys) == 0 {
		return nil, nil, nil
	}
	got, err := pms.GetPins(ctx, keys)
	if err != nil {
		return nil, nil, err
	}
	byKey := make(map[string]store.Pin, len(got))
	for _, e := range got {
		byKey[e.Key] = e
	}
	lookup := func(_ context.Context, candidateKeys []string) (string, string, bool, error) {
		for _, k := range candidateKeys {
			if e, ok := byKey[k]; ok {
				return e.Namespace, e.Key, true, nil
			}
		}
		return "", "", false, nil
	}
	return lookup, byKey, nil
}

// handshakeSources re-types the merge provenance map onto the generated
// HandshakeResponse.SettingsSources value type.
func handshakeSources(m map[string]string) map[string]HandshakeResponseSettingsSources {
	out := make(map[string]HandshakeResponseSettingsSources, len(m))
	for k, v := range m {
		out[k] = HandshakeResponseSettingsSources(v)
	}
	return out
}
