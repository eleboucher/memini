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

// apiActivityEvent maps a service event onto the wire shape.
func apiActivityEvent(ev service.ActivityEvent) ActivityEvent {
	out := ActivityEvent{
		OpId:      ev.OpID,
		Kind:      EventKind(ev.Kind),
		Time:      ev.Time,
		Namespace: ev.Namespace,
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
