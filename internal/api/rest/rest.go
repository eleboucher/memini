// Package rest exposes memini's HTTP/JSON API. It is a thin adapter over the
// service layer; the MCP surface (internal/api/mcp) shares the same service.
package rest

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// Handler serves the REST API backed by a service.Service.
type Handler struct {
	svc  *service.Service
	auth AuthConfig
}

// New builds a REST handler.
func New(svc *service.Service, auth AuthConfig) *Handler {
	return &Handler{svc: svc, auth: auth}
}

// Mount attaches the /v1 routes to r, wrapped in namespace + auth middleware.
func (h *Handler) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.auth.namespaceMiddleware)
		r.Use(h.auth.authMiddleware)

		r.Post("/v1/memories", h.remember)
		r.Get("/v1/memories", h.list)
		r.Get("/v1/memories/{id}", h.get)
		r.Delete("/v1/memories/{id}", h.forget)
		r.Post("/v1/search", h.search)
		r.Post("/v1/answer", h.answer)
		r.Post("/v1/fsck", h.fsck)
		r.Get("/v1/stats", h.stats)
		r.Get("/v1/namespaces", h.namespaces)
	})
}

func (h *Handler) fsck(w http.ResponseWriter, r *http.Request) {
	report, err := h.svc.Fsck(r.Context())
	if err != nil {
		httputil.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.JSON(w, http.StatusOK, report)
}

type rememberRequest struct {
	Content    string         `json:"content"`
	Tier       memory.Tier    `json:"tier,omitempty"`
	Summary    string         `json:"summary,omitempty"`
	Tags       []string       `json:"tags,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Importance float64        `json:"importance,omitempty"`
	TTLSeconds *int           `json:"ttl_seconds,omitempty"` // negative = never expire
	ID         string         `json:"id,omitempty"`
}

func (h *Handler) remember(w http.ResponseWriter, r *http.Request) {
	var req rememberRequest
	if !decode(w, r, &req) {
		return
	}
	in := service.RememberInput{
		Namespace:  namespaceFromContext(r.Context()),
		Content:    req.Content,
		Tier:       req.Tier,
		Summary:    req.Summary,
		Tags:       req.Tags,
		Metadata:   req.Metadata,
		Importance: req.Importance,
		ID:         req.ID,
	}
	if req.TTLSeconds != nil {
		d := time.Duration(*req.TTLSeconds) * time.Second
		in.TTL = &d
	}

	m, err := h.svc.Remember(r.Context(), in)
	if err != nil {
		httputil.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	httputil.JSON(w, http.StatusCreated, m)
}

// pathID returns the {id} path param, percent-decoded. chi matches on the
// escaped path, so URLParam yields the raw segment; ids with reserved chars
// like ':' (e.g. imported "openclaw:main:<uuid>") arrive as %3A and must be
// decoded to match the stored literal. A malformed encoding yields ok=false.
func pathID(r *http.Request) (string, bool) {
	id, err := url.PathUnescape(chi.URLParam(r, "id"))
	return id, err == nil
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
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
	httputil.JSON(w, http.StatusOK, m)
}

func (h *Handler) forget(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
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

type searchRequest struct {
	Query             string        `json:"query"`
	Tiers             []memory.Tier `json:"tiers,omitempty"`
	Limit             int           `json:"limit,omitempty"`
	IncludeExpired    bool          `json:"include_expired,omitempty"`
	IncludeSuperseded bool          `json:"include_superseded,omitempty"`
}

type scoredDTO struct {
	Memory *memory.Memory `json:"memory"`
	Score  float64        `json:"score"`
}

type searchResponse struct {
	Results []scoredDTO `json:"results"`
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	var req searchRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := h.svc.Recall(r.Context(), service.RecallInput{
		Namespace:         namespaceFromContext(r.Context()),
		Query:             req.Query,
		Tiers:             req.Tiers,
		Limit:             req.Limit,
		IncludeExpired:    req.IncludeExpired,
		IncludeSuperseded: req.IncludeSuperseded,
	})
	if err != nil {
		httputil.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	out := searchResponse{Results: make([]scoredDTO, len(res))}
	for i, s := range res {
		out.Results[i] = scoredDTO{Memory: s.Memory, Score: s.Score}
	}
	httputil.JSON(w, http.StatusOK, out)
}

type answerRequest struct {
	Query string        `json:"query"`
	Tiers []memory.Tier `json:"tiers,omitempty"`
	Limit int           `json:"limit,omitempty"`
}

type answerResponse struct {
	Answer  string      `json:"answer"`
	Sources []scoredDTO `json:"sources"`
}

func (h *Handler) answer(w http.ResponseWriter, r *http.Request) {
	var req answerRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := h.svc.Answer(r.Context(), service.AnswerInput{
		Namespace: namespaceFromContext(r.Context()),
		Query:     req.Query,
		Tiers:     req.Tiers,
		Limit:     req.Limit,
	})
	if err != nil {
		httputil.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	out := answerResponse{Answer: res.Answer, Sources: make([]scoredDTO, len(res.Sources))}
	for i, s := range res.Sources {
		out.Sources[i] = scoredDTO{Memory: s.Memory, Score: s.Score}
	}
	httputil.JSON(w, http.StatusOK, out)
}

type listResponse struct {
	Memories []*memory.Memory `json:"memories"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	in := service.ListInput{
		Namespace:         namespaceFromContext(r.Context()),
		Tiers:             parseTiers(q),
		IncludeExpired:    q.Get("include_expired") == "true",
		IncludeSuperseded: q.Get("include_superseded") == "true",
		Limit:             parseLimit(q.Get("limit")),
	}
	mems, err := h.svc.List(r.Context(), in)
	if err != nil {
		httputil.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	httputil.JSON(w, http.StatusOK, listResponse{Memories: mems})
}

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	s, err := h.svc.Stats(r.Context(), namespaceFromContext(r.Context()))
	if err != nil {
		httputil.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.JSON(w, http.StatusOK, s)
}

type namespacesResponse struct {
	Namespaces []string `json:"namespaces"`
}

// namespaces lists every tenant in the store, for the UI's namespace switcher.
// Unlike the other /v1 routes it is not namespace-scoped: it deliberately spans
// tenants. memini authenticates with a single MEMINI_API_KEY that already
// grants access to any namespace (the caller picks it via the namespace
// header), so enumerating namespaces confers no extra privilege. If memini ever
// grows per-tenant credentials, this endpoint must be gated behind an admin
// scope.
func (h *Handler) namespaces(w http.ResponseWriter, r *http.Request) {
	ns, err := h.svc.Namespaces(r.Context())
	if err != nil {
		httputil.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.JSON(w, http.StatusOK, namespacesResponse{Namespaces: ns})
}

// parseTiers reads repeated and/or comma-separated ?tier= values into a tier
// slice, silently dropping unknown tiers.
func parseTiers(q url.Values) []memory.Tier {
	var tiers []memory.Tier
	for _, v := range q["tier"] {
		for part := range strings.SplitSeq(v, ",") {
			t := memory.Tier(strings.TrimSpace(part))
			if t.Valid() {
				tiers = append(tiers, t)
			}
		}
	}
	return tiers
}

// parseLimit parses a non-negative ?limit=; invalid or absent yields 0 (all).
func parseLimit(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
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
