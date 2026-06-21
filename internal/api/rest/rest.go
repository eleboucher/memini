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
// are rejected with 400 by the generated wrappers.
func (h *Server) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		// Authentication first: an unauthenticated caller gets 401, never a
		// 400 that leaks namespace-validation behavior.
		r.Use(h.auth.authMiddleware)
		r.Use(h.auth.namespaceMiddleware)
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
		Content:   req.Content,
	}
	in.Tier = memory.Tier(deref(req.Tier))
	in.Summary = deref(req.Summary)
	in.Tags = deref(req.Tags)
	in.Metadata = deref(req.Metadata)
	in.Importance = deref(req.Importance)
	in.ID = deref(req.Id)
	in.Confidence = req.Confidence
	in.ValidFrom = req.ValidFrom
	in.ValidTo = req.ValidTo
	if req.TtlSeconds != nil {
		d := time.Duration(*req.TtlSeconds) * time.Second
		in.TTL = &d
	}

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
	httputil.JSON(w, http.StatusCreated, apiMemory(m))
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
	in := service.RecallInput{
		Namespace: namespaceFromContext(r.Context()),
		Query:     req.Query,
		Tiers:     tiers,
	}
	in.Tags = deref(req.Tags)
	in.Metadata = deref(req.Metadata)
	in.ExcludeMetadata = deref(req.ExcludeMetadata)
	in.Limit = deref(req.Limit)
	in.IncludeExpired = deref(req.IncludeExpired)
	in.IncludeSuperseded = deref(req.IncludeSuperseded)
	if req.AsOf != nil {
		in.AsOf = req.AsOf.UTC()
	}
	if req.Scope != nil && *req.Scope == Subtree {
		in.Subtree = true
	}
	if req.MinScore != nil {
		in.MinScore = *req.MinScore
	}

	res, err := h.svc.Recall(r.Context(), in)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	httputil.JSON(w, http.StatusOK, SearchResponse{Results: apiScored(res)})
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
	in := service.AnswerInput{
		Namespace: namespaceFromContext(r.Context()),
		Query:     req.Query,
		Tiers:     tiers,
	}
	in.Tags = deref(req.Tags)
	in.Metadata = deref(req.Metadata)
	in.Limit = deref(req.Limit)

	res, err := h.svc.Answer(r.Context(), in)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	httputil.JSON(w, http.StatusOK, AnswerResponse{Answer: res.Answer, Sources: apiScored(res.Sources)})
}

// ListMemories implements GET /v1/memories.
func (h *Server) ListMemories(w http.ResponseWriter, r *http.Request, params ListMemoriesParams) {
	tiers, err := queryTiers(params.Tier)
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

// GetBriefing implements GET /v1/namespaces/{name}/briefing.
func (h *Server) GetBriefing(w http.ResponseWriter, r *http.Request, name string, params GetBriefingParams) {
	// chi binds the raw escaped path segment, so a nested namespace ("project/
	// agent") arrives as "project%2Fagent" and must be decoded to match storage.
	decoded, ok := unescapeID(name)
	if !ok {
		httputil.Error(w, http.StatusBadRequest, "invalid namespace encoding")
		return
	}
	name = decoded
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
	}
	b, err := h.svc.Briefing(r.Context(), name, opts)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	httputil.JSON(w, http.StatusOK, Briefing{
		Namespace:  b.Namespace,
		Facts:      apiMemoryList(b.Facts),
		Procedures: apiMemoryList(b.Procedures),
		Recent:     apiMemoryList(b.Recent),
		Pinned:     apiMemoryList(b.Pinned),
	})
}

// apiMemoryList maps a slice of memories to API models, returning nil for an
// empty slice so the field is omitted from the response.
func apiMemoryList(mems []*memory.Memory) *[]Memory {
	if len(mems) == 0 {
		return nil
	}
	out := make([]Memory, len(mems))
	for i, m := range mems {
		out[i] = apiMemory(m)
	}
	return &out
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

// DeleteNamespace implements DELETE /v1/namespaces/{name}.
func (h *Server) DeleteNamespace(w http.ResponseWriter, r *http.Request, name string) {
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

func apiScored(res []store.Scored) []ScoredMemory {
	out := make([]ScoredMemory, len(res))
	for i, s := range res {
		out[i] = ScoredMemory{Memory: apiMemory(s.Memory), Score: s.Score}
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
