package importer

import (
	"strings"
	"testing"

	"github.com/eleboucher/memini/internal/memory"
)

func TestExtractTypedClassifies(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want TypedKind
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
			got := extractTyped(tc.text)
			if len(got) != 1 {
				t.Fatalf("got %d extractions, want 1: %+v", len(got), got)
			}
			if got[0].kind != tc.want {
				t.Errorf("kind = %q, want %q", got[0].kind, tc.want)
			}
		})
	}
}

func TestExtractTypedSkipsNoise(t *testing.T) {
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
			if got := extractTyped(tc.text); len(got) != 0 {
				t.Errorf("expected no extractions, got %+v", got)
			}
		})
	}
}

func TestExtractTypedCapsPerExchange(t *testing.T) {
	one := "We decided to use a queue because it decouples the writers, instead of synchronous calls.\n\n"
	got := extractTyped(strings.Repeat(one, 10))
	if len(got) > extractMaxPerExchange {
		t.Fatalf("got %d extractions, want <= %d", len(got), extractMaxPerExchange)
	}
}

func TestExtractTypedRecords(t *testing.T) {
	src := []Record{{
		Namespace: "proj",
		Tier:      memory.TierEpisodic,
		Content:   "user: which db?\nassistant: We decided to use Postgres instead of SQLite because we need concurrent writes.",
		Metadata:  map[string]any{"session_id": "s1"},
	}}
	out := ExtractTyped(src)
	if len(out) != 1 {
		t.Fatalf("got %d records, want 1", len(out))
	}
	r := out[0]
	if r.Tier != memory.TierSemantic {
		t.Errorf("tier = %q, want semantic", r.Tier)
	}
	if len(r.Tags) != 1 || r.Tags[0] != string(KindDecision) {
		t.Errorf("tags = %v, want [decision]", r.Tags)
	}
	if r.Namespace != "proj" {
		t.Errorf("namespace = %q, want proj (inherited)", r.Namespace)
	}
	if r.Metadata["memory_type"] != "decision" || r.Metadata["session_id"] != "s1" {
		t.Errorf("metadata not carried/stamped: %+v", r.Metadata)
	}
}

// TestExtractTypedRecordsSkipsNonEpisodic guards against re-extracting from a
// memini re-import: only untyped episodic records are scanned, so durable or
// already-typed records don't get reclassified into duplicates.
func TestExtractTypedRecordsSkipsNonEpisodic(t *testing.T) {
	recs := []Record{
		{Tier: memory.TierSemantic, Content: "We decided to use Postgres instead of SQLite."},
		{Tier: memory.TierEpisodic, Content: "We decided to use Redis instead of Memcached.", Metadata: map[string]any{"memory_type": "decision"}},
		{Tier: memory.TierEpisodic, Content: "user: cache?\nassistant: We decided to use Redis instead of Memcached for sessions."},
	}
	out := ExtractTyped(recs)
	if len(out) != 1 {
		t.Fatalf("got %d extractions, want 1 (only the untyped episodic record)", len(out))
	}
}
