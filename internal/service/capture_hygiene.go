package service

import (
	"regexp"
	"strings"

	"github.com/eleboucher/memini/internal/memory"
)

// Turn captures — conversation turns auto-saved by client integrations — are
// identified by isTurnCapture (metadata format="turn", see service.go): every
// integration stamps it (the Claude Code plugin adds source="turn_capture",
// hermes/pi/openclaw/openwebui stamp their own source), and it is the same
// predicate the recall echo guard keys on.

// dropsAsCaptureOrLowSignal is Remember's write-drop decision. It first
// applies capture hygiene — a turn capture (metadata format="turn") arrives
// polluted with harness boilerplate that recall would re-inject as noise, so
// its content is rewritten in place via stripCaptureBoilerplate. Stripping
// runs BEFORE the value gate so a capture that was boilerplate-only shrinks
// under MEMINI_EPISODIC_MIN_CHARS and gets value-gated instead of stored; a
// capture emptied entirely is dropped outright, gate or no gate. Non-capture
// writes are never touched, only gated.
func (s *Service) dropsAsCaptureOrLowSignal(tier memory.Tier, in *RememberInput) bool {
	if isTurnCapture(in.Metadata) {
		in.Content = stripCaptureBoilerplate(in.Content)
		if in.Content == "" {
			return true
		}
	}
	return s.dropsLowSignalEpisodic(tier, in.Content)
}

// captureBoilerplateBlocks match harness boilerplate that pollutes captured
// assistant text and would otherwise be re-injected by recall as noise: the
// plugin's own injected wrappers echoed back in the capture, plus harness
// <system-reminder> blocks. Whole blocks are stripped — the body is injected
// context, not conversation. Tags are case-sensitive; the opening tag may
// carry attributes; a block missing its closing tag is stripped to the end of
// the string.
var captureBoilerplateBlocks = func() []*regexp.Regexp {
	tags := []string{
		"memini-context",
		"memini-recall",
		"memini-pretool",
		"memini-memory-directive",
		"memini-compact-recovery",
		"system-reminder",
	}
	res := make([]*regexp.Regexp, 0, len(tags))
	for _, tag := range tags {
		res = append(res, regexp.MustCompile(`(?s)<`+tag+`(?:\s[^>]*)?>.*?(?:</`+tag+`>|\z)`))
	}
	return res
}()

// newlineRuns collapses the 3+ newline runs left behind where a stripped block
// sat between paragraphs.
var newlineRuns = regexp.MustCompile(`\n{3,}`)

// insightMarker opens a Claude Code output-style banner block:
//
//	★ Insight ─────────────────────────────────────
//	stylized commentary
//	─────────────────────────────────────────────────
const insightMarker = "★ Insight"

// boxDashLine reports whether a line consists solely of box-drawing dashes —
// the closing line of a ★ Insight banner.
func boxDashLine(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	for _, r := range t {
		if r != '─' {
			return false
		}
	}
	return true
}

// stripInsightBanners removes ★ Insight banner blocks: from a line starting
// with the marker through the next line consisting of box-drawing dashes,
// inclusive. The whole block goes — the body is stylized commentary, not
// conversation. A banner with no closing dash line is stripped to the end.
func stripInsightBanners(s string) string {
	if !strings.Contains(s, insightMarker) {
		return s
	}
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimLeft(lines[i], " \t"), insightMarker) {
			// Skip to the closing dash line; the loop increment then steps
			// past it. Without one, i runs off the end and the rest is gone.
			for i++; i < len(lines) && !boxDashLine(lines[i]); i++ {
			}
			continue
		}
		kept = append(kept, lines[i])
	}
	return strings.Join(kept, "\n")
}

// stripCaptureBoilerplate removes harness boilerplate from a captured
// conversation turn: injected wrapper blocks (see captureBoilerplateBlocks)
// and ★ Insight banners, then collapses the newline runs left behind and trims
// the result. Clean text comes back unchanged. Pure — the caller decides which
// writes it applies to (turn captures only, see isTurnCapture).
func stripCaptureBoilerplate(s string) string {
	out := s
	for _, re := range captureBoilerplateBlocks {
		out = re.ReplaceAllString(out, "")
	}
	out = stripInsightBanners(out)
	if out == s {
		return s
	}
	out = newlineRuns.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out)
}
