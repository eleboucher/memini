package config

import (
	"os"
	"path/filepath"
	"strings"
)

// NamespaceFromGitRemote marks a namespace resolved from the `git remote
// get-url origin` repo name — the order the plugins use.
const NamespaceFromGitRemote NamespaceSource = "git-remote"

// ResolvePluginNamespace resolves a namespace the way the Claude Code / OpenClaw
// plugins do (plugin/scripts/_shared.mjs resolveProject):
//
//  1. MEMINI_NAMESPACE (or MEMINI_DEFAULT_NAMESPACE) env, if non-empty
//  2. repo name from `git remote get-url origin` (stable across worktrees/clones)
//  3. basename of `git rev-parse --show-toplevel`
//  4. basename of dir
//
// It differs from resolveDefaultNamespace (the server's header-less fallback)
// only in step 2: the server skips the git-remote step. `memini doctor` uses
// both to flag the divergence that lands writes where recall doesn't look. The
// server's resolution is intentionally left unchanged so existing stores keyed
// by the worktree basename are not silently relocated.
func ResolvePluginNamespace(dir string) (string, NamespaceSource) {
	if v := firstNonEmpty(
		os.Getenv("MEMINI_NAMESPACE"),
		os.Getenv("MEMINI_DEFAULT_NAMESPACE"),
	); v != "" {
		return sanitizeNamespace(v), NamespaceFromEnv
	}
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return defaultNamespace, NamespaceFromLiteral
		}
		dir = cwd
	}
	return ResolveDirNamespace(dir)
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
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "" {
			return parts[i]
		}
	}
	return ""
}
