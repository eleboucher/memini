package extract

import (
	"strings"
	"testing"

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
	} {
		if got := tc.kind.Tier(); got != tc.want {
			t.Errorf("%s.Tier() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}
