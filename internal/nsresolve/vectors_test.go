package nsresolve_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/eleboucher/memini/internal/nsresolve"
	"github.com/eleboucher/memini/internal/store"
)

// derivationVectors is the shared cross-language contract at
// packages/memini-client/test/fixtures/derivation-vectors.json: the same cases
// Go (this package), TS (@memini/client), and the Python integration tests all
// run their derivation through, so the three implementations can never silently
// disagree. It is consumed IN PLACE (located relative to this test file, never
// copied) — a copy would rot the instant the canonical fixture changed.
type derivationVectors struct {
	Description string `json:"description"`
	Derivation  []struct {
		Name  string `json:"name"`
		Facts struct {
			RemoteURL         string `json:"remote_url"`
			ToplevelPath      string `json:"toplevel_path"`
			ToplevelBasename  string `json:"toplevel_basename"`
			CwdBasename       string `json:"cwd_basename"`
			Agent             string `json:"agent"`
			EnvNamespace      string `json:"env_namespace"`
			DeclaredNamespace string `json:"declared_namespace"`
		} `json:"facts"`
		Scope  string `json:"scope"`
		Expect struct {
			Namespace string `json:"namespace"`
			Source    string `json:"source"`
		} `json:"expect"`
	} `json:"derivation"`
	CanonicalRemote []struct {
		Input  string `json:"input"`
		Expect string `json:"expect"`
		Note   string `json:"note"`
	} `json:"canonical_remote"`
}

// loadVectors reads the canonical fixture, resolving its path from THIS test
// file's location via runtime.Caller so the test never bakes in a working
// directory and never needs a copy of the fixture in the Go tree.
func loadVectors(t *testing.T) derivationVectors {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the derivation-vectors fixture")
	}
	// internal/nsresolve/ -> repo root is ../../
	fixture := filepath.Join(filepath.Dir(thisFile), "..", "..",
		"packages", "memini-client", "test", "fixtures", "derivation-vectors.json")
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture %s: %v", fixture, err)
	}
	var v derivationVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(v.Derivation) == 0 || len(v.CanonicalRemote) == 0 {
		t.Fatalf("fixture looks empty: %d derivation, %d canonical_remote cases",
			len(v.Derivation), len(v.CanonicalRemote))
	}
	return v
}

// TestDerivationVectors runs every derivation case through the no-pin, no-env
// fallback chain (declared > remote > toplevel > cwd, plus namespace_scope and
// the agent suffix) — the exact chain the fixture describes — and asserts both
// the resolved namespace and its source match the cross-language contract.
func TestDerivationVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Derivation {
		t.Run(c.Name, func(t *testing.T) {
			var s store.ClientSettings
			if c.Scope != "" {
				sc := c.Scope
				s.NamespaceScope = &sc
			}
			f := nsresolve.Facts{
				RemoteURL:         c.Facts.RemoteURL,
				ToplevelPath:      c.Facts.ToplevelPath,
				ToplevelBasename:  c.Facts.ToplevelBasename,
				CwdBasename:       c.Facts.CwdBasename,
				Agent:             c.Facts.Agent,
				EnvNamespace:      c.Facts.EnvNamespace,
				DeclaredNamespace: c.Facts.DeclaredNamespace,
			}
			// No pins, no key/server default: this exercises exactly the
			// declared>remote>toplevel>cwd chain the fixture is about. A
			// sentinel server-default that no case should ever reach makes an
			// unexpected fall-through obvious rather than silently passing.
			got, err := nsresolve.Resolve(context.Background(), f, nil, s, "", "unreachable-server-default")
			if err != nil {
				t.Fatalf("Resolve(%s): %v", c.Name, err)
			}
			if got.Namespace != c.Expect.Namespace {
				t.Errorf("namespace = %q, want %q", got.Namespace, c.Expect.Namespace)
			}
			if got.Source != c.Expect.Source {
				t.Errorf("source = %q, want %q", got.Source, c.Expect.Source)
			}
		})
	}
}

// TestCanonicalRemoteVectors runs every canonical_remote case through
// CanonicalRemote — the pin-key canonicalization that must agree byte-for-byte
// with the other languages so a pin created by one client is found by another.
func TestCanonicalRemoteVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.CanonicalRemote {
		t.Run(c.Input, func(t *testing.T) {
			if got := nsresolve.CanonicalRemote(c.Input); got != c.Expect {
				t.Errorf("CanonicalRemote(%q) = %q, want %q (%s)", c.Input, got, c.Expect, c.Note)
			}
		})
	}
}
