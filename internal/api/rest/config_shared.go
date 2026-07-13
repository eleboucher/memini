package rest

import (
	"context"
	"net/http"
	"strings"

	"github.com/eleboucher/memini/internal/apiauth"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// Shared plumbing for the config-handshake surface (handshake / self /
// settings-defaults / pins). The eight handlers live in handshake.go,
// selfsettings.go, settingsdefaults.go and projectmap.go; this file holds the
// capability accessors, the settings-layer merge, the identity mapping, and the
// store.ClientSettings <-> generated-ClientSettings conversions they share.

// Settings-layer source labels, echoed back as the settings_sources provenance
// (spec enum [default, global, key]) and used as the SettingsLayer.Source labels
// MergeClientSettings attributes each winning field to.
const (
	settingsSourceDefault = "default"
	settingsSourceGlobal  = "global"
	settingsSourceKey     = "key"
)

// eventDetailLayer is the LogConfigEvent detail key naming which settings
// layer changed (settingsSourceGlobal or settingsSourceKey), shared by
// settingsdefaults.go, selfsettings.go and apikeys.go's settings-editing
// handlers so the three don't each repeat the literal "layer".
const eventDetailLayer = "layer"

// projectMapStore type-asserts the backing store to store.ProjectMapStore, the
// optional capability the project-map pins need (keyStore/linkStore's
// precedent). Returns false — a 501 to the caller — for a driver that predates
// it, so a handshake against such a backend resolves derived-only.
func (h *Server) projectMapStore() (store.ProjectMapStore, bool) {
	pms, ok := h.svc.Store().(store.ProjectMapStore)
	return pms, ok
}

// settingsStore type-asserts the backing store to store.ClientSettingsStore,
// the optional capability the server's global-defaults layer needs. Returns
// false for a driver that predates it — the caller then has no KV-backed global
// layer (handshake/self degrade to built-ins; PUT /v1/settings/defaults 501s).
func (h *Server) settingsStore() (store.ClientSettingsStore, bool) {
	ss, ok := h.svc.Store().(store.ClientSettingsStore)
	return ss, ok
}

// globalDefaults returns the server's global-defaults ClientSettings layer and
// how it is managed. env is true when MEMINI_CLIENT_DEFAULTS pins the layer
// (AuthConfig.ClientDefaults): it then IS the layer, the KV store is not
// consulted, and PUT /v1/settings/defaults is refused. capable is false only
// when the layer is neither env-managed nor backed by a ClientSettingsStore —
// the degraded backend where there is no global layer at all (the zero
// ClientSettings is returned, so handshake/self still resolve over the
// built-ins, but GET/PUT /v1/settings/defaults answer 501).
func (h *Server) globalDefaults(ctx context.Context) (s store.ClientSettings, env, capable bool, err error) {
	if h.auth.ClientDefaults != nil {
		return *h.auth.ClientDefaults, true, true, nil
	}
	if ss, ok := h.settingsStore(); ok {
		g, gerr := ss.GlobalClientSettings(ctx)
		if gerr != nil {
			return store.ClientSettings{}, false, true, gerr
		}
		return g, false, true, nil
	}
	return store.ClientSettings{}, false, false, nil
}

// resolveSettingsFor computes a caller's fully-merged ClientSettings and the
// per-field provenance the wire exposes, layering (bottom to top): the built-in
// defaults, the server's global defaults (env-managed or KV), and the caller's
// per-key override. A nil principal (the admin key or dev mode) has no key
// layer. A file-key principal reads the settings loaded from
// MEMINI_API_KEYS_FILE at boot; a table-key principal reads its stored row. The
// result always has every field set (the default layer guarantees it), so it is
// safe to render as a fully resolved ClientSettings.
func (h *Server) resolveSettingsFor(ctx context.Context, principal *apiauth.Principal) (store.ClientSettings, map[string]string, error) {
	global, _, _, err := h.globalDefaults(ctx)
	if err != nil {
		return store.ClientSettings{}, nil, err
	}
	layers := []store.SettingsLayer{
		{Source: settingsSourceDefault, S: store.DefaultClientSettings()},
		{Source: settingsSourceGlobal, S: global},
	}
	if principal != nil {
		key, kerr := h.perKeySettings(ctx, *principal)
		if kerr != nil {
			return store.ClientSettings{}, nil, kerr
		}
		layers = append(layers, store.SettingsLayer{Source: settingsSourceKey, S: key})
	}
	merged, sources := store.MergeClientSettings(layers...)
	return merged, sources, nil
}

// perKeySettings returns the per-key ClientSettings override for a named
// principal: a file key's boot-loaded settings, or a table key's stored row.
// The zero ClientSettings (no override) is returned when the key carries none,
// or when a table-key principal somehow can't be read (the merge then just
// falls through to the global/default layers rather than erroring).
func (h *Server) perKeySettings(ctx context.Context, p apiauth.Principal) (store.ClientSettings, error) {
	if h.auth.FileKeys.IsFileKey(p.Name) {
		for _, k := range h.auth.FileKeys.FileKeys() {
			if k.Name == p.Name {
				return k.Settings, nil
			}
		}
		return store.ClientSettings{}, nil
	}
	ks, ok := h.keyStore()
	if !ok {
		return store.ClientSettings{}, nil
	}
	key, err := findAPIKeyByName(ctx, ks, p.Name)
	if err != nil {
		return store.ClientSettings{}, err
	}
	if key == nil {
		return store.ClientSettings{}, nil
	}
	return key.Settings, nil
}

// principalPtr returns a pointer to the request's authenticated table/file-key
// principal, or nil for the admin key / dev mode (no named principal). The
// settings and identity resolvers take *Principal because "no principal" is a
// meaningful state (no per-key settings layer, nameless identity).
func principalPtr(ctx context.Context) *apiauth.Principal {
	if p, ok := principalFromContext(ctx); ok {
		return &p
	}
	return nil
}

// identityFor maps the request's authenticated principal onto the wire
// CallerIdentity. A named table/file key is authenticated and reports its
// name/home/default; the admin key authenticates with no principal but a
// bearer, so it is authenticated-but-nameless; dev mode (no bearer) is
// unauthenticated. Mirrors the nil-principal distinction requireAdminOrDev
// leans on.
func (h *Server) identityFor(r *http.Request) CallerIdentity {
	if p, ok := principalFromContext(r.Context()); ok {
		id := CallerIdentity{Authenticated: true}
		name := p.Name
		id.KeyName = &name
		if p.HomeNS != "" {
			home := p.HomeNS
			id.Home = &home
		}
		if p.DefaultNS != "" {
			def := p.DefaultNS
			id.DefaultNamespace = &def
		}
		return id
	}
	return CallerIdentity{Authenticated: bearerPresent(r)}
}

// bearerPresent reports whether the request carried a non-empty bearer token —
// the signal that separates the admin key (authenticated, no principal) from
// dev mode (no credential at all) once authMiddleware has already let the
// request through.
func bearerPresent(r *http.Request) bool {
	return strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) != ""
}

