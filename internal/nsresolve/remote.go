// Package nsresolve is the transport-free namespace-resolution core behind the
// config-handshake redesign. It turns the project facts a client sends
// (POST /v1/handshake) into the one namespace that caller should use, applying
// the precedence pin > env > declared > derive > key-default > server-default.
//
// It deliberately owns no I/O: the only way it reaches the project_map pin
// table is the PinLookup callback the caller supplies, so the package stays a
// pure function of (facts, settings, callback results) — the same inputs always
// produce the same Result, which is what lets the handshake be deterministic
// and side-effect-free. Its only dependencies are internal/store (for the
// ClientSettings type that carries namespace_scope/namespace_prefix) and
// internal/httputil (namespace normalize/validate, standard-library only).
package nsresolve

import (
	"regexp"
	"strings"
)

// scpStyleRe matches an scp-style remote (git@host:owner/repo): a run of
// non-slash, non-colon host characters, then a colon, then a non-slash — which
// is what distinguishes "git@github.com:acme/repo" from a "scheme://" URL,
// where the character after the first colon is always a slash. Ported verbatim
// from plugin/scripts/_shared.mjs:37 so the two languages segment identically.
var scpStyleRe = regexp.MustCompile(`^[^/:]+:[^/]`)

// schemeRe matches a leading URL scheme (https://, http://, ssh://, git://),
// case-insensitively — the prefix CanonicalRemote strips before splitting the
// authority from the path.
var schemeRe = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://`)

// dotGitRe matches a trailing .git suffix, case-insensitively.
var dotGitRe = regexp.MustCompile(`(?i)\.git$`)

// cleanRemote applies the shared pre-segmentation cleanup: trim surrounding
// whitespace, strip trailing slashes, then strip a trailing .git — the exact
// order plugin/scripts/_shared.mjs:35 uses (slash strip before .git strip, so
// "repo.git/" reduces to "repo").
func cleanRemote(url string) string {
	cleaned := strings.TrimSpace(url)
	cleaned = strings.TrimRight(cleaned, "/")
	cleaned = dotGitRe.ReplaceAllString(cleaned, "")
	return cleaned
}

// remotePathSegments splits a git remote URL into its path segments (owner,
// repo, ...), dropping the host/user/port and any empty segments. Ported from
// plugin/scripts/_shared.mjs:33-40 so Go derivation matches the plugin's:
// scp-style remotes keep only the part after the first colon, scheme URLs keep
// their whole "scheme://host/owner/repo" split (host/scheme land in discarded
// leading segments — only the last one or two segments are ever read). Returns
// nil on an empty or unparseable URL. Case is preserved deliberately: the
// derived namespace keeps the remote's original case (contrast CanonicalRemote,
// which lowercases for the pin key).
func remotePathSegments(url string) []string {
	if strings.TrimSpace(url) == "" {
		return nil
	}
	cleaned := cleanRemote(url)
	if cleaned == "" {
		return nil
	}
	path := cleaned
	if scpStyleRe.MatchString(cleaned) {
		if _, after, ok := strings.Cut(cleaned, ":"); ok {
			path = after
		}
	}
	var segs []string
	for s := range strings.SplitSeq(path, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	return segs
}

// repoNameFromRemote returns the repo name (last path segment) of a git remote
// URL, or "" when none can be parsed. The "repo"-scope derivation basename;
// mirrors plugin/scripts/_shared.mjs:46-49 (repoNameFromRemote).
func repoNameFromRemote(url string) string {
	segs := remotePathSegments(url)
	if len(segs) == 0 {
		return ""
	}
	return segs[len(segs)-1]
}

// repoSlugFromRemote returns an "owner-repo" slug (last two path segments joined
// with "-") so same-named repos under different owners don't collide, or the
// bare repo name when only one segment is present, or "" when unparseable. Only
// the owner segment is sanitized ([^A-Za-z0-9._-]+ -> "-", trimmed), matching
// plugin/scripts/_shared.mjs:58-65 (repoSlugFromRemote) — the repo segment and
// the owner's case are left untouched.
func repoSlugFromRemote(url string) string {
	segs := remotePathSegments(url)
	if len(segs) == 0 {
		return ""
	}
	if len(segs) == 1 {
		return segs[0]
	}
	owner := sanitizeSegment(segs[len(segs)-2])
	repo := segs[len(segs)-1]
	if owner == "" {
		return repo
	}
	return owner + "-" + repo
}

// CanonicalRemote reduces a raw git remote URL to a stable, credential-free
// pin key of the form "host/owner/repo": scheme, user@, and :port are stripped,
// scp-style "host:path" colons become slashes, a trailing "/" and ".git" are
// removed, and the whole result is lowercased so trivial formatting or
// case differences between two clones of the same repo resolve to one key.
// Returns "" for an empty/unparseable URL.
//
// Unlike remotePathSegments (which discards the host — only the tail matters for
// derivation), the pin key KEEPS the host, so github.com/acme/app and
// gitlab.com/acme/app are distinct pins. And unlike derivation, it lowercases:
// a pin must match regardless of how a remote happens to be cased.
func CanonicalRemote(url string) string {
	cleaned := cleanRemote(url)
	if cleaned == "" {
		return ""
	}

	var authority, path string
	switch {
	case scpStyleRe.MatchString(cleaned):
		// scp-style: everything before the first colon is the authority, the
		// rest is the path. scpStyleRe (`^[^/:]+:[^/]`) can never match a
		// scheme:// URL (the char after the first colon is always "/"), so no
		// schemeRe guard is needed here — the default branch handles schemes.
		i := strings.Index(cleaned, ":")
		authority, path = cleaned[:i], cleaned[i+1:]
	default:
		rest := schemeRe.ReplaceAllString(cleaned, "")
		if before, after, ok := strings.Cut(rest, "/"); ok {
			authority, path = before, after
		} else {
			authority = rest
		}
	}

	// Strip user@ (identity, never part of the repo's identity) and :port.
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		authority = authority[i+1:]
	}
	if i := strings.Index(authority, ":"); i >= 0 {
		authority = authority[:i]
	}

	out := authority
	if path != "" {
		out += "/" + path
	}
	return strings.ToLower(out)
}

// sanitizeSegmentRe is the character class replaced by "-" when sanitizing an
// owner segment or an agent suffix, matching plugin/scripts/_shared.mjs:62,156.
var sanitizeSegmentRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitizeSegment collapses every run of characters outside [A-Za-z0-9._-] to a
// single "-", then trims leading/trailing "-". Shared by the owner-repo slug
// and the agent suffix so both sanitize identically to the plugin.
func sanitizeSegment(s string) string {
	return strings.Trim(sanitizeSegmentRe.ReplaceAllString(s, "-"), "-")
}
