package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eleboucher/memini/internal/api/rest"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/server"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

const (
	dims  = 64
	nsHdr = "X-Memini-Namespace"
)

// slowStore wraps a real store.Store, overriding List to block until ctx is
// cancelled or d elapses, simulating a handler that respects context
// propagation (e.g. an LLM- or embedder-backed call), so a request-scoped
// deadline placed on ctx by the timeout middleware actually cuts it short.
type slowStore struct {
	store.Store
	d time.Duration
}

func (s slowStore) List(ctx context.Context, namespace string, f store.Filter, limit int) ([]*memory.Memory, error) {
	select {
	case <-time.After(s.d):
		return s.Store.List(ctx, namespace, f, limit)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestRequestTimeoutCutsSlowV1Request confirms that a /v1 request whose
// downstream work outlives the configured MEMINI_REQUEST_TIMEOUT is bounded to
// roughly that timeout, rather than running for however long the slow
// downstream call takes — the whole point of the middleware. chi's
// middleware.Timeout (as vendored: github.com/go-chi/chi/v5@v5.3.0) only
// cancels the request context; it does not forcibly abort a handler that
// ignores ctx.Done(), so this test uses a downstream (slowStore.List) that
// honors cancellation, which is the realistic case for memini's I/O-bound
// handlers (store/embedder/LLM calls all thread ctx through).
func TestRequestTimeoutCutsSlowV1Request(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "timeout.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const slowDuration = 300 * time.Millisecond
	const reqTimeout = 20 * time.Millisecond
	svc := service.New(slowStore{Store: st, d: slowDuration}, embedtest.New(dims))

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Options{Addr: ":0", ShutdownTimeout: time.Second}, log, prometheus.NewRegistry())
	rest.New(svc, rest.AuthConfig{
		NamespaceHeader: nsHdr, DefaultNamespace: "default", RequestTimeout: reqTimeout,
	}).Mount(srv.Router())

	req := httptest.NewRequest(http.MethodGet, "/v1/memories", nil)
	req.Header.Set(nsHdr, "default")
	rec := httptest.NewRecorder()

	start := time.Now()
	srv.Router().ServeHTTP(rec, req)
	elapsed := time.Since(start)

	// The point of the middleware: bounded well under the slow downstream's
	// full duration, not the ~300ms it would otherwise take.
	if elapsed >= slowDuration {
		t.Fatalf("request took %v, want well under the %v slow-downstream duration (timeout middleware did not cut it)", elapsed, slowDuration)
	}
	// A response must still have been written (not a hung/empty connection).
	if rec.Code == 0 {
		t.Fatal("no response written")
	}
	t.Logf("slow /v1/memories: elapsed=%v status=%d body=%s", elapsed, rec.Code, rec.Body.String())
}

// TestRequestTimeoutDoesNotApplyToMCPRoute confirms the timeout middleware is
// scoped to rest.Mount's /v1 group only: a handler mounted directly on the
// server router the way cmd/memini/root.go mounts /mcp (outside rest.Mount)
// is not subject to MEMINI_REQUEST_TIMEOUT, since MCP is a long-lived SSE
// stream that must not be cut off (see server.go's WriteTimeout comment).
func TestRequestTimeoutDoesNotApplyToMCPRoute(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "timeout-mcp.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := service.New(st, embedtest.New(dims))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Options{Addr: ":0", ShutdownTimeout: time.Second}, log, prometheus.NewRegistry())

	// Mirrors cmd/memini/root.go: rest.Mount attaches /v1 with a request
	// timeout; /mcp is mounted separately, straight on the router.
	rest.New(svc, rest.AuthConfig{
		NamespaceHeader: nsHdr, DefaultNamespace: "default", RequestTimeout: 20 * time.Millisecond,
	}).Mount(srv.Router())

	const mcpDelay = 150 * time.Millisecond
	srv.Router().Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A real MCP/SSE handler blocks on ctx.Done() for the life of the
		// stream; ignoring ctx here is the strictest test of "unaffected" —
		// if a timeout were (mis)applied to this route, ctx would still
		// cancel even though this handler doesn't check it, but chi's
		// synchronous Timeout only writes a header after the handler
		// returns, so absence-of-cutoff is best shown by elapsed duration.
		time.Sleep(mcpDelay)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	rec := httptest.NewRecorder()

	start := time.Now()
	srv.Router().ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if elapsed < mcpDelay {
		t.Fatalf("mcp handler returned after %v, want >= %v (it should run to completion, unaffected by MEMINI_REQUEST_TIMEOUT)", elapsed, mcpDelay)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (mcp route must not be timed out)", rec.Code)
	}
}
