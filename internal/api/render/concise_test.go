package render_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/eleboucher/memini/internal/api/render"
)

// TestConciseSummaryPassthrough pins that a stored summary is always returned
// verbatim and never reported as truncated — even when the summary itself is
// longer than the cap (the summary is the author's chosen short form; cutting
// it would double-compress).
func TestConciseSummaryPassthrough(t *testing.T) {
	long := strings.Repeat("content ", 100)
	got, truncated := render.Concise(long, "the short form", 240)
	if got != "the short form" || truncated {
		t.Fatalf("Concise = (%q, %v), want summary verbatim and truncated=false", got, truncated)
	}

	longSummary := strings.Repeat("summary word ", 40) // > 240 runes
	got, truncated = render.Concise(long, longSummary, 240)
	if got != longSummary || truncated {
		t.Fatalf("over-cap summary: got (%d runes, %v), want verbatim and truncated=false",
			len([]rune(got)), truncated)
	}
}

// TestConciseShortContentUntouched pins that content at or under the rune cap
// passes through byte-for-byte with no ellipsis and truncated=false.
func TestConciseShortContentUntouched(t *testing.T) {
	for name, content := range map[string]string{
		"short":       "the deploy key lives in vault",
		"exactly-max": strings.Repeat("a", 240),
		"empty":       "",
	} {
		got, truncated := render.Concise(content, "", 240)
		if got != content || truncated {
			t.Fatalf("%s: Concise = (%q, %v), want verbatim and truncated=false", name, got, truncated)
		}
	}
}

// TestConciseWordBoundaryCut pins that a cut never lands mid-word: the text is
// cut at the last space inside the window and trailing whitespace is trimmed
// before the ellipsis.
func TestConciseWordBoundaryCut(t *testing.T) {
	// max=10 window over "alpha beta gamma delta" is "alpha beta"; rune 10 is a
	// space, so the window already ends at a word end and is kept whole.
	got, truncated := render.Concise("alpha beta gamma delta", "", 10)
	if got != "alpha beta…" || !truncated {
		t.Fatalf("cut at word end: got (%q, %v), want (\"alpha beta…\", true)", got, truncated)
	}

	// max=8 window is "alpha be" — mid-word in "beta", so the cut retreats to
	// the last space and drops the fragment.
	got, truncated = render.Concise("alpha beta gamma delta", "", 8)
	if got != "alpha…" || !truncated {
		t.Fatalf("mid-word cut: got (%q, %v), want (\"alpha…\", true)", got, truncated)
	}

	// Consecutive spaces before the cut point are all trimmed.
	got, truncated = render.Concise("alpha   beta gamma", "", 9)
	if got != "alpha…" || !truncated {
		t.Fatalf("multi-space trim: got (%q, %v), want (\"alpha…\", true)", got, truncated)
	}
}

// TestConciseNoSpaceHardCut pins the fallback: content with no space anywhere
// in the window (one giant token) is hard-cut at exactly max runes — the
// legacy behavior — rather than returning nothing.
func TestConciseNoSpaceHardCut(t *testing.T) {
	content := strings.Repeat("x", 500)
	got, truncated := render.Concise(content, "", 240)
	if got != strings.Repeat("x", 240)+"…" || !truncated {
		t.Fatalf("no-space content: got %d runes, truncated=%v; want 240-rune hard cut + ellipsis",
			len([]rune(got)), truncated)
	}
}

// TestConciseSentenceBoundaryPreferred pins that a sentence end (. ! ?
// followed by a space or newline) inside the last 25% of the window wins over
// the plain last-space cut, so the concise text ends on a complete sentence.
func TestConciseSentenceBoundaryPreferred(t *testing.T) {
	// "." at rune index 40 (cut length 41) — inside the last 25% of a 50-rune
	// window (threshold 38) — beats the later word boundary.
	content := "The deploy pipeline is fully green today. Everything else follows later."
	got, truncated := render.Concise(content, "", 50)
	if got != "The deploy pipeline is fully green today.…" || !truncated {
		t.Fatalf("sentence cut: got (%q, %v)", got, truncated)
	}

	// "!" and "?" are sentence ends too, and a newline counts as the follower.
	got, _ = render.Concise("Ship it! More words beyond the window follow here", "", 10)
	if got != "Ship it!…" {
		t.Fatalf("bang cut: got %q, want \"Ship it!…\"", got)
	}
	got, _ = render.Concise("Why not?\nBecause the window ends right here somewhere", "", 10)
	if got != "Why not?…" {
		t.Fatalf("question cut: got %q, want \"Why not?…\"", got)
	}
}

