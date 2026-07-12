//go:build integration

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/eleboucher/memini/internal/config"
)

// TestIntegrationGracefulShutdown drives runServer itself — the production
// entrypoint — where the other integration tests assemble the stack by hand.
// It pins the SIGTERM path end to end: signal.NotifyContext cancels the run
// context, srv.Run drains the real listener, and the deferred
// joinWorkers/cleanup chain completes so runServer returns nil instead of
// hanging, panicking, or surfacing a shutdown error.
func TestIntegrationGracefulShutdown(t *testing.T) {
	embed := fakeEmbedServer(t)

	// Reserve a loopback port for the real listener runServer will open.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	const apiKey = "secret-token"
	const ns = "it-shutdown"
	t.Setenv("MEMINI_BACKEND", "sqlite")
	t.Setenv("MEMINI_SQLITE_PATH", filepath.Join(t.TempDir(), "memini.db"))
	t.Setenv("MEMINI_EMBED_BASE_URL", embed.URL+"/v1")
	t.Setenv("MEMINI_EMBED_MODEL", "fake")
	t.Setenv("MEMINI_EMBED_DIMS", fmt.Sprint(embedDims))
	t.Setenv("MEMINI_API_KEY", apiKey)
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", ns)
	t.Setenv("MEMINI_UI_ENABLED", "false")
	t.Setenv("MEMINI_HTTP_ADDR", addr)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	done := make(chan error, 1)
	go func() { done <- runServer(cmd, nil) }()

	// Wait for the real listener before signalling: the SIGTERM must arrive
	// after signal.NotifyContext is registered, and readiness proves that.
	base := "http://" + addr
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case err := <-done:
			t.Fatalf("runServer exited before becoming ready: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("server never became ready on " + base)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// One real write so the drain covers a server that has touched the store
	// and embedder, not an idle one.
	req, err := http.NewRequest(http.MethodPost, base+"/v1/memories",
		strings.NewReader(`{"content":"shutdown drain probe"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(config.DefaultNamespaceHeader, ns)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("remember: status %d", resp.StatusCode)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServer returned error on SIGTERM: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runServer did not return within 20s of SIGTERM")
	}

	// The listener must be gone once runServer has returned.
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("listener still accepting connections after shutdown")
	}
}
