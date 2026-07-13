package config

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/eleboucher/memini/internal/nsresolve"
	"github.com/eleboucher/memini/internal/store"
)

// NamespaceFromGitRemote marks a namespace resolved from the `git remote
// get-url origin` repo name — the order ResolveDirNamespace uses.
const NamespaceFromGitRemote NamespaceSource = "git-remote"

// ResolvePluginNamespace resolves a namespace the way an offline CLI caller —
// `memini doctor`, or any other command with no live handshake to ask —
// should: MEMINI_NAMESPACE (or MEMINI_DEFAULT_NAMESPACE) env, if non-empty,
// else derivation via internal/nsresolve, the SAME package POST /v1/handshake
// resolves with server-side (imported here, not duplicated). There is no pin
// lookup and no per-key context: both require a round trip to a running
// server, which is exactly what offline resolution doesn't have — see
// PluginFacts for exactly what is gathered and sent through nsresolve.Resolve.
//
// The returned NamespaceSource is nsresolve's own vocabulary (env, remote,
// toplevel, cwd, server_default — see nsresolve.Source*) rather than this
// package's resolveDefaultNamespace one (env, git, cwd, fallback): mirroring
// nsresolve's derivation means mirroring its labels too, so a source printed
// by `memini doctor` means the same thing whether it came from this resolver
// or from a live handshake response.
//
// It differs from resolveDefaultNamespace (the server's header-less
// fallback) in exactly the two ways `memini doctor` exists to flag: this
// resolver consults the git remote (the server's own default skips straight
// to the toplevel basename) and nests the result under MEMINI_AGENT (the
// server default never does, since a bare MCP client sends no agent). The
// server's resolution is intentionally left unchanged so existing stores
// keyed by the worktree basename are not silently relocated.
func ResolvePluginNamespace(dir string) (string, NamespaceSource) {
	facts := PluginFacts(dir)
	res, err := nsresolve.Resolve(context.Background(), facts, nil, store.ClientSettings{}, "", defaultNamespace)
	if err != nil {
		// Facts derived from real git/cwd output should never fail
		// httputil.ValidateNamespace, but this function's signature has no
		// error to propagate — degrade to the same literal fallback
		// resolveDefaultNamespace uses as its own last resort rather than
		// panicking or returning a namespace nothing actually resolved to.
		return defaultNamespace, NamespaceFromLiteral
	}
	return res.Namespace, NamespaceSource(res.Source)
}

// PluginFacts gathers the project facts an offline CLI caller can supply for
// nsresolve derivation, or for a POST /v1/handshake request body (`memini
// doctor`'s handshake probe sends exactly what this returns): the git remote
// URL, git toplevel path/basename, cwd basename, MEMINI_AGENT, and the
// client's MEMINI_NAMESPACE/MEMINI_DEFAULT_NAMESPACE (sent as EnvNamespace so
// a server-side pin can still beat it — see nsresolve.Facts.EnvNamespace).
// DeclaredNamespace is deliberately left unset: that field is for gateway/CI
// callers with no meaningful cwd, which neither ResolvePluginNamespace nor
// doctor is. dir is the working directory to resolve from; "" uses
// os.Getwd().
func PluginFacts(dir string) nsresolve.Facts {
	if dir == "" {
		if cwd, err := os.Getwd(); err == nil {
			dir = cwd
		}
	}
	top := gitToplevel(dir)
	return nsresolve.Facts{
		RemoteURL:        gitRemoteOrigin(dir),
		ToplevelPath:     top,
		ToplevelBasename: basenameOrEmpty(top),
		CwdBasename:      basenameOrEmpty(dir),
		Agent:            os.Getenv("MEMINI_AGENT"),
		EnvNamespace: firstNonEmpty(
			os.Getenv("MEMINI_NAMESPACE"),
			os.Getenv("MEMINI_DEFAULT_NAMESPACE"),
		),
	}
}

// basenameOrEmpty is filepath.Base guarded against "" (Base("") == "." would
// otherwise derive a bogus "." namespace) and a bare separator — the same
// edge cases sanitizeNamespace strips for the server-default resolver.
func basenameOrEmpty(dir string) string {
	if dir == "" {
		return ""
	}
	b := filepath.Base(dir)
	if b == "." || b == string(filepath.Separator) {
		return ""
	}
	return b
}

// ResolveDirNamespace resolves a directory's namespace from git, ignoring any
// MEMINI_NAMESPACE env override: git remote origin repo name, then the worktree
// basename, then the directory basename. The claude-code backfill uses this so
// each project's transcripts land in their own namespace instead of collapsing
// into one global env namespace — which is exactly the pooling failure to avoid.
func ResolveDirNamespace(dir string) (string, NamespaceSource) {
	if url := gitRemoteOrigin(dir); url != "" {
		if name := repoNameFromRemote(url); name != "" {
			return sanitizeNamespace(name), NamespaceFromGitRemote
		}
	}
	if top := gitToplevel(dir); top != "" {
		return sanitizeNamespace(filepath.Base(top)), NamespaceFromGit
	}
	return sanitizeNamespace(filepath.Base(dir)), NamespaceFromCWD
}

// gitRemoteOrigin returns the origin remote URL for dir, or "" if there is none
// or the lookup fails/times out.
func gitRemoteOrigin(dir string) string {
	ctx, cancel := execContext()
	defer cancel()
	out, err := runGit(ctx, dir, "git", "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// repoNameFromRemote extracts the repo basename from a git remote URL, handling
// ssh://, https:// and scp-style (git@host:owner/repo) forms and stripping a
// trailing .git. Returns "" on a parse failure. Mirrors the plugin's
// repoNameFromRemote so server and plugin agree on the same name.
func repoNameFromRemote(url string) string {
	cleaned := strings.TrimRight(strings.TrimSpace(url), "/")
	if len(cleaned) >= 4 && strings.EqualFold(cleaned[len(cleaned)-4:], ".git") {
		cleaned = cleaned[:len(cleaned)-4]
	}
	if cleaned == "" {
		return ""
	}
	// scp-style "host:owner/repo": a colon whose host part has no slash and
	// whose path doesn't start with a slash (which would be a "://" scheme).
	if i := strings.IndexByte(cleaned, ':'); i > 0 {
		host, rest := cleaned[:i], cleaned[i+1:]
		if !strings.Contains(host, "/") && rest != "" && rest[0] != '/' {
			return lastSegment(rest)
		}
	}
	return lastSegment(cleaned)
}

// lastSegment returns the last non-empty "/"-separated segment of p.
func lastSegment(p string) string {
	parts := strings.Split(p, "/")
	for _, v := range slices.Backward(parts) {
		if v != "" {
			return v
		}
	}
	return ""
}
