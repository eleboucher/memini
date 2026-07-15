package extract

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/eleboucher/memini/internal/memory"
)

func TestTypedClassifies(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want Kind
	}{
		{"decision with rationale",
			"We decided to use Postgres instead of SQLite because we need concurrent writes from multiple replicas.",
			KindDecision},
		{"stated preference",
			"My convention is: always use snake_case for table columns. Please never use camelCase here.",
			KindPreference},
		{"problem and fix",
			"The bug was a nil deref in the auth middleware. The fix was to guard the token lookup before dereferencing it.",
			KindProblem},
		{"architecture fact",
			"The store uses sqlite-vec for hybrid vector search because it keeps the index in the same SQLite database as the metadata. By default the vector index is built at write time.",
			KindFact},
		{"configuration fact",
			"By default the server listens on :8080, but you can override it with the MEMINI_HTTP_ADDR environment variable.",
			KindFact},
		{"how-to instruction",
			"To configure the echo guard, you need to set MEMINI_TURN_ECHO_WINDOW to a duration like 5m. Then you must restart the server for the change to take effect.",
			KindHowTo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Typed(tc.text)
			if len(got) != 1 {
				t.Fatalf("got %d extractions, want 1: %+v", len(got), got)
			}
			if got[0].Kind != tc.want {
				t.Errorf("kind = %q, want %q", got[0].Kind, tc.want)
			}
		})
	}
}

func TestTypedSkipsNoise(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{"too short", "ok thanks"},
		{"plain chatter, no markers", "Here is the file you asked about. Let me know what you think of it overall."},
		{"topic words are not decisions", "Let me walk through the architecture and the general approach and strategy in this module."},
		{"single repeated marker word doesn't inflate score", "Hit a bug, then another bug, then a third bug, bugs everywhere today."},
		{"code only", "```go\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n```"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Typed(tc.text); len(got) != 0 {
				t.Errorf("expected no extractions, got %+v", got)
			}
		})
	}
}

func TestTypedCapsPerExchange(t *testing.T) {
	one := "We decided to use a queue because it decouples the writers, instead of synchronous calls.\n\n"
	got := Typed(strings.Repeat(one, 10))
	if len(got) > MaxPerExchange {
		t.Fatalf("got %d extractions, want <= %d", len(got), MaxPerExchange)
	}
}

// TestKindTier checks the kind→tier mapping: preferences are procedural how-to,
// decisions and problems are semantic facts.
func TestKindTier(t *testing.T) {
	for _, tc := range []struct {
		kind Kind
		want memory.Tier
	}{
		{KindPreference, memory.TierProcedural},
		{KindDecision, memory.TierSemantic},
		{KindProblem, memory.TierSemantic},
		{KindFact, memory.TierSemantic},
		{KindHowTo, memory.TierProcedural},
	} {
		if got := tc.kind.Tier(); got != tc.want {
			t.Errorf("%s.Tier() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// asciiOfLen builds pure-ASCII prose of exactly n runes carrying two distinct
// "problem" markers ("bug", "not working"), so it scores 2 and clears
// MinConfidence at any length — leaving the length gates as the only thing that
// can reject it. ASCII so len(s) == RuneCountInString(s) and the case pins the
// same boundary under either counting.
func asciiOfLen(t *testing.T, n int) string {
	t.Helper()
	const base = "bug not working " // 16 runes, both markers
	if n < len(base) {
		t.Fatalf("asciiOfLen(%d): shorter than the %d-rune marker base", n, len(base))
	}
	s := base + strings.Repeat("x", n-len(base))
	if got := utf8.RuneCountInString(s); got != n {
		t.Fatalf("asciiOfLen(%d) produced %d runes", n, got)
	}
	if len(s) != n {
		t.Fatalf("asciiOfLen(%d) is not pure ASCII: %d bytes", n, len(s))
	}
	return s
}

func TestClassify(t *testing.T) {
	type classifyCase struct {
		name string
		text string
		want Kind
		ok   bool
	}
	// For ASCII, bytes and runes agree, so switching the gates to runes must not
	// have moved either boundary by even one character. Pinned exactly on both
	// sides: the whole claim of the rune fix is that ASCII is untouched.
	tests := []classifyCase{
		{"ascii at the floor", asciiOfLen(t, MinFactChars), KindProblem, true},
		{"ascii one under the floor", asciiOfLen(t, MinFactChars-1), "", false},
		{"ascii at the ceiling", asciiOfLen(t, ClassifyMaxChars), KindProblem, true},
		{"ascii one over the ceiling", asciiOfLen(t, ClassifyMaxChars+1), "", false},

		{"decision", "We decided to use Postgres instead of MySQL for the vector store.", KindDecision, true},
		{"preference", "I prefer table-driven tests, please always use them instead of ad-hoc asserts.", KindPreference, true},
		{"problem fix", "The bug was a nil map write; the fix was initializing metadata in the constructor.", KindProblem, true},
		{"hedged decision", "Maybe we should go with Postgres instead of MySQL.", "", false},
		{"tentative", "I think the reason is the cache, not sure though.", "", false},
		{"transcript", "User: which db?\nAssistant: we decided to use postgres instead of mysql", "", false},
		{"no markers", "Met with the platform team about quarterly planning this afternoon.", "", false},
		{"single weak marker", "I prefer tabs.", "", false},
		{"too short", "use postgres", "", false},
		{"too long", strings.Repeat("we decided to go with postgres instead of mysql. ", 20), "", false},
		// Non-ASCII prose is counted in runes, not bytes: this is ~210 runes but
		// ~534 bytes, so a byte-based ceiling would silently refuse to classify it
		// and drop the write into the short-lived working tier.
		{"cjk under the rune ceiling, over a byte ceiling",
			"We decided to go with Postgres instead of MySQL. " +
				strings.Repeat("这个部署流水线在十六个核心上并行运行测试以最小化延迟。", 6),
			KindDecision, true},
		// The floor is runes too, and that cuts the other way: 18 runes but 22
		// bytes. A byte floor let this through while rejecting the same-length
		// ASCII text, so two markers on a scrap of prose became a durable fact.
		{"cjk under the rune floor, over a byte floor", "bug not working 是的", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, ok := Classify(tc.text)
			if ok != tc.ok || kind != tc.want {
				t.Fatalf("Classify(%q) = (%q, %v), want (%q, %v)", tc.text, kind, ok, tc.want, tc.ok)
			}
		})
	}
}
