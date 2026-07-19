// Package render holds the shared response-projection helpers for memini's
// API surfaces (REST and MCP): concise content rendering for progressive
// disclosure, compact display titles, and the content-identity hash clients
// use for injection dedupe. It is a projection layer only — it never touches
// ranking, filtering, or storage.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"unicode"
)

const (
	// SearchMax is the concise rune cap for search results — REST
	// POST /v1/search with response_format="concise" and the MCP
	// memory_recall equivalent.
	SearchMax = 240
	// BriefingMax is the concise rune cap for briefing items
	// (GET /v1/namespaces/briefing?format=concise), matching the plugin
	// client's briefing render cap so server-side concise text is never
	// re-truncated on injection.
	BriefingMax = 280
	// TitleMax is the rune cap for child-rollup display titles — shorter than
	// the concise caps because the rollup is an index of what's under a
	// namespace, not the content itself.
	TitleMax = 60
)

// sentenceWindow is the fraction of the window (from the end) in which a
// sentence boundary is preferred over the plain last-space cut: a sentence
// end inside the last 1/sentenceWindow of the cap ends the concise text on a
// complete sentence; an earlier one would waste most of the budget and is
// ignored in favor of the word-boundary cut.
const sentenceWindow = 4

// Concise returns the compact representation of a memory's text: the summary
// verbatim when one exists, the content verbatim when it already fits in max
// runes, and otherwise a boundary cut of the content with a "…" suffix. The
// boolean reports whether the text is a truncating cut of the content — false
// for a summary or for content that fits, which is how callers decide whether
// to mark content_truncated on the wire.
//
// Cut rules, in order:
//  1. Prefer a sentence boundary — '.', '!', or '?' followed by whitespace
//     (space or newline) or end of text — when the latest one inside the
//     window keeps at least 75% of it, so the concise text ends on a complete
//     sentence without wasting most of the budget.
//  2. Otherwise cut at the last space inside the window (trailing whitespace
//     trimmed), so the cut never lands mid-word. A window whose following
//     rune is whitespace already ends at a word end and is kept whole.
//  3. A window with no space at all (one giant token) is hard-cut at exactly
//     max runes — the legacy behavior.
//
// Every decision — the cap, the scan, and the cut — counts runes, never
// bytes: deciding on bytes would append a spurious "…" to multi-byte content
// under the limit, and cutting on bytes could split a UTF-8 sequence.
func Concise(content, summary string, max int) (string, bool) {
	if summary != "" {
		return summary, false
	}
	runes := []rune(content)
	if len(runes) <= max {
		return content, false
	}
	return string(cut(runes, max)) + "…", true
}

// Title derives a compact display title for index-style listings (the
// briefing's child rollup): the summary verbatim, else a TitleMax-rune
// boundary cut of the content.
func Title(content, summary string) string {
	text, _ := Concise(content, summary, TitleMax)
	return text
}

// ContentHash is the content-identity hash carried on search and briefing
// items: the first 16 hex chars of sha256 over the FULL stored content,
// falling back to the summary only when content is empty. It must match the
// plugin client's injectedIdentity recipe byte-for-byte
// (plugin/scripts/_shared.mjs — content first, summary only when content is
// empty/absent), and is always computed over the stored text, never a
// concise projection, so identity is stable across response formats.
func ContentHash(content, summary string) string {
	text := content
	if text == "" {
		text = summary
	}
	sum := sha256.Sum256([]byte(text))
	// 8 bytes hex-encoded == the first 16 hex chars of the full digest.
	return hex.EncodeToString(sum[:8])
}

// cut returns the boundary cut of runes for a max-rune window, per the rules
// on Concise. len(runes) > max is the caller's guarantee, so runes[max]
// always exists.
func cut(runes []rune, max int) []rune {
	window := runes[:max]
	if i, ok := lastSentenceEnd(runes, max); ok {
		return window[:i+1]
	}
	// The rune just past the window being whitespace means the window already
	// ends exactly at a word end: keep it whole (modulo trailing spaces).
	if unicode.IsSpace(runes[max]) {
		if w := trimRightSpace(window); len(w) > 0 {
			return w
		}
	}
	for j := max - 1; j >= 0; j-- {
		if unicode.IsSpace(window[j]) {
			if w := trimRightSpace(window[:j]); len(w) > 0 {
				return w
			}
			break
		}
	}
	// No usable space in the window: hard cut at the rune cap.
	return window
}

// lastSentenceEnd reports the index (within the first max runes) of the
// latest sentence-ending punctuation whose follower is whitespace or end of
// text, provided cutting there keeps at least (1 - 1/sentenceWindow) of the
// window.
func lastSentenceEnd(runes []rune, max int) (int, bool) {
	threshold := max - max/sentenceWindow
	for i := max - 1; i >= 0 && i+1 >= threshold; i-- {
		if !isSentenceEnd(runes[i]) {
			continue
		}
		if i+1 >= len(runes) || unicode.IsSpace(runes[i+1]) {
			return i, true
		}
	}
	return 0, false
}

func isSentenceEnd(r rune) bool { return r == '.' || r == '!' || r == '?' }

func trimRightSpace(runes []rune) []rune {
	end := len(runes)
	for end > 0 && unicode.IsSpace(runes[end-1]) {
		end--
	}
	return runes[:end]
}
