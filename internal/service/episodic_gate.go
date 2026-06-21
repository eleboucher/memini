package service

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/eleboucher/memini/internal/memory"
)

// turnScaffold matches the role labels integrations prepend to a captured turn
// ("User:", "Assistant:", case-insensitive, at a line start) so the gate scores
// the exchange, not the framing. OpenClaw captures carry no labels; for those
// this is a no-op and the gate just measures the trimmed length.
var turnScaffold = regexp.MustCompile(`(?im)^[ \t]*(user|assistant|human|ai)[ \t]*:[ \t]*`)

// episodicSignalChars counts the substantive characters in a capture: content
// with role scaffolding and surrounding whitespace stripped. A "keep going" /
// "ok" turn scores near zero; a real exchange (or a terse turn paired with a
// substantive answer) scores high. The write-gate thresholds on this.
func episodicSignalChars(content string) int {
	return utf8.RuneCountInString(strings.TrimSpace(turnScaffold.ReplaceAllString(content, "")))
}

// dropsLowSignalEpisodic reports whether an episodic write should be dropped for
// carrying too little signal. Only episodic is gated, and only when
// episodicMinChars > 0.
func (s *Service) dropsLowSignalEpisodic(tier memory.Tier, content string) bool {
	return tier == memory.TierEpisodic && s.episodicMinChars > 0 &&
		episodicSignalChars(content) < s.episodicMinChars
}
