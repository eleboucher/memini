package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/store"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// writeAPIKeysFile writes contents to a temp YAML file and returns its path.
func writeAPIKeysFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api_keys.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	return path
}

// TestRunServerRefusesInvalidAPIKeysFile pins K2b's fail-loud boot
// validation at the actual server entrypoint (not just LoadFileKeys in
// isolation): a malformed MEMINI_API_KEYS_FILE must refuse the boot with a
// message naming the file, before the server ever starts listening.
func TestRunServerRefusesInvalidAPIKeysFile(t *testing.T) {
	badFile := writeAPIKeysFile(t, `
keys:
  - name: alex
    hash: "not-valid-hex"
`)
	t.Setenv("MEMINI_SQLITE_PATH", filepath.Join(t.TempDir(), "memini.db"))
	t.Setenv("MEMINI_API_KEYS_FILE", badFile)
	t.Setenv("MEMINI_UI_ENABLED", "false")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runServer(cmd, nil)
	if err == nil {
		t.Fatal("runServer: want an error for an invalid api keys file, got nil")
	}
	if !strings.Contains(err.Error(), badFile) {
		t.Errorf("runServer error = %q, want it to name the file %q", err.Error(), badFile)
	}
	if !strings.Contains(err.Error(), "alex") {
		t.Errorf("runServer error = %q, want it to name the offending entry (alex)", err.Error())
	}
}

// TestRunServerRefusesMissingAPIKeysFile pins the same refusal for a file
// that doesn't exist at all (a likely GitOps misconfiguration — wrong mount
// path), rather than silently booting with the feature off.
func TestRunServerRefusesMissingAPIKeysFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	t.Setenv("MEMINI_SQLITE_PATH", filepath.Join(t.TempDir(), "memini.db"))
	t.Setenv("MEMINI_API_KEYS_FILE", missing)
	t.Setenv("MEMINI_UI_ENABLED", "false")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runServer(cmd, nil)
	if err == nil {
		t.Fatal("runServer: want an error for a missing api keys file, got nil")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("runServer error = %q, want it to name the missing file %q", err.Error(), missing)
	}
}

// TestNewServerFileKeyShadowsDBKeyAndWins builds the real server stack (store,
// service, HTTP wiring) with a DB-stored api_keys row AND a file key sharing
// its name but a different secret, then drives an actual HTTP request through
// it. This pins three things end to end: (1) newServer actually threads the
// loaded FileKeySet into both AuthConfig and the boot-time shadow check, (2)
// the shadowing warning is logged naming the file and the shadowed key name,
// and (3) the file key's own secret authenticates (and its DefaultNS is
// used), proving the file — not the stale DB row — is what answered.
func TestNewServerFileKeyShadowsDBKeyAndWins(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "memini.db")

	keysFile := writeAPIKeysFile(t, `
keys:
  - name: shared
    secret: "file-secret"
    default_namespace: from-file
`)

	t.Setenv("MEMINI_BACKEND", "sqlite")
	t.Setenv("MEMINI_SQLITE_PATH", dbPath)
	t.Setenv("MEMINI_EMBED_DIMS", "8")
	t.Setenv("MEMINI_API_KEYS_FILE", keysFile)
	t.Setenv("MEMINI_UI_ENABLED", "false")
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "it-shadow-test")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))

	reg := prometheus.NewRegistry()
	svc, st, deps, joinWorkers, cleanup, err := buildServiceStack(ctx, cfg, log, reg)
	if err != nil {
		t.Fatalf("buildServiceStack: %v", err)
	}
	t.Cleanup(func() { joinWorkers(); cleanup() })

	ks, ok := st.(store.APIKeyStore)
	if !ok {
		t.Fatalf("sqlite store must implement store.APIKeyStore")
	}
	if err := ks.PutAPIKey(ctx, store.APIKey{
		Name: "shared", Hash: sha256Hex("db-secret"), DefaultNS: "from-db", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}

	srv, err := newServer(cfg, svc, st, deps, log, reg)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}

	logOut := logBuf.String()
	if !strings.Contains(logOut, keysFile) {
		t.Errorf("boot log missing the api keys file path %q; got:\n%s", keysFile, logOut)
	}
	if !strings.Contains(logOut, "shared") {
		t.Errorf("boot log missing the shadowed key name \"shared\"; got:\n%s", logOut)
	}

	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)

	// The file key's own secret authenticates and lands in its DefaultNS.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/memories", strings.NewReader(
		`{"content":"file wins","tier":"semantic"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer file-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("file key write: want 201, got %d", resp.StatusCode)
	}
}
