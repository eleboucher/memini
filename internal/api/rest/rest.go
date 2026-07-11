// Package rest exposes memini's HTTP/JSON API. The surface is API-first: the
// routes, parameters, and request/response models are generated from
// api/openapi.yaml (see gen.go / api.gen.go); this file implements the
// generated ServerInterface on top of the service layer. The MCP surface
// (internal/api/mcp) shares the same service.
package rest

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// Server implements the spec-generated ServerInterface backed by a service.Service.
type Server struct {
	svc  *service.Service
	auth AuthConfig
}

var _ ServerInterface = (*Server)(nil)

// New builds the REST server.
func New(svc *service.Service, auth AuthConfig) *Server {
	return &Server{svc: svc, auth: auth}
}

// Mount attaches the spec-generated /v1 routes to r, wrapped in namespace +
// auth middleware. Binding failures on declared parameters (e.g. ?limit=abc)
// are rejected with 400 by the generated wrappers. Callers that also mount
// long-lived streaming routes (e.g. the MCP SSE handler) must mount those
// directly on r, outside this group — Timeout below would sever them.
func (h *Server) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		// Authentication first: an unauthenticated caller gets 401, never a
		// 400 that leaks namespace-validation behavior.
		r.Use(h.auth.authMiddleware)
		r.Use(h.auth.namespaceMiddleware)
		r.Use(h.auth.homeMiddleware)
		// Timeout is innermost (closest to the handler) so it bounds only
		// the generated handler's own work, not the surrounding auth checks.
		// It cancels the request context past the deadline; it does not
		// forcibly abort a handler that ignores ctx.Done() (see AuthConfig's
		// RequestTimeout doc).
		if h.auth.RequestTimeout > 0 {
			r.Use(middleware.Timeout(h.auth.RequestTimeout))
		}
		HandlerWithOptions(h, ChiServerOptions{
			BaseRouter: r,
			ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
				httputil.Error(w, http.StatusBadRequest, err.Error())
			},
		})
	})
}

// statusFor maps a service error to an HTTP status: caller mistakes are 4xx,
// backend failures 5xx (so outages alert and clients retry instead of being
// blamed with a 400).
func statusFor(err error) int {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, store.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, embed.ErrDisabled):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// writeError logs and scrubs 500s (the unrecognised-error bucket, whose wrapped
// chain can hold SQL/driver/filesystem text) to a generic body; every other
// status returns its deliberate caller-facing message verbatim.
func writeError(w http.ResponseWriter, r *http.Request, status int, err error) {
	if status == http.StatusInternalServerError {
		slog.ErrorContext(r.Context(), "request failed",
			"method", r.Method, "path", r.URL.Path, "status", status,
			"request_id", middleware.GetReqID(r.Context()), "err", err)
		httputil.Error(w, status, "internal error")
		return
	}
	httputil.Error(w, status, err.Error())
}

// RunFsck implements POST /v1/fsck.
func (h *Server) RunFsck(w http.ResponseWriter, r *http.Request, _ RunFsckParams) {
	report, err := h.svc.Fsck(r.Context())
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	out := FsckReport{
		ExpiredPurged:    report.ExpiredPurged,
		ShortTermEvicted: report.ShortTermEvicted,
		Namespaces:       report.Namespaces,
	}
	if len(report.DuplicateGroups) > 0 {
		out.DuplicateGroups = &report.DuplicateGroups
	}
	httputil.JSON(w, http.StatusOK, out)
}

// RunDedup implements POST /v1/dedup. The optional body tunes the pass; the
// zero value uses the production defaults. The pass is scoped to the request's
// namespace unless all_namespaces is set. Dry-run reports what would happen
// without tombstoning.
func (h *Server) RunDedup(w http.ResponseWriter, r *http.Request, _ RunDedupParams) {
	in := service.DedupInput{Namespaces: []string{namespaceFromContext(r.Context())}}
	if r.ContentLength != 0 {
		var req DedupRequest
		if !decode(w, r, &req) {
			return
		}
		if req.Similarity != nil {
			in.Similarity = *req.Similarity
		}
		if req.MinClusterSize != nil {
			in.MinClusterSize = *req.MinClusterSize
		}
		if req.NeighboursPerAnchor != nil {
			in.NeighboursPerAnchor = *req.NeighboursPerAnchor
		}
		if req.DryRun != nil {
			in.DryRun = *req.DryRun
		}
		if req.AllNamespaces != nil && *req.AllNamespaces {
			in.Namespaces = nil // empty → every namespace
		}
		if req.Tiers != nil && len(*req.Tiers) > 0 {
			tiers := make([]memory.Tier, len(*req.Tiers))
			for i, t := range *req.Tiers {
				tiers[i] = memory.Tier(t)
			}
			in.Tiers = tiers
		}
	}
	report, err := h.svc.Dedup(r.Context(), in)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	out := DedupReport{
		Namespaces:    report.Namespaces,
		MemoriesSeen:  report.MemoriesSeen,
		ClustersFound: report.ClustersFound,
		Tombstoned:    report.Tombstoned,
		DryRun:        report.DryRun,
	}
	if len(report.Actions) > 0 {
		actions := make([]ClusterAction, len(report.Actions))
		for i, a := range report.Actions {
			actions[i] = ClusterAction{
				RepresentativeId: a.RepresentativeID,
				TombstonedIds:    a.TombstonedIDs,
				Size:             a.Size,
			}
		}
		out.Actions = &actions
	}
	httputil.JSON(w, http.StatusOK, out)
}

