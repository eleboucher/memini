package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/eleboucher/memini/internal/httputil"
)

// requestLogger emits one structured log line per request at completion.
// Health and metrics probes are logged at debug to avoid noise, and so is
// /v1/stats: the admin UI polls it once per namespace per refresh, which at
// info level buried everything meaningful under bursts of identical lines.
//
// The line carries the authenticated actor when one was resolved: this
// middleware is the OUTERMOST wrapper, so it installs an actor holder
// (httputil.WithActorHolder) before dispatch, the auth middlewares deeper in
// (REST's actorMiddleware, MCP's bearer wrapper) record into it, and the
// attrs are read back here after the handler returns — "key" names the key,
// "auth" is the key/env/none attribution kind. Requests that never reach an
// authenticating middleware (probes, 404s, the UI shell) record nothing and
// log exactly as before.
func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			r = r.WithContext(httputil.WithActorHolder(r.Context()))
			next.ServeHTTP(ww, r)

			level := slog.LevelInfo
			switch r.URL.Path {
			case "/healthz", "/readyz", "/metrics", "/v1/stats":
				level = slog.LevelDebug
			}

			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", middleware.GetReqID(r.Context())),
			}
			if name, kind, ok := httputil.RecordedActor(r.Context()); ok {
				if name != "" {
					attrs = append(attrs, slog.String("key", name))
				}
				attrs = append(attrs, slog.String("auth", kind))
			}
			log.LogAttrs(r.Context(), level, "http request", attrs...)
		})
	}
}