// apiReadSet maps the service's resolved read-set entries onto the wire shape,
// shared by GetReadSet and Handshake so both render the read-set identically.
func apiReadSet(entries []service.ReadSetEntry) []ReadSetEntryItem {
	out := make([]ReadSetEntryItem, len(entries))
	for i, e := range entries {
		item := ReadSetEntryItem{Namespace: e.NS, Origin: ReadSetOrigin(e.Origin)}
		if len(e.Tiers) > 0 {
			tiers := make([]Tier, len(e.Tiers))
			for j, t := range e.Tiers {
				tiers[j] = Tier(t)
			}
			item.Tiers = &tiers
		}
		out[i] = item
	}
	return out
}

// apiProjectMapEntry maps a store.ProjectMapEntry onto the wire shape, omitting
// the optional note/created_by when empty.
func apiProjectMapEntry(e store.ProjectMapEntry) ProjectMapEntry {
	out := ProjectMapEntry{Key: e.Key, Namespace: e.Namespace, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
	if e.Note != "" {
		note := e.Note
		out.Note = &note
	}
	if e.CreatedBy != "" {
		by := e.CreatedBy
		out.CreatedBy = &by
	}
	return out
}

// clientSettingsToAPI converts a store.ClientSettings into the generated
// ClientSettings wire shape. The two min-score fields cross the store's float64
// to the wire's float32 here (the schema declares them number/float32); every
// other field is a straight pointer copy, with the enum/label string types
// re-typed.
func clientSettingsToAPI(s store.ClientSettings) ClientSettings {
	out := ClientSettings{
		CaptureTurns:             s.CaptureTurns,
		SessionDigest:            s.SessionDigest,
		InlineExtract:            s.InlineExtract,
		AutoSave:                 s.AutoSave,
		AutoSaveInterval:         s.AutoSaveInterval,
		InjectBriefingPinned:     s.InjectBriefingPinned,
		InjectBriefingFacts:      s.InjectBriefingFacts,
		InjectBriefingProcedures: s.InjectBriefingProcedures,
		InjectBriefingRecent:     s.InjectBriefingRecent,
		InjectBriefingMaxTok:     s.InjectBriefingMaxTok,
		InjectPretoolItems:       s.InjectPretoolItems,
		InjectPretoolMaxTok:      s.InjectPretoolMaxTok,
		InjectPretoolMinScore:    float64PtrToFloat32(s.InjectPretoolMinScore),
		InjectPretoolTools:       s.InjectPretoolTools,
		InjectDedupe:             s.InjectDedupe,
		Recall:                   s.Recall,
		Capture:                  s.Capture,
		RecallLimit:              s.RecallLimit,
		InjectRecallMaxTok:       s.InjectRecallMaxTok,
		InjectRecallMinScore:     float64PtrToFloat32(s.InjectRecallMinScore),
		MinCaptureChars:          s.MinCaptureChars,
		NamespacePrefix:          s.NamespacePrefix,
	}
	if s.NamespaceScope != nil {
		v := ClientSettingsNamespaceScope(*s.NamespaceScope)
		out.NamespaceScope = &v
	}
	if s.InjectLabels != nil {
		labels := make([]ClientSettingsInjectLabels, len(*s.InjectLabels))
		for i, l := range *s.InjectLabels {
			labels[i] = ClientSettingsInjectLabels(l)
		}
		out.InjectLabels = &labels
	}
	return out
}

// clientSettingsFromAPI converts a decoded ClientSettings wire body into a
// store.ClientSettings, the inverse of clientSettingsToAPI. The two min-score
// fields cross back from the wire's float32 to the store's float64. Used by the
// self-settings and settings-defaults PUT handlers before Validate.
func clientSettingsFromAPI(s ClientSettings) store.ClientSettings {
	out := store.ClientSettings{
		CaptureTurns:             s.CaptureTurns,
		SessionDigest:            s.SessionDigest,
		InlineExtract:            s.InlineExtract,
		AutoSave:                 s.AutoSave,
		AutoSaveInterval:         s.AutoSaveInterval,
		InjectBriefingPinned:     s.InjectBriefingPinned,
		InjectBriefingFacts:      s.InjectBriefingFacts,
		InjectBriefingProcedures: s.InjectBriefingProcedures,
		InjectBriefingRecent:     s.InjectBriefingRecent,
		InjectBriefingMaxTok:     s.InjectBriefingMaxTok,
		InjectPretoolItems:       s.InjectPretoolItems,
		InjectPretoolMaxTok:      s.InjectPretoolMaxTok,
		InjectPretoolMinScore:    float32PtrToFloat64(s.InjectPretoolMinScore),
		InjectPretoolTools:       s.InjectPretoolTools,
		InjectDedupe:             s.InjectDedupe,
		Recall:                   s.Recall,
		Capture:                  s.Capture,
		RecallLimit:              s.RecallLimit,
		InjectRecallMaxTok:       s.InjectRecallMaxTok,
		InjectRecallMinScore:     float32PtrToFloat64(s.InjectRecallMinScore),
		MinCaptureChars:          s.MinCaptureChars,
		NamespacePrefix:          s.NamespacePrefix,
	}
	if s.NamespaceScope != nil {
		v := string(*s.NamespaceScope)
		out.NamespaceScope = &v
	}
	if s.InjectLabels != nil {
		labels := make([]string, len(*s.InjectLabels))
		for i, l := range *s.InjectLabels {
			labels[i] = string(l)
		}
		out.InjectLabels = &labels
	}
	return out
}

// float64PtrToFloat32 / float32PtrToFloat64 bridge the store's float64 min-score
// fields and the wire's float32, preserving nil (unset). The cast is
// deliberately explicit at this one boundary rather than scattered through the
// handlers, so the narrowing is auditable and tested (a non-integral value must
// survive the round trip; see the self-settings round-trip test).
func float64PtrToFloat32(p *float64) *float32 {
	if p == nil {
		return nil
	}
	v := float32(*p)
	return &v
}

func float32PtrToFloat64(p *float32) *float64 {
	if p == nil {
		return nil
	}
	v := float64(*p)
	return &v
}