// RememberMemory implements POST /v1/memories.
func (h *Server) RememberMemory(w http.ResponseWriter, r *http.Request, _ RememberMemoryParams) {
	var req RememberRequest
	if !decode(w, r, &req) {
		return
	}
	in := service.RememberInput{
		Namespace: namespaceFromContext(r.Context()),
		Home:      homeFromContext(r.Context()),
		Content:   req.Content,
	}
	in.Tier = memory.Tier(deref(req.Tier))
	in.Level = memory.Level(deref(req.Level))
	in.Summary = deref(req.Summary)
	in.Tags = deref(req.Tags)
	in.Metadata = deref(req.Metadata)
	in.Importance = deref(req.Importance)
	in.ID = deref(req.Id)
	in.Confidence = req.Confidence
	in.ValidFrom = req.ValidFrom
	in.ValidTo = req.ValidTo
	in.Visibility = deref(req.Visibility)
	if req.TtlSeconds != nil {
		d := time.Duration(*req.TtlSeconds) * time.Second
		in.TTL = &d
	}

	var hint service.MergeHint
	var superseded bool
	in.MergeHint = &hint
	in.AutoSuperseded = &superseded
	m, err := h.svc.Remember(r.Context(), in)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	// A nil memory with no error means the episodic value gate dropped the write:
	// accepted, not stored.
	if m == nil {
		httputil.JSON(w, http.StatusOK, map[string]any{"stored": false, "reason": "low_signal"})
		return
	}
	// Build the response via JSON round-trip so we can add the optional
	// merge_hint and auto_superseded fields without duplicating the
	// apiMemory field-by-field copy.
	respBytes, err := json.Marshal(apiMemory(m))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	var resp map[string]any
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if hint.SimilarID != "" {
		const tierKey = "tier"
		resp["merge_hint"] = map[string]any{
			"similar_id":      hint.SimilarID,
			"similar_content": hint.SimilarContent,
			"score":           hint.Score,
			tierKey:           string(hint.Tier),
		}
	}
	if superseded {
		resp["auto_superseded"] = true
	}
	httputil.JSON(w, http.StatusCreated, resp)
}

// unescapeID percent-decodes a bound {id} path param. chi matches on the
// escaped path, so the generated wrapper binds the raw segment; ids with
// reserved chars like ':' (e.g. imported "openclaw:main:<uuid>") arrive as %3A
// and must be decoded to match the stored literal. A malformed encoding yields
// ok=false.
func unescapeID(bound string) (string, bool) {
	id, err := url.PathUnescape(bound)
	return id, err == nil
}

