// Package rest exposes memini's HTTP/JSON API. It is a thin adapter over the
// service layer; the MCP surface (internal/api/mcp) shares the same service.
package rest

import (
	"encoding/json"
	"errors"
	"net/http"
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
		r.Get("/v1/memories/{id}", h.get)
		r.Delete("/v1/memories/{id}", h.forget)
		r.Post("/v1/search", h.search)
		r.Post("/v1/fsck", h.fsck)
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

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	m, err := h.svc.Get(r.Context(), namespaceFromContext(r.Context()), chi.URLParam(r, "id"))
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
	err := h.svc.Forget(r.Context(), namespaceFromContext(r.Context()), chi.URLParam(r, "id"))
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

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httputil.Error(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
