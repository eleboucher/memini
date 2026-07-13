package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// NamespaceFromOverride marks a namespace that came from an explicit per-project
// override the user set (via `/memini:namespace` or an equivalent command in
// another harness), rather than from the environment or git.
const NamespaceFromOverride NamespaceSource = "override"

// overridesFile is $XDG_CONFIG_HOME/memini/overrides.json (or
// ~/.config/memini/overrides.json). It sits beside the tenant config.json, under
// CONFIG rather than CACHE, because it is user intent rather than derived state:
// clearing a cache must never silently discard it.
//
// The file is written by the client plugins (packages/memini-client). The Go
// side only reads it — but it MUST read it, because `memini doctor` exists to
// tell you which namespace is in force, and a doctor that disagreed with the
// plugin about that would be worse than no doctor at all.
type overridesFile struct {
	Version   int                          `json:"version"`
	Overrides map[string]namespaceOverride `json:"overrides"`
}

type namespaceOverride struct {
	Namespace string `json:"namespace"`
	SetAt     string `json:"setAt"`
}

// overridesPath mirrors packages/memini-client's defaultOverridesPath.
func overridesPath() string {
	base := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "memini", "overrides.json")
}

// overrideKeyFor mirrors packages/memini-client's overrideKey: the git toplevel
// when there is one, else the resolved directory. Keying on the repo root rather
// than the raw cwd means an override set at the top of a repo still applies when
// the agent is working several directories down.
func overrideKeyFor(dir string) string {
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return ""
		}
		dir = cwd
	}
	if top := gitToplevel(dir); top != "" {
		if abs, err := filepath.Abs(top); err == nil {
			return abs
		}
		return top
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// NamespaceOverride returns the per-project namespace override in force for dir,
// and whether one exists. Any read or parse error yields ("", false) — a broken
// overrides file must degrade to automatic resolution, never fail a command.
func NamespaceOverride(dir string) (string, bool) {
	p := overridesPath()
	if p == "" {
		return "", false
	}
	raw, err := os.ReadFile(p) //nolint:gosec // path is derived from XDG/home, not user input
	if err != nil {
		return "", false
	}
	var f overridesFile
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Overrides) == 0 {
		return "", false
	}
	// Read the file before computing the key: the key costs a `git rev-parse`,
	// and nobody should pay for one to discover they have no overrides at all.
	key := overrideKeyFor(dir)
	if key == "" {
		return "", false
	}
	entry, ok := f.Overrides[key]
	if !ok {
		return "", false
	}
	ns := sanitizeNamespacePath(strings.TrimSpace(entry.Namespace))
	if ns == "" {
		return "", false
	}
	return ns, true
}

// OverridesPath exposes the overrides file location so `memini doctor` can point
// at it when reporting that an override is in force.
func OverridesPath() string { return overridesPath() }