// GetMemory implements GET /v1/memories/{id}.
func (h *Server) GetMemory(w http.ResponseWriter, r *http.Request, boundID string, _ GetMemoryParams) {
	id, ok := unescapeID(boundID)
	if !ok {
		httputil.Error(w, http.StatusNotFound, "memory not found")
		return
	}
	m, err := h.svc.Get(r.Context(), namespaceFromContext(r.Context()), id)
	if errors.Is(err, store.ErrNotFound) {
		httputil.Error(w, http.StatusNotFound, "memory not found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	httputil.JSON(w, http.StatusOK, apiMemory(m))
}

// GetMemoryHistory implements GET /v1/memories/{id}/history.
func (h *Server) GetMemoryHistory(w http.ResponseWriter, r *http.Request, boundID string, _ GetMemoryHistoryParams) {
	id, ok := unescapeID(boundID)
	if !ok {
		httputil.Error(w, http.StatusNotFound, "memory not found")
		return
	}
	mems, err := h.svc.History(r.Context(), namespaceFromContext(r.Context()), id)
	if errors.Is(err, store.ErrNotFound) {
		httputil.Error(w, http.StatusNotFound, "memory not found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	out := ListResponse{Memories: make([]Memory, len(mems))}
	for i, m := range mems {
		out.Memories[i] = apiMemory(m)
	}
	httputil.JSON(w, http.StatusOK, out)
}

// ForgetMemory implements DELETE /v1/memories/{id}.
func (h *Server) ForgetMemory(w http.ResponseWriter, r *http.Request, boundID string, _ ForgetMemoryParams) {
	id, ok := unescapeID(boundID)
	if !ok {
		httputil.Error(w, http.StatusNotFound, "memory not found")
		return
	}
	err := h.svc.Forget(r.Context(), namespaceFromContext(r.Context()), id)
	if errors.Is(err, store.ErrNotFound) {
		httputil.Error(w, http.StatusNotFound, "memory not found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SupersedeMemory implements POST /v1/memories/{id}/supersede. Stamps
// superseded_by + valid_to so default recall hides the row while the
// audit chain and time-travel (AsOf) queries can still reach it.
func (h *Server) SupersedeMemory(w http.ResponseWriter, r *http.Request, boundID string, _ SupersedeMemoryParams) {
	id, ok := unescapeID(boundID)
	if !ok {
		httputil.Error(w, http.StatusNotFound, "memory not found")
		return
	}
	var req SupersedeRequest
	if !decode(w, r, &req) {
		return
	}
	err := h.svc.Supersede(r.Context(), namespaceFromContext(r.Context()), id, req.By)
	if errors.Is(err, store.ErrNotFound) {
		httputil.Error(w, http.StatusNotFound, "memory not found")
		return
	}
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ForgetByTag implements DELETE /v1/memories?tag=... — bulk-delete every memory
// in the namespace carrying the tag. The tag is required (spec-enforced) so a
// missing tag cannot delete the whole namespace.
func (h *Server) ForgetByTag(w http.ResponseWriter, r *http.Request, params ForgetByTagParams) {
	n, err := h.svc.ForgetByTag(r.Context(), namespaceFromContext(r.Context()), params.Tag)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	httputil.JSON(w, http.StatusOK, DeleteByTagResponse{Deleted: int(n)})
}

// SearchMemories implements POST /v1/search.
func (h *Server) SearchMemories(w http.ResponseWriter, r *http.Request, _ SearchMemoriesParams) {
	var req SearchRequest
	if !decode(w, r, &req) {
		return
	}
	tiers, err := domainTiers(req.Tiers)
	if err != nil {
		httputil.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	levels, err := domainLevels(req.Levels)
	if err != nil {
		httputil.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	in := service.RecallInput{
		Namespace: namespaceFromContext(r.Context()),
		Home:      homeFromContext(r.Context()),
		Query:     req.Query,
		Tiers:     tiers,
		Levels:    levels,
	}
	in.Tags = deref(req.Tags)
	in.Metadata = deref(req.Metadata)
	in.ExcludeMetadata = deref(req.ExcludeMetadata)
	if req.IncludeFreshTurns != nil {
		in.IncludeFreshTurns = *req.IncludeFreshTurns
	}
	if req.QueryRewrite != nil {
		in.QueryRewrite = *req.QueryRewrite
	}
	in.Limit = deref(req.Limit)
	in.IncludeExpired = deref(req.IncludeExpired)
	in.IncludeSuperseded = deref(req.IncludeSuperseded)
	if req.AsOf != nil {
		in.AsOf = req.AsOf.UTC()
	}
	// The generated binding does not enforce the spec enum, so an unknown
	// scope must be rejected here rather than silently searched as exact
	// (mirrors GetBriefing).
	if req.Scope != nil {
		if !req.Scope.Valid() {
			httputil.Error(w, http.StatusBadRequest, invalidScopeMsg(string(*req.Scope)))
			return
		}
		in.Scope = restScopeAlias(string(*req.Scope))
	}
	// Explicit namespaces REPLACE the default read set; the service layer
	// validates entries and enforces the 16-entry cap (ErrInvalidInput → 400).
	in.Namespaces = deref(req.Namespaces)
	if req.MinScore != nil {
		in.MinScore = *req.MinScore
	}
	var degraded string
	in.Degraded = &degraded
	var readset []service.ReadSetEntry
	in.ReadSet = &readset

	res, err := h.svc.Recall(r.Context(), in)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	out := SearchResponse{Results: apiScored(res, service.OriginMap(readset))}
	if degraded != "" {
		keywordOnly := "keyword_only"
		note := "semantic search unavailable (" + degraded + "); results are keyword-only and may be incomplete"
		out.Degraded = &keywordOnly
		out.Note = &note
	}
	httputil.JSON(w, http.StatusOK, out)
}

// AnswerQuestion implements POST /v1/answer.
func (h *Server) AnswerQuestion(w http.ResponseWriter, r *http.Request, _ AnswerQuestionParams) {
	var req AnswerRequest
	if !decode(w, r, &req) {
		return
	}
	tiers, err := domainTiers(req.Tiers)
	if err != nil {
		httputil.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	levels, err := domainLevels(req.Levels)
	if err != nil {
		httputil.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	in := service.AnswerInput{
		Namespace: namespaceFromContext(r.Context()),
		Home:      homeFromContext(r.Context()),
		Query:     req.Query,
		Tiers:     tiers,
		Levels:    levels,
	}
	in.Tags = deref(req.Tags)
	in.Metadata = deref(req.Metadata)
	in.Limit = deref(req.Limit)
	// The generated binding does not enforce the spec enum, so an unknown
	// scope must be rejected here rather than silently run as full (mirrors
	// SearchMemories).
	if req.Scope != nil {
		if !req.Scope.Valid() {
			httputil.Error(w, http.StatusBadRequest, invalidScopeMsg(string(*req.Scope)))
			return
		}
		in.Scope = restScopeAlias(string(*req.Scope))
	}
	var readset []service.ReadSetEntry
	in.ReadSet = &readset

	res, err := h.svc.Answer(r.Context(), in)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	httputil.JSON(w, http.StatusOK, AnswerResponse{
		Answer: res.Answer, Sources: apiScored(res.Sources, service.OriginMap(readset)),
	})
}

// ListMemories implements GET /v1/memories.
func (h *Server) ListMemories(w http.ResponseWriter, r *http.Request, params ListMemoriesParams) {
	tiers, err := queryTiers(params.Tier)
	if err != nil {
		httputil.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	levels, err := queryLevels(params.Level)
	if err != nil {
		httputil.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	meta, err := parseMetaFilters(params.Meta)
	if err != nil {
		httputil.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	in := service.ListInput{
		Namespace: namespaceFromContext(r.Context()),
		Tiers:     tiers,
		Levels:    levels,
		Tags:      queryTags(params.Tag),
		Metadata:  meta,
	}
	in.IncludeExpired = deref(params.IncludeExpired)
	in.IncludeSuperseded = deref(params.IncludeSuperseded)
	in.AllNamespaces = deref(params.AllNamespaces)
	if params.Limit != nil {
		if *params.Limit < 0 {
			httputil.Error(w, http.StatusBadRequest, fmt.Sprintf("invalid limit %d", *params.Limit))
			return
		}
		in.Limit = *params.Limit
	}

	mems, err := h.svc.List(r.Context(), in)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	out := ListResponse{Memories: make([]Memory, len(mems))}
	for i, m := range mems {
		out.Memories[i] = apiMemory(m)
	}
	httputil.JSON(w, http.StatusOK, out)
}

// restScopeAlias maps the REST scope enum onto the service's
// project/full/everywhere vocabulary (service.RecallInput.Scope /
// service.BriefingOpts.Scope, parsed by the unexported service.parseScope).
// "project", "full", and "everywhere" pass through unchanged; "exact" and
// "subtree" are deprecated back-compat aliases: "exact" maps to "project"
// (its original, pre-cascade meaning — the request namespace only, no
// ancestor/home/link cascade) and "subtree" maps to "everywhere" (the
// cascade plus the request namespace's subtree — every scope now inherits
// the cascade legs that didn't exist when "subtree" was coined). MCP still
// speaks exact/subtree literally; its enum swap to
// project/full/everywhere is T8's job, not this REST-only mapping.
func restScopeAlias(scope string) string {
	switch scope {
	case "exact":
		return scopeProject
	case "subtree":
		return "everywhere"
	default:
		return scope
	}
}

// invalidScopeMsg formats the 400 body for an unrecognized scope value,
// shared by SearchMemories and GetBriefing.
func invalidScopeMsg(scope string) string {
	return fmt.Sprintf("invalid scope %q: want project, full, everywhere, exact, or subtree", scope)
}

// scopeProject is the REST-layer mirror of service.scopeProject (unexported
// there); duplicated as a literal here rather than importing an unexported
// identifier — see restScopeAlias.
const scopeProject = "project"

// GetReadSet implements GET /v1/namespaces/read-set. Header-scoped like
// GetBriefing: the namespace comes from X-Memini-Namespace, and
// X-Memini-Home, when set, contributes the home leg.
func (h *Server) GetReadSet(w http.ResponseWriter, r *http.Request, _ GetReadSetParams) {
	name := namespaceFromContext(r.Context())
	if err := httputil.ValidateNamespace(name); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid namespace: "+err.Error())
		return
	}
	entries, err := h.svc.ResolveReadSetInfo(r.Context(), name, homeFromContext(r.Context()))
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	out := ReadSetResponse{Entries: make([]ReadSetEntryItem, len(entries))}
	for i, e := range entries {
		item := ReadSetEntryItem{Namespace: e.NS, Origin: ReadSetOrigin(e.Origin)}
		if len(e.Tiers) > 0 {
			tiers := make([]Tier, len(e.Tiers))
			for j, t := range e.Tiers {
				tiers[j] = Tier(t)
			}
			item.Tiers = &tiers
		}
		out.Entries[i] = item
	}
	httputil.JSON(w, http.StatusOK, out)
}

// linkStore type-asserts the backing store to store.LinkStore, the optional
// capability interface namespace links require (sqlitevec and postgres both
// implement it; see store.LinkStore's doc comment). Returns false — a 501 to
// the caller — for a driver that predates it, mirroring resolveReadSet's
// graceful degrade.
func (h *Server) linkStore() (store.LinkStore, bool) {
	ls, ok := h.svc.Store().(store.LinkStore)
	return ls, ok
}

// PutLink implements POST /v1/links. Creates or replaces a durable-tier read
// link from the request namespace (src) to the given destination.
func (h *Server) PutLink(w http.ResponseWriter, r *http.Request, _ PutLinkParams) {
	ls, ok := h.linkStore()
	if !ok {
		httputil.Error(w, http.StatusNotImplemented, "namespace links are not supported by this storage backend")
		return
	}
	src := namespaceFromContext(r.Context())
	if err := httputil.ValidateNamespace(src); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid namespace: "+err.Error())
		return
	}
	var req PutLinkJSONBody
	if !decode(w, r, &req) {
		return
	}
	dst := httputil.NormalizeNamespace(req.Dst)
	if err := httputil.ValidateNamespace(dst); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid dst namespace: "+err.Error())
		return
	}
	if strings.Contains(dst, "*") {
		httputil.Error(w, http.StatusBadRequest, "invalid dst namespace: \"*\" is reserved for read-set patterns")
		return
	}
	if dst == src {
		httputil.Error(w, http.StatusBadRequest, "dst namespace equals the request namespace (no self-links)")
		return
	}
	tiers, err := domainTiers(req.Tiers)
	if err != nil {
		httputil.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	// Both drivers set created_at = this value on every PUT (insert or
	// replace), so stamping it here — rather than leaving it zero for the
	// driver to fill in on first insert only — keeps the echoed response
	// accurate without a second round-trip to re-read the stored row.
	link := store.NamespaceLink{Src: src, Dst: dst, Tiers: tiers, Note: deref(req.Note), CreatedAt: time.Now().UTC()}
	if err := ls.PutLink(r.Context(), link); err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	httputil.JSON(w, http.StatusOK, apiNamespaceLink(link))
}

// ListLinks implements GET /v1/links: outgoing links from the request namespace.
func (h *Server) ListLinks(w http.ResponseWriter, r *http.Request, _ ListLinksParams) {
	ls, ok := h.linkStore()
	if !ok {
		httputil.Error(w, http.StatusNotImplemented, "namespace links are not supported by this storage backend")
		return
	}
	src := namespaceFromContext(r.Context())
	links, err := ls.ListLinks(r.Context(), src)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	out := NamespaceLinksResponse{Links: make([]NamespaceLink, len(links))}
	for i, l := range links {
		out.Links[i] = apiNamespaceLink(l)
	}
	httputil.JSON(w, http.StatusOK, out)
}

// DeleteLink implements DELETE /v1/links. dst comes from the query parameter
// or, when absent, the optional JSON body.
func (h *Server) DeleteLink(w http.ResponseWriter, r *http.Request, params DeleteLinkParams) {
	ls, ok := h.linkStore()
	if !ok {
		httputil.Error(w, http.StatusNotImplemented, "namespace links are not supported by this storage backend")
		return
	}
	dst := deref(params.Dst)
	if dst == "" && r.ContentLength != 0 {
		var body DeleteLinkJSONBody
		if !decode(w, r, &body) {
			return
		}
		dst = deref(body.Dst)
	}
	dst = httputil.NormalizeNamespace(dst)
	if dst == "" {
		httputil.Error(w, http.StatusBadRequest, "dst namespace is required (query or body)")
		return
	}
	src := namespaceFromContext(r.Context())
	found, err := ls.DeleteLink(r.Context(), src, dst)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	if !found {
		httputil.Error(w, http.StatusNotFound, "link not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiNamespaceLink maps a store.NamespaceLink onto the spec model.
func apiNamespaceLink(l store.NamespaceLink) NamespaceLink {
	out := NamespaceLink{Src: l.Src, Dst: l.Dst, CreatedAt: l.CreatedAt}
	if l.Note != "" {
		out.Note = &l.Note
	}
	if len(l.Tiers) > 0 {
		tiers := make([]Tier, len(l.Tiers))
		for i, t := range l.Tiers {
			tiers[i] = Tier(t)
		}
		out.Tiers = &tiers
	}
	return out
}

// GetBriefing implements GET /v1/namespaces/briefing. The namespace comes from
// the X-Memini-Namespace header (via the middleware), not the URL path.
func (h *Server) GetBriefing(w http.ResponseWriter, r *http.Request, params GetBriefingParams) {
	name := namespaceFromContext(r.Context())
	if err := httputil.ValidateNamespace(name); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid namespace: "+err.Error())
		return
	}
	// Per-section caps: a dedicated param (per_section_*) wins over the
	// uniform per_section fallback so callers can pin a small durable
	// "top-of-mind" set without unbalancing the rest. We pass pointers
	// through (rather than deref'ing) so nil-vs-zero carries "unset" vs
	// "explicitly disable this section" to the service layer.
	pick := func(dedicated, fallback *int) *int {
		if dedicated != nil {
			return dedicated
		}
		return fallback
	}
	opts := service.BriefingOpts{
		Pinned:     pick(params.PerSectionPinned, params.PerSection),
		Facts:      pick(params.PerSectionFacts, params.PerSection),
		Procedures: pick(params.PerSectionProcedures, params.PerSection),
		Recent:     pick(params.PerSectionRecent, params.PerSection),
		Home:       homeFromContext(r.Context()),
		// Explicit namespaces REPLACE the default read set; the service layer
		// validates entries and enforces the 16-entry cap (ErrInvalidInput → 400).
		Namespaces: deref(params.Namespaces),
	}
	// The generated binding does not enforce the spec enum, so an unknown
	// scope must be rejected here rather than silently briefed as exact.
	if params.Scope != nil {
		if !params.Scope.Valid() {
			httputil.Error(w, http.StatusBadRequest, invalidScopeMsg(string(*params.Scope)))
			return
		}
		opts.Scope = restScopeAlias(string(*params.Scope))
	}
	var readset []service.ReadSetEntry
	opts.ReadSet = &readset
	b, err := h.svc.Briefing(r.Context(), name, opts)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	origins := service.OriginMap(readset)
	resp := Briefing{
		Namespace:  b.Namespace,
		Facts:      apiBriefingItems(b.Facts, origins),
		Procedures: apiBriefingItems(b.Procedures, origins),
		Recent:     apiBriefingItems(b.Recent, origins),
		Pinned:     apiBriefingItems(b.Pinned, origins),
		Children:   apiBriefingChildren(b.Children),
	}
	if b.ScopeHeader != "" {
		resp.ScopeHeader = &b.ScopeHeader
	}
	// b.ChildrenTruncated has no field in the T6 wire shape; REST returns
	// just the capped children array (MCP surfaces the count as a note).
	httputil.JSON(w, http.StatusOK, resp)
}

// apiBriefingChildren maps the service's direct-child rollup to the
// spec-generated BriefingChild shape — full memory objects (unlike MCP's
// title-only rendering), since the admin UI consumes them. nil for an empty
// rollup so the field is omitted at a leaf namespace.
func apiBriefingChildren(children []service.ChildSummary) *[]BriefingChild {
	if len(children) == 0 {
		return nil
	}
	out := make([]BriefingChild, len(children))
	for i, c := range children {
		out[i] = BriefingChild{
			Namespace: c.NS,
			Total:     c.Total,
			Pinned:    apiMemories(c.Pinned),
			Recent:    apiMemories(c.Recent),
		}
	}
	return &out
}

// apiMemories maps a memory slice to the generated wire shape, nil-for-empty
// so optional fields are omitted.
func apiMemories(mems []*memory.Memory) *[]Memory {
	if len(mems) == 0 {
		return nil
	}
	out := make([]Memory, len(mems))
	for i, m := range mems {
		out[i] = apiMemory(m)
	}
	return &out
}

// apiBriefingItems maps a briefing section's memories to the spec-generated
// BriefingItem shape (memory + read-set provenance), returning nil for an
// empty slice so the field is omitted from the response. origins is built
// once per request via service.OriginMap from the Briefing call's ReadSet
// out-param; see service.ReadSetFrom for the provenance rendering rules.
func apiBriefingItems(mems []*memory.Memory, origins map[string]string) *[]BriefingItem {
	if len(mems) == 0 {
		return nil
	}
	out := make([]BriefingItem, len(mems))
	for i, m := range mems {
		item := BriefingItem{Memory: apiMemory(m)}
		if from := service.ReadSetFrom(origins, m.Namespace); from != "" {
			item.From = &from
		}
		out[i] = item
	}
	return &out
}

// apiRenamespaceReport converts a maintenance.RenamespaceReport into the
// spec-generated RenamespaceReport, only setting fields that are non-zero.
func apiRenamespaceReport(r maintenance.RenamespaceReport) RenamespaceReport {
	out := RenamespaceReport{}
	if r.Moved > 0 {
		out.Moved = &r.Moved
	}
	if len(r.Targets) > 0 {
		out.Targets = &r.Targets
	}
	if r.Skipped > 0 {
		out.Skipped = &r.Skipped
	}
	if r.DryRun {
		out.DryRun = &r.DryRun
	}
	return out
}

// GetStats implements GET /v1/stats.
func (h *Server) GetStats(w http.ResponseWriter, r *http.Request, params GetStatsParams) {
	var s service.Stats
	var err error
	if params.AllNamespaces != nil && *params.AllNamespaces {
		s, err = h.svc.StatsAll(r.Context())
	} else {
		s, err = h.svc.Stats(r.Context(), namespaceFromContext(r.Context()))
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	byTier := make(map[string]int, len(s.ByTier))
	for tier, n := range s.ByTier {
		byTier[string(tier)] = n
	}
	resp := Stats{
		Namespace:            s.Namespace,
		Total:                s.Total,
		ByTier:               byTier,
		Expired:              s.Expired,
		Superseded:           s.Superseded,
		LowConfidenceDurable: s.LowConfidenceDurable,
		TotalAccesses:        s.TotalAccesses,
		AvgImportance:        s.AvgImportance,
		LastWriteAt:          s.LastWriteAt,
	}
	if len(s.ByMemoryType) > 0 {
		resp.ByMemoryType = &s.ByMemoryType
	}
	httputil.JSON(w, http.StatusOK, resp)
}

// ListNamespaces implements GET /v1/namespaces.
//
// Unlike the other /v1 routes it is not namespace-scoped: it deliberately spans
// tenants. memini authenticates with a single MEMINI_API_KEY that already
// grants access to any namespace (the caller picks it via the namespace
// header), so enumerating namespaces confers no extra privilege. If memini ever
// grows per-tenant credentials, this endpoint must be gated behind an admin
// scope.
func (h *Server) ListNamespaces(w http.ResponseWriter, r *http.Request) {
	ns, err := h.svc.Namespaces(r.Context())
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if ns == nil {
		ns = []string{} // marshal as [] rather than null on an empty store
	}
	httputil.JSON(w, http.StatusOK, NamespacesResponse{Namespaces: ns})
}

// DeleteNamespace implements DELETE /v1/namespaces. The namespace comes from
// the X-Memini-Namespace header (via the middleware), not the URL path, so a
// hierarchical name like "work/memini" needs no %2F path encoding.
func (h *Server) DeleteNamespace(w http.ResponseWriter, r *http.Request, _ DeleteNamespaceParams) {
	name := namespaceFromContext(r.Context())
	if err := httputil.ValidateNamespace(name); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid namespace: "+err.Error())
		return
	}
	n, err := h.svc.DeleteNamespace(r.Context(), name)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	httputil.JSON(w, http.StatusOK, DeleteNamespaceResponse{Deleted: int(n)})
}

// SplitNamespace implements POST /v1/namespaces/split. Regroups the request
// namespace (X-Memini-Namespace header) by metadata keys, moving each record to
// the namespace named by the first of the given keys it carries.
func (h *Server) SplitNamespace(w http.ResponseWriter, r *http.Request, _ SplitNamespaceParams) {
	name := namespaceFromContext(r.Context())
	if err := httputil.ValidateNamespace(name); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid namespace: "+err.Error())
		return
	}
	var req SplitNamespaceJSONBody
	if !decode(w, r, &req) {
		return
	}
	var by []string
	if req.By != nil {
		by = *req.By
	}
	// Undocumented ?by=a,b fallback kept for callers that predate the body.
	if len(by) == 0 {
		for _, p := range strings.Split(r.URL.Query().Get("by"), ",") {
			if p = strings.TrimSpace(p); p != "" {
				by = append(by, p)
			}
		}
	}
	if len(by) == 0 {
		by = maintenance.DefaultSplitKeys
	}
	dryRun := req.DryRun != nil && *req.DryRun
	rep, err := maintenance.Split(r.Context(), h.svc.Store(), name, by, dryRun)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	httputil.JSON(w, http.StatusOK, apiRenamespaceReport(rep))
}

// MoveNamespace implements POST /v1/namespaces/move. Relocates every memory in
// the request namespace (X-Memini-Namespace header) to the target namespace.
func (h *Server) MoveNamespace(w http.ResponseWriter, r *http.Request, _ MoveNamespaceParams) {
	name := namespaceFromContext(r.Context())
	if err := httputil.ValidateNamespace(name); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid namespace: "+err.Error())
		return
	}
	var req MoveNamespaceJSONBody
	if !decode(w, r, &req) {
		return
	}
	// Normalize before validating: move CREATES the target namespace, so a
	// stray "work/" or " x" would mint a namespace unreachable through the
	// namespace header, and "*" would masquerade as a read-set pattern.
	to := httputil.NormalizeNamespace(req.To)
	if err := httputil.ValidateNamespace(to); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid target namespace: "+err.Error())
		return
	}
	if strings.Contains(to, "*") {
		httputil.Error(w, http.StatusBadRequest, "invalid target namespace: \"*\" is reserved for read-set patterns")
		return
	}
	if to == name {
		httputil.Error(w, http.StatusBadRequest, "target namespace equals the source namespace")
		return
	}
	dryRun := req.DryRun != nil && *req.DryRun
	rep, err := maintenance.Move(r.Context(), h.svc.Store(), name, to, dryRun)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	httputil.JSON(w, http.StatusOK, apiRenamespaceReport(rep))
}

// ReassignMemory implements POST /v1/memories/{id}/reassign. Moves a single
// memory from the request namespace to the target namespace.
func (h *Server) ReassignMemory(w http.ResponseWriter, r *http.Request, id string, _ ReassignMemoryParams) {
	boundID, ok := unescapeID(id)
	if !ok {
		httputil.Error(w, http.StatusBadRequest, "invalid memory ID encoding")
		return
	}
	var req ReassignMemoryJSONBody
	if !decode(w, r, &req) {
		return
	}
	// Normalize before validating: reassign CREATES the target namespace, so a
	// stray "work/" or " x" would mint a namespace unreachable through the
	// namespace header, and "*" would masquerade as a read-set subtree pattern.
	to := httputil.NormalizeNamespace(req.To)
	if err := httputil.ValidateNamespace(to); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid target namespace: "+err.Error())
		return
	}
	if strings.Contains(to, "*") {
		httputil.Error(w, http.StatusBadRequest, "invalid target namespace: \"*\" is reserved for read-set patterns")
		return
	}
	ns := namespaceFromContext(r.Context())
	if to == ns {
		httputil.Error(w, http.StatusBadRequest, "target namespace equals the request namespace")
		return
	}
	n, err := h.svc.Store().Reassign(r.Context(), ns, []string{boundID}, to)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	// Both stores skip IDs absent from the request namespace rather than
	// erroring, so zero rows moved is the only "memory not found" signal.
	if n == 0 {
		httputil.Error(w, http.StatusNotFound, "memory not found in the request namespace")
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]int{"moved": int(n)})
}

// apiMemory maps the domain memory onto the spec model. Optional fields are
// emitted only when set, preserving the wire format clients already rely on.
func apiMemory(m *memory.Memory) Memory {
	out := Memory{
		Id:             m.ID,
		Namespace:      m.Namespace,
		Tier:           Tier(m.Tier),
		Content:        m.Content,
		Importance:     m.Importance,
		CreatedAt:      m.CreatedAt,
		UpdatedAt:      m.UpdatedAt,
		LastAccessedAt: m.LastAccessedAt,
		AccessCount:    m.AccessCount,
		ExpiresAt:      m.ExpiresAt,
		SupersededBy:   m.SupersededBy,
		ValidFrom:      m.ValidFrom,
		ValidTo:        m.ValidTo,
		Confidence:     m.Confidence,
	}
	if m.Level != "" {
		l := Level(m.Level)
		out.Level = &l
	}
	if m.Summary != "" {
		out.Summary = &m.Summary
	}
	if len(m.Tags) > 0 {
		out.Tags = &m.Tags
	}
	if len(m.Metadata) > 0 {
		out.Metadata = &m.Metadata
	}
	return out
}

// apiScored maps recall/answer hits to the spec-generated ScoredMemory shape,
// including read-set provenance ("from"). origins is built once per request
// via service.OriginMap from the call's ReadSet out-param; see
// service.ReadSetFrom for the provenance rendering rules. A nil/empty origins
// map (the caller didn't wire a ReadSet out-param) renders every item's from
// as "".
func apiScored(res []store.Scored, origins map[string]string) []ScoredMemory {
	out := make([]ScoredMemory, len(res))
	for i, s := range res {
		sm := ScoredMemory{Memory: apiMemory(s.Memory), Score: s.Score}
		if from := service.ReadSetFrom(origins, s.Memory.Namespace); from != "" {
			sm.From = &from
		}
		out[i] = sm
	}
	return out
}

// deref returns *p, or the zero value of T when p is nil. It collapses the
// "copy the optional request field only when present" pattern: an absent value
// and an explicit zero leave the domain input struct identical either way.
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// domainTiers validates a body tier filter. An unknown tier is an error rather
// than silently unfiltered results.
func domainTiers(in *[]Tier) ([]memory.Tier, error) {
	if in == nil {
		return nil, nil
	}
	tiers := make([]memory.Tier, 0, len(*in))
	for _, t := range *in {
		mt := memory.Tier(t)
		if !mt.Valid() {
			return nil, fmt.Errorf("invalid tier %q", t)
		}
		tiers = append(tiers, mt)
	}
	return tiers, nil
}

// domainLevels validates a body level filter. An unknown level is an error
// rather than silently unfiltered results.
func domainLevels(in *[]Level) ([]memory.Level, error) {
	if in == nil {
		return nil, nil
	}
	levels := make([]memory.Level, 0, len(*in))
	for _, l := range *in {
		ml := memory.Level(l)
		if !ml.Valid() {
			return nil, fmt.Errorf("invalid level %q", l)
		}
		levels = append(levels, ml)
	}
	return levels, nil
}

// queryTiers expands and validates the ?tier= filter. The parameter is
// documented as repeatable and/or comma-separated; the generated binding only
// splits repeats, so comma-separated values are expanded here.
func queryTiers(in *[]Tier) ([]memory.Tier, error) {
	if in == nil {
		return nil, nil
	}
	var tiers []memory.Tier
	for _, v := range *in {
		for part := range strings.SplitSeq(string(v), ",") {
			t := memory.Tier(strings.TrimSpace(part))
			if !t.Valid() {
				return nil, fmt.Errorf("invalid tier %q", t)
			}
			tiers = append(tiers, t)
		}
	}
	return tiers, nil
}

// queryLevels expands and validates the ?level= filter. Like ?tier=, it supports
// repeatable and/or comma-separated values.
func queryLevels(in *[]Level) ([]memory.Level, error) {
	if in == nil {
		return nil, nil
	}
	var levels []memory.Level
	for _, v := range *in {
		for part := range strings.SplitSeq(string(v), ",") {
			l := memory.Level(strings.TrimSpace(part))
			if !l.Valid() {
				return nil, fmt.Errorf("invalid level %q", l)
			}
			levels = append(levels, l)
		}
	}
	return levels, nil
}

// queryTags expands the ?tag= filter, splitting comma-separated values like
// queryTiers does (the generated binding only splits repeats). Blank entries
// are dropped; nil/empty yields no tag constraint.
func queryTags(in *[]string) []string {
	if in == nil {
		return nil
	}
	var tags []string
	for _, v := range *in {
		for part := range strings.SplitSeq(v, ",") {
			if t := strings.TrimSpace(part); t != "" {
				tags = append(tags, t)
			}
		}
	}
	return tags
}

// parseMetaFilters parses the ?meta=key=value filter into a map. Each entry
// splits on the first '=', so values may themselves contain '='. A missing '='
// or empty key is a client error.
func parseMetaFilters(in *[]string) (map[string]string, error) {
	if in == nil || len(*in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(*in))
	for _, v := range *in {
		k, val, ok := strings.Cut(v, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid meta filter %q: want key=value", v)
		}
		out[k] = val
	}
	return out, nil
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
