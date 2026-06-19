// Package sanitize provides write-path content hygiene for memini: stripping
// unambiguous corruption (always-on) and detecting "script-salad" garble
// (opt-in). It exists because ingestion stores whatever a harness sends, and an
// upstream model/harness glitch can hand memini a garbled digest that then
// surfaces in recall verbatim.
package sanitize

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Clean strips unambiguous corruption from text before it is embedded or
// persisted: invalid UTF-8 byte sequences, the U+FFFD replacement character,
// C0/C1 control codes (except tab, newline, carriage return), and Unicode
// non-character code points. It deliberately does NOT touch valid printable
// text in any language — legitimate Chinese, Japanese, Arabic, or emoji content
// passes through untouched. A string that is pure binary garbage cleans to (or
// near) empty, which the caller can then reject.
func Clean(s string) string {
	if s == "" {
		return s
	}
	// Drop invalid byte sequences rather than replacing them with U+FFFD.
	s = strings.ToValidUTF8(s, "")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return r
		case r == utf8.RuneError: // pre-existing U+FFFD from earlier mojibake
			return -1
		case r < 0x20, r >= 0x7f && r <= 0x9f: // C0 controls + DEL + C1 controls
			return -1
		case isNonCharacter(r):
			return -1
		default:
			return r
		}
	}, s)
}

// isNonCharacter reports the 66 Unicode non-character code points: the
// contiguous U+FDD0..U+FDEF block, plus U+FFFE/U+FFFF in every plane.
func isNonCharacter(r rune) bool {
	if r >= 0xFDD0 && r <= 0xFDEF {
		return true
	}
	return r&0xFFFE == 0xFFFE
}

// Garble-detection thresholds. A write is flagged only when ALL hold, so the
// heuristic stays conservative: short strings and legitimate mixed-script text
// (CJK embedding Latin tech terms, Japanese mixing Han and kana) fall below it.
const (
	garbleMinLetters     = 12
	garbleMinTransitions = 6
	garbleMinDensity     = 0.20
)

// Script buckets returned by scriptOf. scriptSep marks a non-letter (breaks
// glued-adjacency); scriptOther marks a letter in a script not bucketed below
// (counted but never a transition endpoint).
const (
	scriptSep   = "sep"
	scriptOther = "other"
	scriptCJK   = "cjk"
)

// Garbled reports whether text looks like script-salad — Latin glued to CJK
// glued to Cyrillic with no separators, the signature of garbled multilingual
// model output (e.g. `I'm这个家制品 with在上世纪`). It is a heuristic, not a
// proof: it CANNOT tell semantically-random mixing from a rare legitimate case,
// so callers must treat a positive as "downrank/flag", never "delete". It is
// off by default for exactly this reason.
//
// Only *glued* transitions between two different real scripts count — a space
// or punctuation break resets adjacency, so ordinary code-switching ("the 这个
// thing") scores zero. Han, kana, and hangul collapse into one CJK bucket so
// legitimate Japanese (Han+kana) and CJK-with-embedded-Latin tech terms
// ("使用React框架") stay well under the threshold.
func Garbled(s string) bool {
	letters, transitions := 0, 0
	prev := ""            // script of the previous letter rune
	prevAdjacent := false // was the immediately preceding rune a letter?
	for _, r := range s {
		sc := scriptOf(r)
		if sc == scriptSep {
			prevAdjacent = false
			continue
		}
		letters++
		if prevAdjacent && prev != scriptOther && sc != scriptOther && prev != sc {
			transitions++
		}
		prev = sc
		prevAdjacent = true
	}
	if letters < garbleMinLetters || transitions < garbleMinTransitions {
		return false
	}
	return float64(transitions)/float64(letters) >= garbleMinDensity
}

// scriptOf buckets a rune by script. Non-letters return "sep" (they break
// glued-adjacency); letters in scripts memini doesn't bucket return "other"
// (counted as letters but never as a transition endpoint, so symbols/emoji
// can't trip the detector).
func scriptOf(r rune) string {
	if !unicode.IsLetter(r) {
		return scriptSep
	}
	switch {
	case unicode.Is(unicode.Han, r),
		unicode.Is(unicode.Hiragana, r),
		unicode.Is(unicode.Katakana, r),
		unicode.Is(unicode.Hangul, r),
		unicode.Is(unicode.Bopomofo, r):
		return scriptCJK
	case unicode.Is(unicode.Latin, r):
		return "latin"
	case unicode.Is(unicode.Cyrillic, r):
		return "cyrillic"
	case unicode.Is(unicode.Greek, r):
		return "greek"
	case unicode.Is(unicode.Arabic, r):
		return "arabic"
	case unicode.Is(unicode.Hebrew, r):
		return "hebrew"
	default:
		return scriptOther
	}
}
