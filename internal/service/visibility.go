package service

import (
	"strings"

	"github.com/eleboucher/memini/internal/memory"
)

// lastSegment returns the final "/"-separated component of p ("acme/phoenix"
// -> "phoenix"). Local to this package rather than reusing
// internal/config's lastSegment (unexported there, different package) — the
// inputs here are always ancestorsOf output, which never carries a trailing
// slash or empty segments, so the simpler form is sufficient.
func lastSegment(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// visibilityPersonal is the write-side "personal" token: the caller's home
// namespace (RememberInput.Home / X-Memini-Home). scopeProject (readset.go)
// is the sibling "project" token, shared with the read-side Scope
// vocabulary.
const visibilityPersonal = "personal"

// resolveVisibility resolves RememberInput.Visibility (plus .Home) against the
// request's primary namespace (in.Namespace) and the write's tier into the
// namespace the write actually lands in.
//
// Callers MUST pass the FINAL tier — the value validateRememberInput returns
// after auto-classification, not the raw (possibly empty) in.Tier. An empty
// memory.Tier's Term() defaults to LongTerm (see memory.Tier.Term's switch),
// so calling this with an unresolved "" tier would make the clamp below treat
// an unclassified write as durable and wrongly let it travel. Sequencing
// resolveVisibility after tier resolution — even though that means after
// classification — is what makes the clamp correct for auto-classified
// writes: a capture that the marker heuristic leaves at the working default
// clamps to primary, but one it promotes to semantic/procedural does not,
// and both outcomes depend on seeing the tier classification already
// produced.
//
// Resolution:
//  1. "" or "project": in.Namespace (primary) — the default, no-op path.
//  2. Tier clamp: a non-durable tier (episodic/working) never leaves primary,
//     regardless of visibility. Checked before "personal" so a clamped write
//     never requires MEMINI_HOME to be set — the clamp decides first.
//  3. "personal": in.Home, or an invalid-input error naming MEMINI_HOME when
//     Home is empty.
//  4. Otherwise: an ancestor of primary (nearest-first chain from
//     ancestorsOf), matched either by exact full path or by an unambiguous
//     last path segment. An exact full-path match wins outright, even when
//     the same value also matches a segment. A segment match must be unique
//     among the chain; zero or multiple segment matches (and no exact match)
//     is an error. Every error here enumerates the valid targets — this
//     message is the LLM's teacher, so its format is exact and stable:
//     `visibility %q not in scope; valid: project, personal, <chain...>`.
func resolveVisibility(in RememberInput, tier memory.Tier) (string, error) {
	v := strings.TrimSpace(in.Visibility)
	home := strings.TrimSpace(in.Home)
	if v == "" || v == scopeProject {
		return in.Namespace, nil
	}
	if tier.Term() != memory.LongTerm {
		// Clamp: episodic/working writes never travel, silently — no error,
		// the write still succeeds, just in primary.
		return in.Namespace, nil
	}
	if v == visibilityPersonal {
		if home == "" {
			return "", invalidInputf(
				"remember: visibility \"personal\" requires a home namespace (set MEMINI_HOME on the client)")
		}
		return home, nil
	}

	chain := ancestorsOf(in.Namespace)
	var segMatches []string
	for _, a := range chain {
		if a == v {
			return a, nil // exact full-path match wins outright
		}
		if lastSegment(a) == v {
			segMatches = append(segMatches, a)
		}
	}
	if len(segMatches) == 1 {
		return segMatches[0], nil
	}

	valid := append([]string{scopeProject, visibilityPersonal}, chain...)
	return "", invalidInputf("remember: visibility %q not in scope; valid: %s", v, strings.Join(valid, ", "))
}
