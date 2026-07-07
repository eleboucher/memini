package importer

import (
	"regexp"
	"strings"
)

// Claude Code transcripts carry harness chrome that isn't knowledge: injected
// <system-reminder> blocks, command wrappers, task notifications, hook output,
// and memini's own <memini-context> briefing. stripNoise removes it line-anchored
// (prose merely mentioning a tag inline is kept) at import time only — the live
// Remember path never rewrites a caller's content.

// noiseTags wrap blocks that are harness chrome, not conversation. memini-context
// is memini's own SessionStart briefing, echoed back into later turns.
var noiseTags = []string{
	"system-reminder",
	"command-message",
	"command-name",
	"command-args",
	"task-notification",
	"user-prompt-submit-hook",
	"hook_output",
	"local-command-stdout",
	"local-command-stderr",
	"memini-context",
}

// noisePatterns run over a single message's text. Tag blocks use a lazy (?s).*?
// so an unclosed tag matches nothing; per-message scope keeps a block from
// bleeding across turns (RE2 has no negative lookahead to bound it otherwise).
var noisePatterns = func() []*regexp.Regexp {
	pats := make([]*regexp.Regexp, 0, len(noiseTags)+3)
	for _, tag := range noiseTags {
		// ^[ \t>]* tolerates blockquote/indent prefixes before the open tag.
		pats = append(pats, regexp.MustCompile(
			`(?sm)^[ \t>]*<`+tag+`(?:\s[^>]*)?>.*?</`+tag+`>[ \t]*\n?`))
	}
	hookNames := "Stop|PreCompact|PreToolUse|PostToolUse|UserPromptSubmit|Notification|SessionStart|SessionEnd"
	pats = append(pats,
		// Claude Code TUI hook-run chrome: "Ran 2 Stop hook", "Ran 1 PreCompact hook".
		regexp.MustCompile(`(?m)^[ \t>]*Ran \d+ (?:`+hookNames+`) hooks?.*\n?`),
		// Collapsed-output marker: "… +12 lines".
		regexp.MustCompile(`(?m)^[ \t>]*…\s*\+\d+ lines.*\n?`),
		// Collapsed-output chrome: "[1234 tokens] (ctrl+o to expand)".
		regexp.MustCompile(`\s*\[\d+\s+tokens?\]\s*\(ctrl\+o to expand\)`),
	)
	return pats
}()

// untrustedMetadataBlock matches a leading OpenClaw "<Label> (untrusted
// metadata):" preamble followed by either a fenced JSON object or flat
// key=value lines — the runtime plumbing the OpenClaw plugin strips at capture.
// Ported here so a re-import of content captured before that fix (or exported
// from another memini) doesn't reintroduce it. Anchored at the start; the flat
// branch requires a bare key=value so a real message merely containing "=" is
// not consumed. See integrations/openclaw/plugin/src/index.ts.
var untrustedMetadataBlock = regexp.MustCompile(
	"^\\s*[^\\n]*\\(untrusted metadata\\):\\s*" +
		"(?:```(?:json)?\\s*[\\s\\S]*?```\\s*|(?:[ \\t]*[A-Za-z_][\\w.-]*=[^\\n]*\\n?)+)")

// stripRuntimePreambles removes leading untrusted-metadata blocks (stacked
// blocks are peeled one per pass) and trims the result.
func stripRuntimePreambles(s string) string {
	for untrustedMetadataBlock.MatchString(s) {
		s = untrustedMetadataBlock.ReplaceAllString(s, "")
	}
	return strings.TrimSpace(s)
}

// noiseFramingPrefixes mark a turn that is runtime framing, not knowledge: a
// cron/scheduled job definition or a subagent task delegation.
var noiseFramingPrefixes = []string{"[cron:", "[Subagent Context]"}

// roleLabel is an optional leading "User:"/"Assistant:"/"System:" that OpenClaw's
// turn text keeps in front of the content — it must not defeat the prefix match.
var roleLabel = regexp.MustCompile(`(?i)^(?:user|assistant|system)\s*:\s*`)

// isNoiseFraming reports whether content begins with a framing marker (behind an
// optional role label). A message that merely mentions a marker mid-line is kept.
func isNoiseFraming(s string) bool {
	body := roleLabel.ReplaceAllString(strings.TrimSpace(s), "")
	for _, p := range noiseFramingPrefixes {
		if strings.HasPrefix(body, p) {
			return true
		}
	}
	return false
}

// blankRuns collapses 4+ consecutive newlines (left behind by removals) to 3.
var blankRuns = regexp.MustCompile(`\n{4,}`)

// stripNoise removes harness chrome from one message's text and trims the
// result. An empty return means the message was nothing but noise.
func stripNoise(s string) string {
	for _, p := range noisePatterns {
		s = p.ReplaceAllString(s, "")
	}
	s = blankRuns.ReplaceAllString(s, "\n\n\n")
	return strings.TrimSpace(s)
}
