package service

import (
	"context"
	"log/slog"
	"maps"
	"sort"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// eventLogTimeout bounds a detached activity-log write, mirroring
// reinforceTimeout: the log is best-effort, so a wedged store must not leak a
// goroutine for the life of the process.
const eventLogTimeout = 10 * time.Second

// eventSummaryMax caps the snapshot text stored per event row. The feed renders
// a one-line summary, so storing whole memories would bloat the log for nothing.
const eventSummaryMax = 200

// defaultEventsLimit and maxEventsLimit bound a page of the activity feed,
// counted in operations (a recall serving 20 memories is one operation).
const (
	defaultEventsLimit = 50
	maxEventsLimit     = 200
)

// eventRowFanout is how many flat rows Events fetches per requested operation.
// One operation is usually 1 row (a write, a get) and at most recall-K rows, so
// 8x comfortably covers a page of mixed traffic in a single query while keeping
// a pathological all-recalls page to one extra round trip.
const eventRowFanout = 8

// actorCtxKey is the private context key carrying request-scoped attribution.
// A context value (not a parameter on every service method) is what lets the
// actor survive logEvents' fire-and-forget hop: context.WithoutCancel keeps
// values, so a detached background write still stamps the right actor.
type actorCtxKey struct{}

// actor is who a request is attributed to: the name of the API key that
// authenticated it and a kind classifying the empty-name cases. See
// store.Event.Actor/ActorKind for the vocabulary ("key"/"env"/"none"/"").
type actor struct {
	name string
	kind string
}

// WithActor stamps request-scoped attribution onto ctx so every event the
// request logs records who performed it. The REST and MCP surfaces call it
// once, right after authenticating: a named key → (name, "key"); the admin env
// key → ("", "env"); an unauthenticated dev-mode request → ("", "none").
// Attribution is automatic and unconditional — never a setting — so callers
// always stamp; a context with no actor (background maintenance, tests) simply
// logs the legacy "" kind.
func WithActor(ctx context.Context, name, kind string) context.Context {
	return context.WithValue(ctx, actorCtxKey{}, actor{name: name, kind: kind})
}

// actorFromContext returns the request's stamped attribution, or the zero
// (empty) actor when none was stamped — which round-trips as the legacy
// "unknown" row and renders cleanly everywhere.
func actorFromContext(ctx context.Context) actor {
	a, _ := ctx.Value(actorCtxKey{}).(actor)
	return a
}

// eventLog returns the store's activity-log capability, and whether logging is
// both supported and enabled. Drivers that predate the log simply don't
// implement it — the type assertion is the same degrade-gracefully pattern the
// link and API-key capabilities use.
func (s *Service) eventLog() (store.EventLogStore, bool) {
	if !s.eventLogOn {
		return nil, false
	}
	els, ok := s.store.(store.EventLogStore)
	return els, ok
}

// logEvents appends one operation's rows. By default it runs in the background
// so the user-facing call's latency excludes the write; tests force synchronous
// behaviour with WithSyncEventLog. Best-effort throughout: a failure here is
// logged, never returned — losing an audit row must not fail a recall.
func (s *Service) logEvents(ctx context.Context, events []store.Event) {
	els, ok := s.eventLog()
	if !ok || len(events) == 0 {
		return
	}
	// Attribution: every row of one operation shares the request's actor. This
	// is the single funnel every kind flows through (recall, write, briefing,
	// config), so stamping here covers them all in one place.
	a := actorFromContext(ctx)
	for i := range events {
		events[i].Actor = a.name
		events[i].ActorKind = a.kind
	}
	write := func(ctx context.Context) {
		if err := els.AppendEvents(ctx, events); err != nil {
			slog.WarnContext(ctx, "activity: append events failed",
				"kind", string(events[0].Kind), "count", len(events), "err", err)
		}
	}
	if s.syncEventLog {
		write(ctx)
		return
	}
	// Detach from the request lifetime but keep its values; bound the work.
	bg := context.WithoutCancel(ctx)
	s.bg.Go(func() {
		ectx, cancel := context.WithTimeout(bg, eventLogTimeout)
		defer cancel()
		write(ectx)
	})
}

// logRecallEvent records what a recall served and why: the query, and each
// memory's rank and composite score. This is the signal access_count cannot
// carry — a counter says a memory was used, the log says which question it
// answered and how well it scored against it.
//
// finalized is the post-rerank, PRE-floor result list — the single rank space
// every logged row is numbered against. served are the rows the recall returned;
// floored are hits the composite floor (min_rank_score) dropped from the
// response. Floored hits are still logged — visible, not silent — each marked in
// its row detail with {"filtered":"rank_floor"} and carrying its true composite
// score, so the feed shows WHAT was filtered and why. Only the response omitted
// them. budgetOmitted counts what the caller's max_tokens budget trimmed off the
// served set (applyRecallBudget); it rides the operation detail, not a row.
//
// A recall that returned nothing AND floored nothing is still recorded, as a
// single row with no memory: "this query found nothing" is exactly the kind of
// thing you go to an activity feed to discover.
func (s *Service) logRecallEvent(ctx context.Context, in RecallInput, finalized, served, floored []store.Scored, budgetOmitted int) {
	if _, ok := s.eventLog(); !ok {
		return
	}
	detail := map[string]any{}
	if in.Degraded != nil && *in.Degraded != "" {
		detail["degraded"] = *in.Degraded
	}
	// The recall's "why": which integration asked. Recorded verbatim on every
	// row including the zero-hit sentinel, so "who searched for what, and got
	// nothing" is answerable from the feed.
	if in.Source != "" {
		detail["source"] = in.Source
	}
	// How many candidates a dedupe pass suppressed: exclude_ids carries the
	// in-cooldown ids a surface asked the server to drop. Recorded on every row
	// (including the sentinel) so the feed can show what a cooldown pass held
	// back. Absent, not zero, when the caller sent none.
	if len(in.ExcludeIDs) > 0 {
		detail["excluded_count"] = len(in.ExcludeIDs)
	}
	// How many ranked results the caller's max_tokens budget dropped
	// (applyRecallBudget): the server-side visibility for a server-enforced
	// trim — the served rows below are the post-trim set, so without this
	// count the feed could not tell "found 2" from "found 5, budget kept 2".
	// Absent, not zero, when no budget was set or everything fit.
	if budgetOmitted > 0 {
		detail["budget_omitted"] = budgetOmitted
	}
	base := store.Event{
		OpID:      s.newID(),
		Kind:      store.EventRecall,
		Namespace: in.Namespace,
		Query:     in.Query,
		Detail:    detail,
		CreatedAt: s.now(),
	}
	if len(served) == 0 && len(floored) == 0 {
		s.logEvents(ctx, []store.Event{base})
		return
	}
	// One rank space per event: every row's rank is its 1-based position in the
	// finalized pre-floor list. Served and floored rows share it, so a floored
	// hit keeps the exact rank it held and the served ranks show the holes it was
	// dropped from — the feed then shows precisely where the floor bit. With no
	// floor this is identical to the old contiguous 1..N over the response.
	rankByID := make(map[string]int, len(finalized))
	for i, r := range finalized {
		rankByID[r.Memory.ID] = i + 1
	}
	// include_linked rows are not in the finalized list; they follow it, ranked
	// past its end in the order they were served (their score-sorted response
	// position). This matches their no-floor behavior of trailing the direct set.
	nextLinkedRank := len(finalized)
	events := make([]store.Event, 0, len(served)+len(floored))
	// Floored rows are appended first so the highest-id row of the batch — the
	// one groupEvents reads the operation-level detail from — is a served row
	// carrying the clean base detail, never a per-row "filtered" marker.
	for i := range floored {
		r := floored[i]
		e := base
		e.Rank = rankByID[r.Memory.ID]
		e.Score = &floored[i].Score
		e.Detail = filteredDetail(detail)
		applyMemory(&e, r.Memory)
		events = append(events, e)
	}
	for i := range served {
		r := served[i]
		e := base
		if rank, ok := rankByID[r.Memory.ID]; ok {
			e.Rank = rank
		} else {
			nextLinkedRank++
			e.Rank = nextLinkedRank
		}
		e.Score = &served[i].Score
		applyMemory(&e, r.Memory)
		events = append(events, e)
	}
	s.logEvents(ctx, events)
}

// filteredDetail clones an event's operation-level detail and stamps the row as
// dropped by the composite rank floor. A per-row copy — not a mutation of the
// shared base map — keeps the marker off the served rows, which reference base
// by value but share its Detail map by pointer.
func filteredDetail(base map[string]any) map[string]any {
	d := make(map[string]any, len(base)+1)
	maps.Copy(d, base)
	d["filtered"] = "rank_floor"
	return d
}

// InjectedReport is one client injection-telemetry beacon (POST
// /v1/activity/injected): after the server served memories (a recall or a
// briefing), the client hook reports which of them actually reached model
// context and what its local gates held back. Memory ids are taken on faith —
// an unknown id is recorded as-is, never resolved or rejected — because the
// report is best-effort observability, not a write to the memories themselves.
type InjectedReport struct {
	// SessionID is the client session the injection happened in; "" when the
	// client did not say.
	SessionID string
	// Surface is the hook surface that reported: "briefing", "prompt" or
	// "pretool". The transport layer validates it; it is recorded verbatim.
	Surface string
	// Source is the free-form client name (e.g. "claude-code"); "" omits it
	// from the event detail, mirroring RecallInput.Source.
	Source string
	// InjectedIDs are the memory ids actually injected, in injection order.
	// May be empty: a suppression-only report still records (as a single
	// memory-less event, the zero-hit recall's precedent).
	InjectedIDs []string
	// TokensEst and Chars are the client's size estimates for what it
	// injected; nil means unreported (the detail key is then absent, not 0).
	TokensEst *int
	Chars     *int
	// Suppressed counts what the client's local gates held back, by reason.
	Suppressed InjectedSuppressed
}

// InjectedSuppressed counts client-side injection suppressions by reason. A
// zero count means "none reported" for that reason and is omitted from the
// event detail (absent, not 0), like every other optional detail key.
type InjectedSuppressed struct {
	Seen, Cooldown, Budget, Unchanged, Score int
}

// suppressedBucket pairs a suppression reason with its count for iteration.
type suppressedBucket struct {
	reason string
	n      int
}

// buckets returns the (reason, count) pairs in a stable order, so the metrics
// labels and the detail map are built from one vocabulary.
func (s InjectedSuppressed) buckets() []suppressedBucket {
	return []suppressedBucket{
		{"seen", s.Seen}, {"cooldown", s.Cooldown}, {"budget", s.Budget},
		{"unchanged", s.Unchanged}, {"score", s.Score},
	}
}

// RecordInjected records one injection-telemetry report as a single inject
// activity event: the session/surface/source and size estimates ride the
// event detail (the way recall stores its source), the injected ids become
// the event's memory refs, and the suppression counts land under detail
// "suppressed" with zero reasons omitted. Metrics are incremented per report
// regardless of the event log's availability. Best-effort like every other
// event writer: nothing here can fail the request path — the report IS the
// request, so a lost row costs an audit line, never a client error — and it
// degrades to metrics-only against a backend with no activity log.
func (s *Service) RecordInjected(ctx context.Context, namespace string, r InjectedReport) {
	if n := len(r.InjectedIDs); n > 0 {
		s.metrics.InjectedResult(r.Surface, "injected", n)
	}
	for _, b := range r.Suppressed.buckets() {
		if b.n > 0 {
			s.metrics.InjectedResult(r.Surface, "suppressed_"+b.reason, b.n)
		}
	}
	if r.TokensEst != nil && *r.TokensEst > 0 {
		s.metrics.InjectedTokens(r.Surface, *r.TokensEst)
	}
	if _, ok := s.eventLog(); !ok {
		return
	}
	detail := map[string]any{"surface": r.Surface}
	if r.SessionID != "" {
		detail["session_id"] = r.SessionID
	}
	if r.Source != "" {
		detail["source"] = r.Source
	}
	if r.TokensEst != nil {
		detail["injected_tokens_est"] = *r.TokensEst
	}
	if r.Chars != nil {
		detail["injected_chars"] = *r.Chars
	}
	suppressed := map[string]any{}
	for _, b := range r.Suppressed.buckets() {
		if b.n > 0 {
			suppressed[b.reason] = b.n
		}
	}
	if len(suppressed) > 0 {
		detail["suppressed"] = suppressed
	}
	base := store.Event{
		OpID:      s.newID(),
		Kind:      store.EventInject,
		Namespace: namespace,
		Detail:    detail,
		CreatedAt: s.now(),
	}
	if len(r.InjectedIDs) == 0 {
		s.logEvents(ctx, []store.Event{base})
		return
	}
	events := make([]store.Event, 0, len(r.InjectedIDs))
	for i, id := range r.InjectedIDs {
		e := base
		e.MemoryID = id
		e.Rank = i + 1
		events = append(events, e)
	}
	s.logEvents(ctx, events)
}

// briefingExplorationWindow bounds "recently shown" for the briefing's reserved
// exploration slot: a durable item served in any briefing of the namespace
// within this window yields its section's last slot to a staler one.
const briefingExplorationWindow = 7 * 24 * time.Hour

// servedRecencyFloor is the lower age bound on a "recently served" event: serves
// younger than this are ignored. Briefing logs a serve on EVERY call, and a
// SessionStart re-fires within one session (resume/clear/compact); the client
// skips re-injecting a byte-identical briefing, which only stays identical if a
// re-fire sees the SAME served set. Without a floor, the just-logged serves of
// the current sitting would make the whole top-N "served" and rotate the last
// slot on every re-fire, defeating that guard. The floor makes "recently served"
// mean "served in a prior sitting, not this one". A long session that re-fires
// after >1h may rotate once — post-compact re-injection is acceptable there.
const servedRecencyFloor = time.Hour

// recentlyServedIDs returns the set of memory IDs any briefing served in
// namespace between servedRecencyFloor and briefingExplorationWindow before now
// — the input the reserved exploration slot tests candidates against. Scoped to
// the primary namespace because that is where a briefing's serves are logged
// (see logBriefingEvent). It degrades like every other log-dependent path: a
// store with no event log (or logging disabled), or a failed query, yields a nil
// set, and the briefing then ranks by pure DurableScore exactly as before the
// slot existed. now is threaded from s.now() so the floor stays deterministic.
func (s *Service) recentlyServedIDs(ctx context.Context, namespace string, now time.Time) map[string]bool {
	els, ok := s.eventLog()
	if !ok {
		return nil
	}
	rows, err := els.ListEvents(ctx, store.EventFilter{
		Namespace: namespace,
		Kinds:     []store.EventKind{store.EventBriefing},
		Since:     now.Add(-briefingExplorationWindow),
	})
	if err != nil {
		return nil
	}
	floor := now.Add(-servedRecencyFloor)
	served := make(map[string]bool, len(rows))
	for _, r := range rows {
		// Skip this sitting's own serves: an event younger than the floor was
		// logged by a re-fire of the current session, not a prior one.
		if r.CreatedAt.After(floor) {
			continue
		}
		if r.MemoryID != "" {
			served[r.MemoryID] = true
		}
	}
	return served
}

// logBriefingEvent records the memories a session-start briefing served, tagged
// with the section each appeared in. Unlike recall this does not reinforce:
// see the note on Briefing's call site.
func (s *Service) logBriefingEvent(ctx context.Context, namespace string, b Briefing) {
	if _, ok := s.eventLog(); !ok {
		return
	}
	opID := s.newID()
	now := s.now()
	var events []store.Event
	seen := map[string]bool{}
	// Section order is the order the briefing itself presents them, so the
	// first section a memory appears in is the one the reader saw it under.
	for _, sec := range []struct {
		name string
		mems []*memory.Memory
	}{
		{"pinned", b.Pinned},
		{"facts", b.Facts},
		{"procedures", b.Procedures},
		{"recent", b.Recent},
	} {
		for i, m := range sec.mems {
			if m == nil || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			e := store.Event{
				OpID:      opID,
				Kind:      store.EventBriefing,
				Namespace: namespace,
				Rank:      i + 1,
				Detail:    map[string]any{"section": sec.name},
				CreatedAt: now,
			}
			applyMemory(&e, m)
			events = append(events, e)
		}
	}
	if len(events) == 0 {
		return
	}
	s.logEvents(ctx, events)
}

// logWriteEvent records a completed write. One Upsert backs both verbs: an ID
// that already resolved to a row (existing != nil) is an update, anything else
// is a new memory. Distinguishing them here is what lets the feed say "updated"
// rather than "remembered" for MCP's memory_update, which composes Get+Remember.
//
// detail carries what the write decided — its final tier, and any outcome
// flags (an auto-superseded predecessor id, a surfaced merge hint) — built at
// the Upsert call site where that plumbing is in scope.
func (s *Service) logWriteEvent(ctx context.Context, m, existing *memory.Memory, detail map[string]any) {
	kind := store.EventRemember
	if existing != nil {
		kind = store.EventUpdate
	}
	s.logMemoryEvent(ctx, kind, m.Namespace, m, detail)
}

// writeOutcomeDetail builds the detail a write event records: its final tier
// always, plus the outcome flags that fired — the predecessor id an
// auto-supersede will tombstone, and whether the dedup gate surfaced a merge
// hint. A free function so its branches stay out of Remember's cyclomatic
// budget (already at the limit).
func writeOutcomeDetail(tier memory.Tier, supersedeID string, hint *MergeHint) map[string]any {
	d := map[string]any{"tier": string(tier)}
	if supersedeID != "" {
		d["auto_superseded"] = supersedeID
	}
	if hint != nil && hint.SimilarID != "" {
		d["merge_hint"] = true
	}
	return d
}

// logMemoryEvent records a single-memory operation (a get, or a write).
func (s *Service) logMemoryEvent(ctx context.Context, kind store.EventKind, namespace string, m *memory.Memory, detail map[string]any) {
	if _, ok := s.eventLog(); !ok || m == nil {
		return
	}
	e := store.Event{
		OpID:      s.newID(),
		Kind:      kind,
		Namespace: namespace,
		Detail:    detail,
		CreatedAt: s.now(),
	}
	applyMemory(&e, m)
	s.logEvents(ctx, []store.Event{e})
}

// LogConfigEvent records a config-surface write — a pin (EventPin), an unpin
// (EventUnpin), or a behavioral-settings change (EventSettings) — to the
// activity log. Unlike the memory events above these carry no memory snapshot:
// the payload that matters lives in detail (a pin's keys/author/note, which
// settings layer changed), so the event is a single memory-less row. namespace
// is what the event is recorded against (the pinned namespace for a pin/unpin,
// "" for a global-defaults change). Exposed so the REST handlers can log
// through the service that owns logEvents rather than reaching into the store
// themselves. Best-effort like every other log write: a failure is logged,
// never returned, and it is a no-op against a backend with no activity log.
func (s *Service) LogConfigEvent(ctx context.Context, kind store.EventKind, namespace string, detail map[string]any) {
	if _, ok := s.eventLog(); !ok {
		return
	}
	s.logEvents(ctx, []store.Event{{
		OpID:      s.newID(),
		Kind:      kind,
		Namespace: namespace,
		Detail:    detail,
		CreatedAt: s.now(),
	}})
}

// applyMemory stamps the memory snapshot onto an event row. The snapshot is
// what keeps the feed a single query (no per-row fetch) and what keeps a forget
// event readable once its memory no longer exists.
func applyMemory(e *store.Event, m *memory.Memory) {
	e.MemoryID = m.ID
	e.MemoryNS = m.Namespace
	e.MemoryTier = m.Tier
	e.MemorySummary = eventSummary(m)
}

// eventSummary is the memory's own summary, else a clipped prefix of its content.
func eventSummary(m *memory.Memory) string {
	s := m.Summary
	if s == "" {
		s = m.Content
	}
	if len(s) > eventSummaryMax {
		// Clip on a rune boundary so a multi-byte character is never halved.
		r := []rune(s)
		if len(r) > eventSummaryMax {
			return string(r[:eventSummaryMax]) + "…"
		}
	}
	return s
}

// ActivityMemory is one memory as it appeared in an activity event: the
// snapshot taken at serve time, plus why it was there (rank, score, section).
type ActivityMemory struct {
	ID        string
	Namespace string
	Summary   string
	Tier      memory.Tier
	Rank      int
	Score     *float64
	Section   string // briefing only
	// Injected is the served→injected join's verdict for a recall event's
	// memory: true when a nearby inject report named it as actually reaching
	// model context, false when a report existed but omitted it (the client
	// suppressed it). nil when no report covered the serve — absent means
	// unknown, so old data and non-reporting integrations render unchanged,
	// while false means reported-suppressed. See annotateInjected.
	Injected *bool
	// Filtered marks a hit the recall dropped from its response but still logged:
	// "rank_floor" when the composite floor (min_rank_score) cut it. Empty for a
	// served hit. Lets the feed dim what was filtered instead of hiding it.
	Filtered string
}

// ActivityEvent is one logical operation: what happened, when, against which
// namespace, who performed it, and — for a recall — the query and the memories
// it served.
type ActivityEvent struct {
	OpID      string
	Kind      store.EventKind
	Time      time.Time
	Namespace string
	Query     string
	Detail    map[string]any
	// Actor/ActorKind are who performed the operation — see store.Event. Empty
	// on a legacy row that predates attribution.
	Actor     string
	ActorKind string
	Memories  []ActivityMemory
}

// EventsInput selects a page of the activity feed.
//
// Tiers and Text select whole operations rather than individual memories — see
// store.EventFilter — so a filtered recall still reports everything it served.
type EventsInput struct {
	// Namespace restricts the feed to one namespace; "" means every namespace.
	Namespace string
	// Namespaces narrows an all-namespaces feed to these namespaces (OR);
	// ignored when Namespace is set.
	Namespaces []string
	// Kinds restricts to these event kinds; empty means all.
	Kinds []store.EventKind
	// Actor restricts to events performed by the named API key (exact match);
	// empty means no constraint.
	Actor string
	// Tiers restricts to operations that touched a memory of one of these tiers.
	Tiers []memory.Tier
	// Text restricts to operations whose query or a served memory's summary
	// contains it, case-insensitively.
	Text string
	// Since restricts to events at or after the instant.
	Since time.Time
	// Before/BeforeID is the keyset cursor from a previous page.
	Before   time.Time
	BeforeID int64
	// Limit caps the returned operations (not rows); <= 0 uses the default.
	Limit int
}

// EventsPage is one page of the feed, with the cursor for the next.
type EventsPage struct {
	Events       []ActivityEvent
	NextBefore   time.Time
	NextBeforeID int64
	HasMore      bool
}

// Events reads the activity feed, regrouping the store's flat (event, memory)
// rows back into whole operations. Returns ErrUnsupported when the driver has
// no activity log.
//
// The regrouping leans on a property the store guarantees: one operation's rows
// are written as a single batch, so they share a created_at and sit contiguously
// in the (created_at DESC, id DESC) ordering. That lets a flat row page be
// grouped by walking consecutive rows — no join table, no second query.
func (s *Service) Events(ctx context.Context, in EventsInput) (EventsPage, error) {
	els, ok := s.store.(store.EventLogStore)
	if !ok {
		return EventsPage{}, ErrUnsupported
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultEventsLimit
	}
	if limit > maxEventsLimit {
		limit = maxEventsLimit
	}
	rowLimit := limit * eventRowFanout

	rows, err := els.ListEvents(ctx, store.EventFilter{
		Namespace:  in.Namespace,
		Namespaces: in.Namespaces,
		Kinds:      in.Kinds,
		Actor:      in.Actor,
		Tiers:      in.Tiers,
		Text:       in.Text,
		Since:      in.Since,
		Before:     in.Before,
		BeforeID:   in.BeforeID,
		Limit:      rowLimit,
	})
	if err != nil {
		return EventsPage{}, err
	}
	if len(rows) == 0 {
		return EventsPage{}, nil
	}

	groups := groupEvents(rows)

	page := EventsPage{Events: groups}
	truncated := false
	switch {
	case len(groups) > limit:
		page.Events = groups[:limit]
		truncated = true
	case len(rows) == rowLimit && len(groups) > 1:
		// The fetch hit its row cap, so the oldest group may be only partially
		// read — its remaining rows sit just past the boundary. Drop it and let
		// the next page re-read it whole. Guarded on len(groups) > 1 so a single
		// operation larger than rowLimit still renders (truncated) rather than
		// vanishing and stalling the cursor.
		page.Events = groups[:len(groups)-1]
		truncated = true
	}
	if truncated {
		// The cursor is the oldest row we actually kept, so the next page picks
		// up strictly after it.
		last := page.Events[len(page.Events)-1]
		var oldest store.Event
		for _, r := range rows {
			if r.OpID == last.OpID {
				oldest = r
			}
		}
		page.NextBefore = oldest.CreatedAt
		page.NextBeforeID = oldest.ID
		page.HasMore = true
	}
	return page, nil
}

// groupEvents folds consecutive same-operation rows into whole events,
// preserving the newest-first row order across groups. Within a group the rows
// arrive in reverse rank order (they share a created_at, so the id tiebreak
// runs backwards), so memories are re-sorted by rank.
func groupEvents(rows []store.Event) []ActivityEvent {
	var out []ActivityEvent
	for i := 0; i < len(rows); {
		j := i
		for j < len(rows) && rows[j].OpID == rows[i].OpID {
			j++
		}
		head := rows[i]
		ev := ActivityEvent{
			OpID:      head.OpID,
			Kind:      head.Kind,
			Time:      head.CreatedAt,
			Namespace: head.Namespace,
			Query:     head.Query,
			Detail:    head.Detail,
			Actor:     head.Actor,
			ActorKind: head.ActorKind,
		}
		for _, r := range rows[i:j] {
			// The sentinel row of a zero-hit recall carries no memory.
			if r.MemoryID == "" {
				continue
			}
			am := ActivityMemory{
				ID:        r.MemoryID,
				Namespace: r.MemoryNS,
				Summary:   r.MemorySummary,
				Tier:      r.MemoryTier,
				Rank:      r.Rank,
				Score:     r.Score,
			}
			if sec, ok := r.Detail["section"].(string); ok {
				am.Section = sec
			}
			if f, ok := r.Detail["filtered"].(string); ok {
				am.Filtered = f
			}
			ev.Memories = append(ev.Memories, am)
		}
		sort.SliceStable(ev.Memories, func(a, b int) bool {
			return ev.Memories[a].Rank < ev.Memories[b].Rank
		})
		out = append(out, ev)
		i = j
	}
	annotateInjected(out)
	return out
}

// injectAnnotateWindow bounds the served→injected join: an inject report only
// annotates a recall event that happened at or before it, within this window.
// Wide enough that a hook's post-serve beacon (seconds later) always lands,
// tight enough that an unrelated later session rarely does.
const injectAnnotateWindow = 5 * time.Minute

// injectReportView is one inject event as the join consumes it: when and
// against which namespace it was reported, and the ids it names as injected.
type injectReportView struct {
	ns  string
	at  time.Time
	ids map[string]bool
}

// annotateInjected stamps the served→injected flag onto recall events from
// the inject reports in the same fetched page — a pure regrouping-pass join,
// no extra store round-trips (activity queries are bounded). A recall memory
// gets true when a covering report names it, false when covering reports
// exist but none does, and stays nil (unknown) when no report covers the
// recall at all. "Covering" is namespace + time proximity (at or after the
// recall, within injectAnnotateWindow): recall events carry no session id, so
// the report's session_id cannot narrow the match further. A recall whose
// report landed on a newer page than the rows fetched here stays unannotated
// — acceptable for a best-effort feed decoration.
func annotateInjected(events []ActivityEvent) {
	var reports []injectReportView
	for _, ev := range events {
		if ev.Kind != store.EventInject {
			continue
		}
		ids := make(map[string]bool, len(ev.Memories))
		for _, m := range ev.Memories {
			ids[m.ID] = true
		}
		reports = append(reports, injectReportView{ns: ev.Namespace, at: ev.Time, ids: ids})
	}
	if len(reports) == 0 {
		return
	}
	for i := range events {
		ev := &events[i]
		if ev.Kind != store.EventRecall || len(ev.Memories) == 0 {
			continue
		}
		covered := false
		injected := map[string]bool{}
		for _, rep := range reports {
			if rep.ns != ev.Namespace || rep.at.Before(ev.Time) || rep.at.Sub(ev.Time) > injectAnnotateWindow {
				continue
			}
			covered = true
			for id := range rep.ids {
				injected[id] = true
			}
		}
		if !covered {
			continue
		}
		for j := range ev.Memories {
			v := injected[ev.Memories[j].ID]
			ev.Memories[j].Injected = &v
		}
	}
}

// PruneEvents trims the activity log to the configured retention window and row
// cap. Called by the maintenance sweeper; a no-op against a driver with no
// activity log, or when both bounds are unset (keep forever).
func (s *Service) PruneEvents(ctx context.Context, olderThan time.Time, keepMax int) (int64, error) {
	els, ok := s.store.(store.EventLogStore)
	if !ok {
		return 0, nil
	}
	if olderThan.IsZero() && keepMax <= 0 {
		return 0, nil
	}
	return els.PruneEvents(ctx, olderThan, keepMax)
}