// TestConciseSentenceBoundaryTooEarlyIgnored pins the 25% rule: a sentence end
// early in the window must NOT shrink the concise text to a fraction of the
// budget — the cut falls back to the last word boundary.
func TestConciseSentenceBoundaryTooEarlyIgnored(t *testing.T) {
	// "." at rune index 8 (cut length 9) is far outside the last 25% of a
	// 40-rune window (threshold 30) — the word-boundary cut must win.
	content := "Short o. " + strings.Repeat("word ", 20)
	got, truncated := render.Concise(content, "", 40)
	if !truncated {
		t.Fatal("want truncated=true")
	}
	if got == "Short o.…" {
		t.Fatalf("early sentence boundary must not win: got %q", got)
	}
	if r := len([]rune(got)); r <= 30 {
		t.Fatalf("cut wasted the window: %d runes, want a near-cap word cut", r)
	}
	if strings.Contains(strings.TrimSuffix(got, "…"), "wor…") {
		t.Fatalf("cut landed mid-word: %q", got)
	}
}

// TestConciseRuneSafetyMultiByte pins that the cap, the cut, and the
// truncation decision all count runes, never bytes: multi-byte content under
// the rune cap passes verbatim, over-cap multi-byte content cuts on a rune
// boundary (no split UTF-8 sequence, no spurious ellipsis).
func TestConciseRuneSafetyMultiByte(t *testing.T) {
	// 200 runes / 600 bytes: under the rune cap → verbatim.
	under := strings.Repeat("憶", 200)
	if got, truncated := render.Concise(under, "", 240); got != under || truncated {
		t.Fatalf("under-cap multibyte: got (%d runes, %v), want verbatim", len([]rune(got)), truncated)
	}

	// 300 runes with no spaces: hard cut at exactly 240 runes + ellipsis.
	over := strings.Repeat("記", 300)
	if got, truncated := render.Concise(over, "", 240); got != strings.Repeat("記", 240)+"…" || !truncated {
		t.Fatalf("over-cap multibyte: got %d runes, truncated=%v", len([]rune(got)), truncated)
	}

	// Multi-byte words separated by spaces: the cut lands on a space, so the
	// kept text is whole words only.
	spaced := strings.TrimSpace(strings.Repeat("記憶 ", 100)) // words of 2 runes
	got, truncated := render.Concise(spaced, "", 7)
	if got != "記憶 記憶…" || !truncated {
		t.Fatalf("multibyte word cut: got (%q, %v), want (\"記憶 記憶…\", true)", got, truncated)
	}
}

// TestContentHash pins the wire dedupe identity recipe shared with the plugin
// client (plugin/scripts/_shared.mjs injectedIdentity): sha256 over the FULL
// stored content — falling back to the summary only when content is empty —
// rendered as the first 16 lowercase hex chars.
func TestContentHash(t *testing.T) {
	// Known vector so the recipe (not just self-consistency) is pinned.
	if got := render.ContentHash("hello", ""); got != "2cf24dba5fb0a30e" {
		t.Fatalf("ContentHash(\"hello\") = %q, want \"2cf24dba5fb0a30e\"", got)
	}

	// Content wins even when a summary exists — identity must be computed over
	// the full stored content, never the concise/summary text.
	withSummary := render.ContentHash("full stored content", "a summary")
	contentOnly := render.ContentHash("full stored content", "")
	if withSummary != contentOnly {
		t.Fatalf("summary leaked into the hash: %q vs %q", withSummary, contentOnly)
	}

	// Empty content falls back to the summary.
	sum := sha256.Sum256([]byte("a summary"))
	if got, want := render.ContentHash("", "a summary"), hex.EncodeToString(sum[:])[:16]; got != want {
		t.Fatalf("summary fallback: got %q, want %q", got, want)
	}

	if got := render.ContentHash("x", ""); len(got) != 16 || strings.ToLower(got) != got {
		t.Fatalf("hash must be 16 lowercase hex chars, got %q", got)
	}
}

// TestTitle pins the child-rollup title derivation: summary verbatim, else a
// boundary cut at the 60-rune title cap.
func TestTitle(t *testing.T) {
	if got := render.Title("content", "the summary"); got != "the summary" {
		t.Fatalf("Title with summary = %q", got)
	}
	long := strings.Repeat("word ", 30)
	got := render.Title(long, "")
	if r := len([]rune(got)); r > render.TitleMax+1 { // +1 for the ellipsis
		t.Fatalf("title too long: %d runes (%q)", r, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("cut title must end with ellipsis: %q", got)
	}
	if strings.Contains(got, "wor…") {
		t.Fatalf("title cut mid-word: %q", got)
	}
}
