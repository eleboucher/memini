package rest

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// eventLogStore type-asserts the backing store to store.EventLogStore, the
// optional capability the activity log requires (sqlitevec and postgres both
// implement it). Returns false — a 501 to the caller — for a driver that
// predates it, mirroring linkStore's graceful degrade.
func (h *Server) eventLogStore() bool {
	_, ok := h.svc.Store().(store.EventLogStore)
	return ok
}

// ListActivity implements GET /v1/activity: the newest page of the activity
// log, grouped into whole operations.
func (h *Server) ListActivity(w http.ResponseWriter, r *http.Request, params ListActivityParams) {
	if !h.eventLogStore() {
		httputil.Error(w, http.StatusNotImplemented,
			"the activity log is not supported by this storage backend")
		return
	}
	kinds, err := queryEventKinds(params.Kind)
	if err != nil {
		httputil.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	tiers, err := queryTiers(params.Tier)
	if err != nil {
		httputil.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	in := service.EventsInput{
		Kinds: kinds,
		Actor: strings.TrimSpace(deref(params.Actor)),
		Tiers: tiers,
		Text:  strings.TrimSpace(deref(params.Q)),
		Since: deref(params.Since),
	}
	// An aggregate view ignores the namespace header entirely; a scoped one is
	// pinned to it, exactly as the memory listing behaves. The namespace filter
	// only narrows the aggregate — with a header-scoped feed there is nothing
	// left to narrow.
	if deref(params.AllNamespaces) {
		in.Namespaces = deref(params.Namespace)
	} else {
		in.Namespace = namespaceFromContext(r.Context())
	}
	if params.Limit != nil {
		if *params.Limit < 0 {
			httputil.Error(w, http.StatusBadRequest, fmt.Sprintf("invalid limit %d", *params.Limit))
			return
		}
		in.Limit = *params.Limit
	}
	if params.Before != nil && *params.Before != "" {
		before, beforeID, err := decodeCursor(*params.Before)
		if err != nil {
			httputil.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		in.Before, in.BeforeID = before, beforeID
	}

	page, err := h.svc.Events(r.Context(), in)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}

	out := ActivityResponse{
		Events:  make([]ActivityEvent, len(page.Events)),
		HasMore: page.HasMore,
	}
	for i, ev := range page.Events {
		out.Events[i] = apiActivityEvent(ev)
	}
	if page.HasMore {
		cursor := encodeCursor(page.NextBefore, page.NextBeforeID)
		out.NextCursor = &cursor
	}
	httputil.JSON(w, http.StatusOK, out)
}

// ReportInjected implements POST /v1/activity/injected: the client-side
// injection-telemetry beacon. Validation is deliberately thin — the surface
// must be a known enum member and every count non-negative; memory ids are
// taken on faith (an unknown id is recorded, never a 404) because the report
// is best-effort observability, not a write to the memories. Past validation
// the answer is always 204: RecordInjected never fails the request path, and
// a backend with no activity log still counts the metrics.
func (h *Server) ReportInjected(w http.ResponseWriter, r *http.Request, _ ReportInjectedParams) {
	var req InjectedReport
	if !decode(w, r, &req) {
		return
	}
	// The generated binding does not enforce the spec enum, so an unknown — or
	// missing, decoded as the zero value — surface must be rejected here
	// (mirrors SearchMemories' scope handling).
	if !req.Surface.Valid() {
		httputil.Error(w, http.StatusBadRequest,
			fmt.Sprintf("invalid surface %q: want briefing, prompt, or pretool", string(req.Surface)))
		return
	}
	sup := deref(req.Suppressed)
	for _, n := range []*int{
		req.InjectedTokensEst, req.InjectedChars,
		sup.Seen, sup.Cooldown, sup.Budget, sup.Unchanged, sup.Score,
	} {
		if n != nil && *n < 0 {
			httputil.Error(w, http.StatusBadRequest, "counts must be >= 0")
			return
		}
	}
	h.svc.RecordInjected(r.Context(), namespaceFromContext(r.Context()), service.InjectedReport{
		SessionID:   strings.TrimSpace(deref(req.SessionId)),
		Surface:     string(req.Surface),
		Source:      strings.TrimSpace(deref(req.Source)),
		InjectedIDs: deref(req.InjectedIds),
		TokensEst:   req.InjectedTokensEst,
		Chars:       req.InjectedChars,
		Suppressed: service.InjectedSuppressed{
			Seen:      deref(sup.Seen),
			Cooldown:  deref(sup.Cooldown),
			Budget:    deref(sup.Budget),
			Unchanged: deref(sup.Unchanged),
			Score:     deref(sup.Score),
		},
	})
	w.WriteHeader(http.StatusNoContent)
}

// apiActivityEvent maps a service event onto the wire shape.
func apiActivityEvent(ev service.ActivityEvent) ActivityEvent {
	out := ActivityEvent{
		OpId:      ev.OpID,
		Kind:      EventKind(ev.Kind),
		Time:      ev.Time,
		Namespace: ev.Namespace,
	}
	// Attribution: a named key surfaces its name; every actor with a known kind
	// surfaces the kind. A legacy row (empty kind) omits both, rendering as a
	// clean "unknown" client-side.
	if ev.Actor != "" {
		actor := ev.Actor
		out.Actor = &actor
	}
	if ev.ActorKind != "" {
		kind := ActivityEventActorKind(ev.ActorKind)
		out.ActorKind = &kind
	}
	if ev.Query != "" {
		q := ev.Query
		out.Query = &q
	}
	if len(ev.Detail) > 0 {
		d := ev.Detail
		out.Detail = &d
	}
	if len(ev.Memories) > 0 {
		mems := make([]ActivityMemory, len(ev.Memories))
		for i, m := range ev.Memories {
			am := ActivityMemory{
				Id:        m.ID,
				Namespace: m.Namespace,
				Summary:   m.Summary,
				Tier:      Tier(m.Tier),
				Score:     m.Score,
			}
			if m.Rank > 0 {
				rank := m.Rank
				am.Rank = &rank
			}
			if m.Section != "" {
				sec := m.Section
				am.Section = &sec
			}
			// Absent-vs-false is the contract: only a report ever sets the
			// pointer, so an uncovered serve omits the key entirely.
			if m.Injected != nil {
				injected := *m.Injected
				am.Injected = &injected
			}
			mems[i] = am
		}
		out.Memories = &mems
	}
	return out
}

// encodeCursor renders a keyset position as an opaque "<unixmilli>-<id>" token.
// It is opaque by contract — clients round-trip it verbatim — so its shape can
// change without a spec change.
func encodeCursor(t time.Time, id int64) string {
	return strconv.FormatInt(t.UnixMilli(), 10) + "-" + strconv.FormatInt(id, 10)
}

// decodeCursor parses a token produced by encodeCursor.
func decodeCursor(s string) (time.Time, int64, error) {
	ms, idStr, ok := strings.Cut(s, "-")
	if !ok {
		return time.Time{}, 0, fmt.Errorf("invalid cursor %q", s)
	}
	msec, err := strconv.ParseInt(ms, 10, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("invalid cursor %q", s)
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("invalid cursor %q", s)
	}
	return time.UnixMilli(msec).UTC(), id, nil
}
