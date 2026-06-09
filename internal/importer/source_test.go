package importer

import (
	"testing"

	"github.com/eleboucher/memini/internal/memory"
)

func TestParseAgentMemory(t *testing.T) {
	data := []byte(`{"version":"1.0","memories":[
		{"id":"a1","type":"workflow","title":"Deploy","content":"run mise release",
		 "concepts":["ci","deploy"],"strength":0.8,"project":"memini",
		 "createdAt":"2026-01-02T03:04:05Z","updatedAt":"2026-01-03T00:00:00Z"},
		{"id":"a2","type":"fact","content":"user likes Go","project":"memini","strength":1.4}
	]}`)
	recs, err := Parse(SourceAgentMemory, data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	r := recs[0]
	if r.ID != "a1" || r.Namespace != "memini" || r.Tier != memory.TierProcedural {
		t.Errorf("rec0 mapping wrong: %+v", r)
	}
	if r.Summary != "Deploy" || r.Content != "run mise release" || len(r.Tags) != 2 {
		t.Errorf("rec0 fields wrong: %+v", r)
	}
	if r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() {
		t.Errorf("rec0 timestamps not parsed: %+v", r)
	}
	if recs[1].Tier != memory.TierSemantic {
		t.Errorf("fact should map to semantic, got %s", recs[1].Tier)
	}
}

func TestParseMem0(t *testing.T) {
	// Bare array (get_all results), session scope in metadata.
	data := []byte(`[
		{"id":"m1","memory":"prefers dark mode","created_at":"2026-05-01T10:00:00Z",
		 "metadata":{"user_id":"alice","categories":["ui","preference"]}},
		{"id":"m2","memory":"works at OVH","user_id":"bob"}
	]`)
	recs, err := Parse(SourceMem0, data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if recs[0].Namespace != "alice" || recs[0].Tier != memory.TierSemantic {
		t.Errorf("rec0 mapping wrong: %+v", recs[0])
	}
	if len(recs[0].Tags) != 2 {
		t.Errorf("rec0 categories→tags wrong: %+v", recs[0].Tags)
	}
	if recs[1].Namespace != "bob" {
		t.Errorf("rec1 top-level user_id→namespace wrong: %q", recs[1].Namespace)
	}
}

func TestParseMnemory(t *testing.T) {
	data := []byte(`{"memories":[
		{"id":"x1","content":"deploy via argocd","memory_type":"procedural",
		 "tags":["ops"],"importance":"high","tenant":"acme","ttl_days":30,
		 "created_at":"2026-03-01T00:00:00Z"},
		{"id":"x2","memory":"meeting happened","memory_type":"episodic","namespace":"acme"}
	]}`)
	recs, err := Parse(SourceMnemory, data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if recs[0].Tier != memory.TierProcedural || recs[0].Namespace != "acme" {
		t.Errorf("rec0 mapping wrong: %+v", recs[0])
	}
	if recs[0].Importance != 0.9 {
		t.Errorf("high importance want 0.9, got %v", recs[0].Importance)
	}
	if recs[0].ExpiresAt == nil {
		t.Errorf("ttl_days should yield expiry")
	}
	if recs[1].Content != "meeting happened" || recs[1].Tier != memory.TierEpisodic {
		t.Errorf("rec1 mapping wrong: %+v", recs[1])
	}
}

func TestParseUnknownSource(t *testing.T) {
	if _, err := Parse(Source("bogus"), []byte(`[]`)); err == nil {
		t.Fatal("expected error for unknown source")
	}
}
