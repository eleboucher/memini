package server

import (
	"net/http"

	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/version"
)

const keyStatus = "status"

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	httputil.JSON(w, http.StatusOK, map[string]string{
		keyStatus: "ok",
		"version": version.Version,
	})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if fn := s.ready.Load(); fn != nil {
		if err := (*fn)(r.Context()); err != nil {
			httputil.JSON(w, http.StatusServiceUnavailable, map[string]string{
				keyStatus: "not ready",
				"error":   err.Error(),
			})
			return
		}
	}
	httputil.JSON(w, http.StatusOK, map[string]string{keyStatus: "ready"})
}
