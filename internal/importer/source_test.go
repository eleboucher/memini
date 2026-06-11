package importer

import (
	"strings"
	"testing"
	"time"

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

func TestParseClaudeCode(t *testing.T) {
	// A transcript with: a real exchange, a thinking-only assistant turn (must
	// be skipped), a sidechain user, an isMeta user, a tool_result array user,
	// command-noise, and a multi-record assistant turn.
	lines := []string{
		`{"type":"user","isMeta":true,"message":{"content":"<system reminder>"}}`,
		`{"type":"user","cwd":"/home/me/myapp","sessionId":"sess-1","gitBranch":"main","timestamp":"2026-01-02T03:04:05Z","message":{"content":"how do I add auth?"}}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Use middleware."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Add it in router.go."}]}}`,
		`{"type":"user","isSidechain":true,"message":{"content":"sidechain noise"}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"x"}]}}`,
		`{"type":"user","cwd":"/home/me/myapp","sessionId":"sess-1","timestamp":"2026-01-02T03:05:00Z","message":{"content":"<local-command-caveat>noise"}}`,
		`{"type":"user","cwd":"/home/me/myapp","sessionId":"sess-1","timestamp":"2026-01-02T03:06:00Z","message":{"content":"thanks"}}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"done"}]}}`,
	}
	data := []byte(strings.Join(lines, "\n") + "\n")

	recs, err := Parse(SourceClaudeCode, data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Only the first exchange has assistant text; the "thanks" turn has a
	// thinking-only reply and is skipped.
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}
	r := recs[0]
	if r.ID != "cc:sess-1:0000" {
		t.Errorf("ID = %q, want cc:sess-1:0000", r.ID)
	}
	if r.Namespace != "myapp" {
		t.Errorf("Namespace = %q, want myapp (cwd basename)", r.Namespace)
	}
	if r.Tier != memory.TierEpisodic {
		t.Errorf("Tier = %q, want episodic", r.Tier)
	}
	if !strings.Contains(r.Content, "user: how do I add auth?") ||
		!strings.Contains(r.Content, "Use middleware.\nAdd it in router.go.") {
		t.Errorf("Content wrong:\n%s", r.Content)
	}
	if r.Metadata["session_id"] != "sess-1" || r.Metadata["source"] != "claude-code" {
		t.Errorf("Metadata wrong: %+v", r.Metadata)
	}
	if r.ExpiresAt == nil || !r.ExpiresAt.After(time.Now()) {
		t.Errorf("ExpiresAt should be in the future (not expired-on-arrival), got %v", r.ExpiresAt)
	}
	if r.CreatedAt.IsZero() {
		t.Error("CreatedAt should carry the transcript timestamp")
	}
}

func TestParseClaudeCodeIdempotentIDs(t *testing.T) {
	line := `{"type":"user","sessionId":"s","cwd":"/x","message":{"content":"q%d"}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"a%d"}]}}`
	var b strings.Builder
	for range 3 {
		b.WriteString(strings.ReplaceAll(line, "%d", "x"))
		b.WriteString("\n")
	}
	recs, err := Parse(SourceClaudeCode, []byte(b.String()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
	for i, want := range []string{"cc:s:0000", "cc:s:0001", "cc:s:0002"} {
		if recs[i].ID != want {
			t.Errorf("rec %d ID = %q, want %q", i, recs[i].ID, want)
		}
	}
}
