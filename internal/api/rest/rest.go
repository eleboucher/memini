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
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

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

// RunFsck implements POST /v1/fsck.
func (h *Server) RunFsck(w http.ResponseWriter, r *http.Request, _ RunFsckParams) {
	report, err := h.svc.Fsck(r.Context())
	if err != nil {
		httputil.Error(w, http.StatusInternalServerError, err.Error())
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
	if req.Tier != nil {
		in.Tier = memory.Tier(*req.Tier)
	}
	if req.Summary != nil {
		in.Summary = *req.Summary
	}
	if req.Tags != nil {
		in.Tags = *req.Tags
	}
	if req.Metadata != nil {
		in.Metadata = *req.Metadata
	}
	if req.Importance != nil {
		in.Importance = *req.Importance
	}
	if req.Id != nil {
		in.ID = *req.Id
	}
	if req.TtlSeconds != nil {
		d := time.Duration(*req.TtlSeconds) * time.Second
		in.TTL = &d
	}

	m, err := h.svc.Remember(r.Context(), in)
	if err != nil {
		httputil.Error(w, statusFor(err), err.Error())
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
		httputil.Error(w, http.StatusInternalServerError, err.Error())
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
		httputil.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	if req.Limit != nil {
		in.Limit = *req.Limit
	}
	if req.IncludeExpired != nil {
		in.IncludeExpired = *req.IncludeExpired
	}
	if req.IncludeSuperseded != nil {
		in.IncludeSuperseded = *req.IncludeSuperseded
	}

	res, err := h.svc.Recall(r.Context(), in)
	if err != nil {
		httputil.Error(w, statusFor(err), err.Error())
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
	if req.Limit != nil {
		in.Limit = *req.Limit
	}

	res, err := h.svc.Answer(r.Context(), in)
	if err != nil {
		httputil.Error(w, statusFor(err), err.Error())
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
	in := service.ListInput{
		Namespace: namespaceFromContext(r.Context()),
		Tiers:     tiers,
	}
	if params.IncludeExpired != nil {
		in.IncludeExpired = *params.IncludeExpired
	}
	if params.IncludeSuperseded != nil {
		in.IncludeSuperseded = *params.IncludeSuperseded
	}
	if params.Limit != nil {
		if *params.Limit < 0 {
			httputil.Error(w, http.StatusBadRequest, fmt.Sprintf("invalid limit %d", *params.Limit))
			return
		}
		in.Limit = *params.Limit
	}

	mems, err := h.svc.List(r.Context(), in)
	if err != nil {
		httputil.Error(w, statusFor(err), err.Error())
		return
	}
	out := ListResponse{Memories: make([]Memory, len(mems))}
	for i, m := range mems {
		out.Memories[i] = apiMemory(m)
	}
	httputil.JSON(w, http.StatusOK, out)
}

// GetStats implements GET /v1/stats.
func (h *Server) GetStats(w http.ResponseWriter, r *http.Request, _ GetStatsParams) {
	s, err := h.svc.Stats(r.Context(), namespaceFromContext(r.Context()))
	if err != nil {
		httputil.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	byTier := make(map[string]int, len(s.ByTier))
	for tier, n := range s.ByTier {
		byTier[string(tier)] = n
	}
	httputil.JSON(w, http.StatusOK, Stats{
		Namespace:     s.Namespace,
		Total:         s.Total,
		ByTier:        byTier,
		Expired:       s.Expired,
		Superseded:    s.Superseded,
		TotalAccesses: s.TotalAccesses,
		AvgImportance: s.AvgImportance,
		LastWriteAt:   s.LastWriteAt,
	})
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
		httputil.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ns == nil {
		ns = []string{} // marshal as [] rather than null on an empty store
	}
	httputil.JSON(w, http.StatusOK, NamespacesResponse{Namespaces: ns})
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

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
