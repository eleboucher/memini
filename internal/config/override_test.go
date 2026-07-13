package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeOverrides writes an overrides.json into a temp XDG_CONFIG_HOME and points
// the env at it, mirroring the file packages/memini-client produces.
func writeOverrides(t *testing.T, entries map[string]namespaceOverride) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if entries == nil {
		return
	}
	if err := os.MkdirAll(filepath.Join(dir, "memini"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(overridesFile{Version: 1, Overrides: entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memini", "overrides.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNamespaceOverride_AbsentFile(t *testing.T) {
	writeOverrides(t, nil)
	if ns, ok := NamespaceOverride(t.TempDir()); ok {
		t.Fatalf("expected no override, got %q", ns)
	}
}

func TestNamespaceOverride_MalformedFileDegrades(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "memini"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memini", "overrides.json"), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A broken overrides file must fall back to automatic resolution, never fail
	// the command — doctor has to keep working when the thing it diagnoses is
	// itself corrupt.
	if ns, ok := NamespaceOverride(t.TempDir()); ok {
		t.Fatalf("expected no override from malformed file, got %q", ns)
	}
}

func TestNamespaceOverride_MatchesProjectDir(t *testing.T) {
	proj := t.TempDir()
	key, err := filepath.Abs(proj)
	if err != nil {
		t.Fatal(err)
	}
	writeOverrides(t, map[string]namespaceOverride{
		key: {Namespace: "acme/api", SetAt: "2026-07-12T20:30:00Z"},
	})

	ns, ok := NamespaceOverride(proj)
	if !ok || ns != "acme/api" {
		t.Fatalf("NamespaceOverride = (%q, %v), want (\"acme/api\", true)", ns, ok)
	}

	// A different project must not pick it up.
	if ns, ok := NamespaceOverride(t.TempDir()); ok {
		t.Fatalf("override leaked to an unrelated project: %q", ns)
	}
}

func TestResolvePluginNamespace_OverrideBeatsEnvPin(t *testing.T) {
	// The ordering the whole feature rests on, asserted on the Go side too:
	// `memini doctor` must agree with the plugin about which namespace is in
	// force, and the plugin puts the override above the env var. If doctor got
	// this backwards it would report a namespace nothing actually writes to —
	// on precisely the machines where a global MEMINI_NAMESPACE is the bug.
	proj := t.TempDir()
	key, err := filepath.Abs(proj)
	if err != nil {
		t.Fatal(err)
	}
	writeOverrides(t, map[string]namespaceOverride{
		key: {Namespace: "the-real-one", SetAt: "2026-07-12T20:30:00Z"},
	})
	t.Setenv("MEMINI_NAMESPACE", "pinned-everywhere")
	t.Setenv("MEMINI_AGENT", "")

	ns, src := ResolvePluginNamespace(proj)
	if ns != "the-real-one" {
		t.Fatalf("ResolvePluginNamespace = %q, want \"the-real-one\" (the override must beat the env pin)", ns)
	}
	if src != NamespaceFromOverride {
		t.Fatalf("source = %q, want %q", src, NamespaceFromOverride)
	}
}

func TestResolvePluginNamespace_EnvPinWinsWithoutOverride(t *testing.T) {
	writeOverrides(t, nil)
	t.Setenv("MEMINI_NAMESPACE", "pinned-everywhere")
	t.Setenv("MEMINI_AGENT", "")

	ns, src := ResolvePluginNamespace(t.TempDir())
	if ns != "pinned-everywhere" || src != NamespaceFromEnv {
		t.Fatalf("ResolvePluginNamespace = (%q, %q), want (\"pinned-everywhere\", %q)", ns, src, NamespaceFromEnv)
	}
}
