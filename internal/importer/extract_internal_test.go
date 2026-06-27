package importer

import (
	"testing"

	"github.com/eleboucher/memini/internal/extract"
	"github.com/eleboucher/memini/internal/memory"
)

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
	if len(r.Tags) != 1 || r.Tags[0] != string(extract.KindDecision) {
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
