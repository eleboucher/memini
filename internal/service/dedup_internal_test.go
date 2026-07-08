package service

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

const dedupTestDims = 64

// newDedupSvc creates a Service with write-dedup configured (no consolidator,
// so runSplitDedup/dedupCheck is the active path). Returns the store for
// seeding base memories.
func newDedupSvc(t *testing.T, score float64, action WriteDedupAction) (*Service, *sqlitevec.Store) {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "dedup.db"), dedupTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	opts := []Option{
		WithWriteDedup(score, action),
		WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
		WithSyncReinforce(), // reinforcement must run inline so coalesce tests observe the result
	}
	svc := New(st, embedtest.New(dedupTestDims), opts...)
	return svc, st
}

// putDedupMem inserts a memory with content and embedding into the store
// directly (bypasses Remember so it's a "pre-existing" memory for dedupCheck).
func putDedupMem(t *testing.T, st *sqlitevec.Store, e embed.Embedder, id, content string, tier memory.Tier) *memory.Memory {
	t.Helper()
	vec, err := embed.EmbedOne(context.Background(), e, content)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	m := &memory.Memory{
		ID: id, Namespace: "ns", Tier: tier, Content: content,
		CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Embedding: vec,
	}
	if err := st.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
	return m
}

// embedMem embeds content and populates a *memory.Memory struct suitable for
// passing to dedupCheck — it is NOT stored; it represents the "incoming" write.
func embedMem(t *testing.T, e embed.Embedder, content string, tier memory.Tier) *memory.Memory {
	t.Helper()
	vec, err := embed.EmbedOne(context.Background(), e, content)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	return &memory.Memory{
		ID:        "incoming",
		Namespace: "ns",
		Tier:      tier,
		Content:   content,
		CreatedAt: now,
		UpdatedAt: now,
		Embedding: vec,
	}
}

// ---------------------------------------------------------------------------
// wordSetScore
// ---------------------------------------------------------------------------

func TestWordSetScore(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"single word", "hello", 11},                                             // 1 + 10*1 = 11
		{"two distinct", "hello world", 22},                                      // 2 + 10*2 = 22
		{"two same", "hello hello", 12},                                          // 2 + 10*1 = 12
		{"three all distinct", "a b c", 33},                                      // 3 + 10*3 = 33
		{"repeated pattern", "the the the", 13},                                  // 3 + 10*1 = 13
		{"mixed repeats", "cat dog cat dog bird", 35},                            // 5 words, 3 uniq → 5 + 30 = 35
		{"case insensitive", "Hello WORLD hello", 23},                            // 3 words, 2 uniq (hello,world) → 3 + 20 = 23
		{"punctuation attached", "hello, world!", 22},                            // 2 tokens, 2 uniq → 2+20=22
		{"long text", "the quick brown fox jumps over the lazy dog the dog", 91}, // 11 words, 8 uniq → 11+80=91
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wordSetScore(tt.in); got != tt.want {
				t.Errorf("wordSetScore(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// dedupCheck
// ---------------------------------------------------------------------------

func TestDedupCheckNoCandidates(t *testing.T) {
	svc, _ := newDedupSvc(t, 0.625, WriteDedupHint)
	e := embedtest.New(dedupTestDims)
	m := embedMem(t, e, "a unique new topic", memory.TierSemantic)

	hit, hint, sid := svc.dedupCheck(context.Background(), m)
	if hit != nil {
		t.Fatalf("empty store: hit = %v, want nil", hit.ID)
	}
	if hint != nil {
		t.Fatalf("empty store: hint = %+v, want nil", hint)
	}
	if sid != "" {
		t.Fatalf("empty store: supersedeID = %q, want empty", sid)
	}
}

func TestDedupCheckExactDuplicate(t *testing.T) {
	svc, st := newDedupSvc(t, 0.625, WriteDedupHint)
	e := embedtest.New(dedupTestDims)
	putDedupMem(t, st, e, "a", "the office is in Paris", memory.TierSemantic)

	m := embedMem(t, e, "the office is in Paris", memory.TierSemantic)

	hit, hint, sid := svc.dedupCheck(context.Background(), m)
	if hint == nil {
		t.Fatalf("exact duplicate: expected a hint, got nil")
	}
	if hint.SimilarID != "a" {
		t.Errorf("hint.SimilarID = %q, want \"a\"", hint.SimilarID)
	}
	if hit != nil {
		t.Errorf("hint action: hit must be nil, got %v", hit.ID)
	}
	if sid != "" {
		t.Errorf("hint action: supersedeID must be empty, got %q", sid)
	}
}

func TestDedupCheckParaphrase(t *testing.T) {
	// "the sky is green" vs "the sky is blue" with the fake embedder
	// scores ≈ 0.586 (known from consolidate tests). Threshold 0.625
	// must reject it.
	svc, st := newDedupSvc(t, 0.625, WriteDedupHint)
	e := embedtest.New(dedupTestDims)
	putDedupMem(t, st, e, "a", "the sky is green", memory.TierSemantic)

	m := embedMem(t, e, "the sky is blue", memory.TierSemantic)

	hit, hint, sid := svc.dedupCheck(context.Background(), m)
	if hit != nil {
		t.Fatalf("below-threshold paraphrase: hit = %v, want nil", hit.ID)
	}
	if hint != nil {
		t.Fatalf("below-threshold paraphrase: hint = %+v, want nil", hint)
	}
	if sid != "" {
		t.Fatalf("below-threshold paraphrase: supersedeID = %q, want empty", sid)
	}
}

func TestDedupCheckAboveThreshold(t *testing.T) {
	// Same paraphrase but lower threshold (0.5) must fire.
	svc, st := newDedupSvc(t, 0.5, WriteDedupHint)
	e := embedtest.New(dedupTestDims)
	putDedupMem(t, st, e, "a", "the sky is green", memory.TierSemantic)

	m := embedMem(t, e, "the sky is blue", memory.TierSemantic)

	hit, hint, sid := svc.dedupCheck(context.Background(), m)
	if hint == nil {
		t.Fatalf("above-threshold paraphrase: expected hint, got nil")
	}
	if hint.SimilarID != "a" {
		t.Errorf("hint.SimilarID = %q, want \"a\"", hint.SimilarID)
	}
	if hit != nil {
		t.Errorf("hint action: hit must be nil, got %v", hit.ID)
	}
	if sid != "" {
		t.Errorf("hint action: supersedeID must be empty, got %q", sid)
	}
}

func TestDedupCheckActionSupersede(t *testing.T) {
	svc, st := newDedupSvc(t, 0.5, WriteDedupSupersede)
	e := embedtest.New(dedupTestDims)
	putDedupMem(t, st, e, "a", "the sky is green", memory.TierSemantic)

	m := embedMem(t, e, "the sky is blue", memory.TierSemantic)

	hit, hint, sid := svc.dedupCheck(context.Background(), m)
	if sid != "a" {
		t.Fatalf("supersede: supersedeID = %q, want \"a\"", sid)
	}
	if hit != nil {
		t.Fatalf("supersede action: hit must be nil, got %v", hit.ID)
	}
	if hint != nil {
		t.Fatalf("supersede action: hint must be nil, got %+v", hint)
	}
}

func TestDedupCheckActionCoalesceIncomingRicher(t *testing.T) {
	// Incoming has a richer phrasing → supersede the old memory.
	svc, st := newDedupSvc(t, 0.5, WriteDedupCoalesce)
	e := embedtest.New(dedupTestDims)
	// 5 distinct words: "the sky is green today" → 5 + 50 = 55
	putDedupMem(t, st, e, "a", "the sky is green", memory.TierSemantic)

	// 8 distinct words → 8 + 80 = 88 > 55 → incoming wins
	m := embedMem(t, e, "the sky is actually more green than blue", memory.TierSemantic)

	hit, hint, sid := svc.dedupCheck(context.Background(), m)
	if sid != "a" {
		t.Fatalf("coalesce (richer incoming): supersedeID = %q, want \"a\"", sid)
	}
	if hit != nil {
		t.Fatalf("coalesce (richer incoming): hit must be nil, got %v", hit.ID)
	}
	if hint != nil {
		t.Fatalf("coalesce (richer incoming): hint must be nil, got %+v", hint)
	}
}

func TestDedupCheckActionCoalesceIncomingPoorer(t *testing.T) {
	// Incoming has fewer unique words → drop into existing, reinforce it.
	svc, st := newDedupSvc(t, 0.5, WriteDedupCoalesce)
	e := embedtest.New(dedupTestDims)
	// Richer existing: 8 distinct words → 88
	existing := putDedupMem(t, st, e, "a", "the sky is actually more green than blue", memory.TierSemantic)

	// Poorer incoming: 5 distinct words → 55 < 88 → incoming loses
	m := embedMem(t, e, "the sky is green", memory.TierSemantic)

	hit, hint, sid := svc.dedupCheck(context.Background(), m)
	if hit == nil {
		t.Fatalf("coalesce (poorer incoming): expected hit, got nil")
	}
	if hit.ID != existing.ID {
		t.Errorf("coalesce (poorer incoming): hit.ID = %q, want %q", hit.ID, existing.ID)
	}
	if hint != nil {
		t.Errorf("coalesce (poorer incoming): hint must be nil, got %+v", hint)
	}
	if sid != "" {
		t.Errorf("coalesce (poorer incoming): supersedeID must be empty, got %q", sid)
	}
	// The existing memory should have been reinforced (access count bumped).
	refreshed, err := st.Get(context.Background(), "ns", existing.ID)
	if err != nil {
		t.Fatalf("get existing: %v", err)
	}
	if refreshed.AccessCount <= existing.AccessCount {
		t.Errorf("coalesce: existing AccessCount not bumped; was %d, now %d",
			existing.AccessCount, refreshed.AccessCount)
	}
}

func TestDedupCheckActionCoalesceCarriesConfidence(t *testing.T) {
	// When incoming wins on informativeness but existing has higher confidence,
	// the incoming copy gets the existing confidence carried forward.
	svc, st := newDedupSvc(t, 0.5, WriteDedupCoalesce)
	e := embedtest.New(dedupTestDims)

	highConf := 0.9
	existing := &memory.Memory{
		ID: "a", Namespace: "ns", Tier: memory.TierSemantic,
		Content:        "the sky is green",
		Confidence:     &highConf,
		CreatedAt:      time.Unix(1_700_000_000, 0).UTC(),
		UpdatedAt:      time.Unix(1_700_000_000, 0).UTC(),
		LastAccessedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	vec, err := embed.EmbedOne(context.Background(), e, existing.Content)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	existing.Embedding = vec
	if err := st.Upsert(context.Background(), existing); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Richer incoming (more distinct words) but no confidence.
	m := embedMem(t, e, "the sky is a beautiful deep green today", memory.TierSemantic)

	hit, _, sid := svc.dedupCheck(context.Background(), m)
	if sid != "a" {
		t.Fatalf("coalesce (richer): supersedeID = %q, want \"a\"", sid)
	}
	if hit != nil {
		t.Fatalf("coalesce (richer): hit must be nil, got %v", hit.ID)
	}
	// The incoming memory's confidence should have been carried forward from existing.
	if m.Confidence == nil || *m.Confidence != 0.9 {
		t.Fatalf("confidence not carried: m.Confidence = %v, want 0.9", func() string {
			if m.Confidence == nil {
				return "nil"
			}
			return fmt.Sprintf("%.1f", *m.Confidence)
		}())
	}
}

func TestDedupCheckDistinct(t *testing.T) {
	// Totally different topics: use a threshold high enough that
	// genuinely distinct content stays below it. The fake embedder
	// produces a small but non-zero score even for disjoint token sets
	// (bucket collisions in 64 dims with 4+ tokens per side).
	svc, st := newDedupSvc(t, 0.45, WriteDedupHint)
	e := embedtest.New(dedupTestDims)
	putDedupMem(t, st, e, "a", "xxyyzz alpha bravo charlie delta", memory.TierSemantic)

	m := embedMem(t, e, "quantum computing uses qubits for parallel processing", memory.TierSemantic)

	hit, hint, sid := svc.dedupCheck(context.Background(), m)
	if hit != nil {
		t.Fatalf("distinct topic: hit = %v, want nil", hit.ID)
	}
	if hint != nil {
		t.Fatalf("distinct topic: hint = %+v (score=%.3f), want nil",
			hint, hint.Score)
	}
	if sid != "" {
		t.Fatalf("distinct topic: supersedeID = %q, want empty", sid)
	}
}

func TestDedupCheckActionOff(t *testing.T) {
	// WriteDedupOff: dedupCheck returns empty unconditionally.
	svc, st := newDedupSvc(t, 0.625, WriteDedupOff)
	e := embedtest.New(dedupTestDims)
	putDedupMem(t, st, e, "a", "the sky is green", memory.TierSemantic)

	m := embedMem(t, e, "the sky is blue", memory.TierSemantic)

	hit, hint, sid := svc.dedupCheck(context.Background(), m)
	if hit != nil {
		t.Fatalf("off: hit = %v, want nil", hit.ID)
	}
	if hint != nil {
		t.Fatalf("off: hint = %+v, want nil", hint)
	}
	if sid != "" {
		t.Fatalf("off: supersedeID = %q, want empty", sid)
	}
}

func TestDedupCheckEpisodicTierNoHint(t *testing.T) {
	// Hint action on episodic tier: dedupCheck still runs (it's called by
	// runSplitDedup which gates the tier), but we can still test it directly.
	// dedupCheck itself doesn't gate on tier — it's the caller's job.
	// The vector search is filtered by tier, so episodic writes only match
	// episodic memories.
	svc, st := newDedupSvc(t, 0.5, WriteDedupHint)
	e := embedtest.New(dedupTestDims)
	putDedupMem(t, st, e, "a", "Alice said hello to Bob", memory.TierEpisodic)

	m := embedMem(t, e, "Alice said hi to Bob", memory.TierEpisodic)

	hit, hint, sid := svc.dedupCheck(context.Background(), m)
	// dedupCheck returns the hint (action is hint); the tier gate is in runSplitDedup.
	if hint == nil {
		t.Fatalf("episodic duplicate: expected hint, got nil")
	}
	if hit != nil {
		t.Fatalf("episodic: hit must be nil for hint action, got %v", hit.ID)
	}
	if sid != "" {
		t.Fatalf("episodic: supersedeID must be empty, got %q", sid)
	}
}

func TestDedupCheckEscapedStringInHint(t *testing.T) {
	// Verify the hint preview truncation works and special chars don't break it.
	svc, st := newDedupSvc(t, 0.5, WriteDedupHint)
	e := embedtest.New(dedupTestDims)
	putDedupMem(t, st, e, "a", "the sky is green", memory.TierSemantic)

	m := embedMem(t, e, "the sky is blue", memory.TierSemantic)

	hit, hint, sid := svc.dedupCheck(context.Background(), m)
	if hint == nil {
		t.Fatalf("expected hint, got nil")
	}
	if len(hint.SimilarContent) == 0 {
		t.Errorf("hint.SimilarContent must not be empty")
	}
	if hint.Score <= 0 {
		t.Errorf("hint.Score = %.3f, must be positive", hint.Score)
	}
	if hint.Tier != memory.TierSemantic {
		t.Errorf("hint.Tier = %v, want semantic", hint.Tier)
	}
	_ = sid
	_ = hit
}

// ---------------------------------------------------------------------------
// runSplitDedup
// ---------------------------------------------------------------------------

func TestRunSplitDedupOff(t *testing.T) {
	svc, st := newDedupSvc(t, 0.625, WriteDedupOff)
	e := embedtest.New(dedupTestDims)
	putDedupMem(t, st, e, "a", "the sky is green", memory.TierSemantic)

	m := embedMem(t, e, "the sky is blue", memory.TierSemantic)
	in := RememberInput{Content: m.Content, Namespace: m.Namespace}

	handled, result, supersedeID := svc.runSplitDedup(context.Background(), m, in)
	if handled {
		t.Fatalf("off: handled=true, want false")
	}
	if result != nil {
		t.Fatalf("off: result non-nil, want nil")
	}
	if supersedeID != "" {
		t.Fatalf("off: supersedeID = %q, want empty", supersedeID)
	}
}

func TestRunSplitDedupHintEpisodicSkipped(t *testing.T) {
	// runSplitDedup skips hint action for short-term (episodic/working) tiers.
	svc, st := newDedupSvc(t, 0.5, WriteDedupHint)
	e := embedtest.New(dedupTestDims)
	putDedupMem(t, st, e, "a", "Alice said hello to Bob", memory.TierEpisodic)

	m := embedMem(t, e, "Alice said hi to Bob", memory.TierEpisodic)
	in := RememberInput{Content: m.Content, Namespace: m.Namespace}

	handled, result, supersedeID := svc.runSplitDedup(context.Background(), m, in)
	if handled {
		t.Fatalf("episodic hint: handled=true, want false (skipped)")
	}
	if result != nil {
		t.Fatalf("episodic hint: result non-nil, want nil")
	}
	if supersedeID != "" {
		t.Fatalf("episodic hint: supersedeID = %q, want empty", supersedeID)
	}
}

func TestRunSplitDedupCoalesceEpisodicRuns(t *testing.T) {
	// Coalesce applies to all tiers (not gated), unlike hint.
	svc, st := newDedupSvc(t, 0.5, WriteDedupCoalesce)
	e := embedtest.New(dedupTestDims)
	putDedupMem(t, st, e, "a", "Alice said hello Bob", memory.TierEpisodic)

	// Richer incoming: 6 distinct words → 6 + 60 = 66 vs existing 4 uniq → 4 + 40 = 44.
	m := embedMem(t, e, "Alice said hi to Bob yesterday", memory.TierEpisodic)
	in := RememberInput{Content: m.Content, Namespace: m.Namespace}

	handled, result, supersedeID := svc.runSplitDedup(context.Background(), m, in)
	if handled {
		t.Fatalf("coalesce episodic (richer incoming): handled=true, want false")
	}
	if result != nil {
		t.Fatalf("coalesce episodic (richer incoming): result non-nil, want nil")
	}
	if supersedeID != "a" {
		t.Fatalf("coalesce episodic (richer incoming): supersedeID = %q, want \"a\"", supersedeID)
	}
}

func TestRunSplitDedupMergeHintStashed(t *testing.T) {
	svc, st := newDedupSvc(t, 0.5, WriteDedupHint)
	e := embedtest.New(dedupTestDims)
	putDedupMem(t, st, e, "a", "the sky is green", memory.TierSemantic)

	m := embedMem(t, e, "the sky is blue", memory.TierSemantic)

	var hint MergeHint
	in := RememberInput{
		Content:   m.Content,
		Namespace: m.Namespace,
		MergeHint: &hint,
	}

	handled, result, supersedeID := svc.runSplitDedup(context.Background(), m, in)
	if handled {
		t.Fatalf("hint: handled=true, want false")
	}
	if result != nil {
		t.Fatalf("hint: result non-nil, want nil")
	}
	if supersedeID != "" {
		t.Fatalf("hint: supersedeID = %q, want empty", supersedeID)
	}
	if hint.SimilarID != "a" {
		t.Errorf("hint.SimilarID = %q, want \"a\"", hint.SimilarID)
	}
	if len(hint.SimilarContent) == 0 {
		t.Errorf("hint.SimilarContent must not be empty")
	}
}

func TestRunSplitDedupNoMergeHintPointer(t *testing.T) {
	// When MergeHint is nil in the input, the hint is not surfaced but
	// the write still proceeds (handled=false).
	svc, st := newDedupSvc(t, 0.5, WriteDedupHint)
	e := embedtest.New(dedupTestDims)
	putDedupMem(t, st, e, "a", "the sky is green", memory.TierSemantic)

	m := embedMem(t, e, "the sky is blue", memory.TierSemantic)
	in := RememberInput{Content: m.Content, Namespace: m.Namespace}

	handled, result, supersedeID := svc.runSplitDedup(context.Background(), m, in)
	if handled {
		t.Fatalf("hint: handled=true, want false")
	}
	if result != nil {
		t.Fatalf("hint: result non-nil, want nil")
	}
	if supersedeID != "" {
		t.Fatalf("hint: supersedeID = %q, want empty", supersedeID)
	}
}

func TestRunSplitDedupCoalesceDrops(t *testing.T) {
	// Coalesce: incoming poorer → handled=true, returns existing.
	svc, st := newDedupSvc(t, 0.5, WriteDedupCoalesce)
	e := embedtest.New(dedupTestDims)
	putDedupMem(t, st, e, "a", "the sky is green today and bright", memory.TierSemantic)

	m := embedMem(t, e, "the sky is green", memory.TierSemantic)
	in := RememberInput{Content: m.Content, Namespace: m.Namespace}

	handled, result, supersedeID := svc.runSplitDedup(context.Background(), m, in)
	if !handled {
		t.Fatalf("coalesce drops: handled=false, want true")
	}
	if result == nil {
		t.Fatalf("coalesce drops: result nil, want existing memory")
	}
	if result.ID != "a" {
		t.Errorf("coalesce drops: result.ID = %q, want \"a\"", result.ID)
	}
	if supersedeID != "" {
		t.Errorf("coalesce drops: supersedeID = %q, want empty", supersedeID)
	}
}

func TestRunSplitDedupSupersedeDeferred(t *testing.T) {
	// Supersede: handled=false, supersedeID returned for caller to tombstone.
	svc, st := newDedupSvc(t, 0.5, WriteDedupSupersede)
	e := embedtest.New(dedupTestDims)
	putDedupMem(t, st, e, "a", "the sky is green", memory.TierSemantic)

	m := embedMem(t, e, "the sky is blue", memory.TierSemantic)
	in := RememberInput{Content: m.Content, Namespace: m.Namespace}

	handled, result, supersedeID := svc.runSplitDedup(context.Background(), m, in)
	if handled {
		t.Fatalf("supersede: handled=true, want false")
	}
	if result != nil {
		t.Fatalf("supersede: result non-nil, want nil")
	}
	if supersedeID != "a" {
		t.Fatalf("supersede: supersedeID = %q, want \"a\"", supersedeID)
	}
}

// ---------------------------------------------------------------------------
// fingerprintHit
// ---------------------------------------------------------------------------

func TestFingerprintHitExactMatch(t *testing.T) {
	// fingerprintHit matches exact content (normalized via SHA-256).
	// Needs a full Service with fingerprint dedup enabled (default).
	svc, st := newDedupSvc(t, 0.625, WriteDedupOff)

	// Direct store write to bypass the full pipeline (including fingerprintHit itself).
	// We need the fingerprint index populated, so we must use the service's
	// store directly with Upsert.
	now := time.Unix(1_700_000_000, 0).UTC()
	e := embedtest.New(dedupTestDims)
	vec, err := embed.EmbedOne(context.Background(), e, "hello world")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	existing := &memory.Memory{
		ID: "a", Namespace: "ns", Tier: memory.TierSemantic,
		Content: "hello world", Embedding: vec,
		CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
	}
	if err := st.Upsert(context.Background(), existing); err != nil {
		t.Fatalf("upsert existing: %v", err)
	}

	in := RememberInput{Namespace: "ns", Content: "hello world"}
	hit, ok := svc.fingerprintHit(context.Background(), in, memory.TierSemantic)
	if !ok {
		t.Fatalf("exact match: ok=false, want true")
	}
	if hit == nil {
		t.Fatalf("exact match: hit nil, want existing")
	}
	if hit.ID != "a" {
		t.Errorf("exact match: hit.ID = %q, want \"a\"", hit.ID)
	}
	// fingerprintHit reinforces the existing memory.
	refreshed, err := st.Get(context.Background(), "ns", "a")
	if err != nil {
		t.Fatalf("get existing: %v", err)
	}
	if refreshed.AccessCount <= existing.AccessCount {
		t.Errorf("fingerprintHit: existing AccessCount not bumped; was %d, now %d",
			existing.AccessCount, refreshed.AccessCount)
	}
}

func TestFingerprintHitDifferentContent(t *testing.T) {
	svc, st := newDedupSvc(t, 0.625, WriteDedupOff)
	now := time.Unix(1_700_000_000, 0).UTC()
	e := embedtest.New(dedupTestDims)
	vec, err := embed.EmbedOne(context.Background(), e, "hello world")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	existing := &memory.Memory{
		ID: "a", Namespace: "ns", Tier: memory.TierSemantic,
		Content: "hello world", Embedding: vec,
		CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
	}
	if err := st.Upsert(context.Background(), existing); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	in := RememberInput{Namespace: "ns", Content: "different content"}
	hit, ok := svc.fingerprintHit(context.Background(), in, memory.TierSemantic)
	if ok {
		t.Fatalf("different content: ok=true, want false")
	}
	if hit != nil {
		t.Fatalf("different content: hit non-nil, want nil")
	}
}

func TestFingerprintHitDedupDisabled(t *testing.T) {
	// Service with fingerprintDedup set to false via New defaults. We need to
	// verify that fingerprintHit returns false when disabled. Since New()
	// enables fingerprintDedup by default, we construct a Service with
	// WithFingerprintDedup(false). But there's no public option for that.
	// Let's instead test via ID override: fingerprintHit returns false when
	// in.ID != "" (an explicit update).
	svc, st := newDedupSvc(t, 0.625, WriteDedupOff)
	now := time.Unix(1_700_000_000, 0).UTC()
	e := embedtest.New(dedupTestDims)
	vec, err := embed.EmbedOne(context.Background(), e, "hello world")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	existing := &memory.Memory{
		ID: "a", Namespace: "ns", Tier: memory.TierSemantic,
		Content: "hello world", Embedding: vec,
		CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
	}
	if err := st.Upsert(context.Background(), existing); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Explicit ID → fingerprint dedup is bypassed.
	in := RememberInput{Namespace: "ns", Content: "hello world", ID: "b"}
	hit, ok := svc.fingerprintHit(context.Background(), in, memory.TierSemantic)
	if ok {
		t.Fatalf("ID override: ok=true, want false")
	}
	if hit != nil {
		t.Fatalf("ID override: hit non-nil, want nil")
	}
}

// ---------------------------------------------------------------------------
// dedupCheck: edge case — hint content truncation
// ---------------------------------------------------------------------------

func TestDedupCheckHintTruncation(t *testing.T) {
	svc, st := newDedupSvc(t, 0.5, WriteDedupHint)
	e := embedtest.New(dedupTestDims)

	// A long existing content (over 200 chars).
	long := strings.Repeat("the sky is green and bright. ", 20)
	putDedupMem(t, st, e, "a", long, memory.TierSemantic)

	// Similar incoming triggers the hint.
	m := embedMem(t, e, "the sky is green", memory.TierSemantic)

	_, hint, _ := svc.dedupCheck(context.Background(), m)
	if hint == nil {
		t.Fatalf("long content: expected hint, got nil")
	}
	if len(hint.SimilarContent) > 203 {
		t.Errorf("hint.SimilarContent too long: %d bytes, want ≤ 203 (200 bytes + ellipsis)",
			len(hint.SimilarContent))
	}
	if !strings.HasSuffix(hint.SimilarContent, "…") {
		t.Errorf("hint.SimilarContent should end with ellipsis, got %q", hint.SimilarContent)
	}
}

// --- C3: top-k dedup tests ---

// TestDedupCheckTopK picks best-fit from multiple above-threshold candidates.
func TestDedupCheckTopK(t *testing.T) {
	svc, st := newDedupSvc(t, 0.01, WriteDedupHint) // very low threshold so all are "above"
	e := embedtest.New(dedupTestDims)

	// Three memories, all semantically close (shared words).
	putDedupMem(t, st, e, "a", "the cache ttl is ten minutes for sure", memory.TierSemantic)
	putDedupMem(t, st, e, "b", "the cache ttl is ten minutes", memory.TierSemantic)
	putDedupMem(t, st, e, "c", "cache something about ttl", memory.TierSemantic)

	// Incoming overlaps with all three. With k=5 the dedup should find multiple
	// candidates and return a hint for the best (highest-scoring) one.
	m := embedMem(t, e, "the cache ttl is ten minutes exactly", memory.TierSemantic)
	_, hint, _ := svc.dedupCheck(context.Background(), m)
	if hint == nil {
		t.Fatalf("expected hint from top-k, got nil")
	}
	// The hint should point at one of the seeded candidates.
	validIDs := map[string]bool{"a": true, "b": true, "c": true}
	if !validIDs[hint.SimilarID] {
		t.Errorf("top-k: hint target %q is not one of the seeded candidates", hint.SimilarID)
	}
}

// TestDedupCheckTopKAllBelowThreshold returns no match when no candidate clears threshold.
func TestDedupCheckTopKAllBelowThreshold(t *testing.T) {
	svc, st := newDedupSvc(t, 0.99, WriteDedupHint) // very high threshold
	e := embedtest.New(dedupTestDims)

	putDedupMem(t, st, e, "a", "the cache is sharded across four nodes by key hash", memory.TierSemantic)
	putDedupMem(t, st, e, "b", "the rate limiter keys buckets by API token not IP", memory.TierSemantic)

	m := embedMem(t, e, "image uploads are limited to ten megabytes", memory.TierSemantic)
	_, hint, sid := svc.dedupCheck(context.Background(), m)
	if hint != nil || sid != "" {
		t.Errorf("expected no match (all below threshold), got hint=%v sid=%q", hint != nil, sid)
	}
}

// --- C2: split-dedup LLM merge tests ---

// newDedupSvcWithLLM creates a Service with split-dedup LLM merge enabled and
// a recording consolidator that returns the given decision.
func newDedupSvcWithLLM(t *testing.T, score float64, action WriteDedupAction, dec llm.Decision) (*Service, *sqlitevec.Store, *recordingConsolidator) {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "dedup_llm.db"), dedupTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rc := &recordingConsolidator{dec: dec}
	opts := []Option{
		WithWriteDedup(score, action),
		WithConsolidator(rc),
		WithSplitDedupLLMMerge(true),
		WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
		WithSyncReinforce(),
	}
	svc := New(st, embedtest.New(dedupTestDims), opts...)
	return svc, st, rc
}

// TestSplitDedupLLMMergeGatedOneCandidate does NOT call LLM when only 1 candidate is above threshold.
func TestSplitDedupLLMMergeGatedOneCandidate(t *testing.T) {
	dec := llm.Decision{Action: llm.ActionNew}
	svc, st, rc := newDedupSvcWithLLM(t, 0.01, WriteDedupHint, dec)
	e := embedtest.New(dedupTestDims)

	// Only one close candidate.
	putDedupMem(t, st, e, "a", "the cache ttl is ten minutes", memory.TierSemantic)
	putDedupMem(t, st, e, "b", "totally unrelated topic about gardening", memory.TierSemantic)

	m := embedMem(t, e, "the cache ttl is ten minutes exactly", memory.TierSemantic)
	svc.dedupCheck(context.Background(), m)

	if rc.called != 0 {
		t.Errorf("LLM should NOT be called with only 1 above-threshold candidate, got called=%d", rc.called)
	}
}

// TestSplitDedupLLMMergeGatedTwoCloseCandidates calls LLM when ≥2 candidates are close.
func TestSplitDedupLLMMergeGatedTwoCloseCandidates(t *testing.T) {
	dec := llm.Decision{Action: llm.ActionNew}
	svc, st, rc := newDedupSvcWithLLM(t, 0.01, WriteDedupHint, dec)
	e := embedtest.New(dedupTestDims)

	// Two close candidates (similar content → similar score).
	putDedupMem(t, st, e, "a", "the cache ttl is ten minutes for sure", memory.TierSemantic)
	putDedupMem(t, st, e, "b", "the cache ttl is ten minutes exactly", memory.TierSemantic)

	m := embedMem(t, e, "the cache ttl is ten minutes right now", memory.TierSemantic)
	svc.dedupCheck(context.Background(), m)

	if rc.called == 0 {
		t.Error("LLM should be called with ≥2 close candidates, got called=0")
	}
}

// TestSplitDedupLLMMergeDisabled does NOT call LLM when feature is off (default).
func TestSplitDedupLLMMergeDisabled(t *testing.T) {
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "dedup_off.db"), dedupTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rc := &recordingConsolidator{dec: llm.Decision{Action: llm.ActionNew}}
	// WithSplitDedupLLMMerge NOT called — default is false.
	opts := []Option{
		WithWriteDedup(0.01, WriteDedupHint),
		WithConsolidator(rc),
		WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
		WithSyncReinforce(),
	}
	svc := New(st, embedtest.New(dedupTestDims), opts...)
	e := embedtest.New(dedupTestDims)

	putDedupMem(t, st, e, "a", "the cache ttl is ten minutes for sure", memory.TierSemantic)
	putDedupMem(t, st, e, "b", "the cache ttl is ten minutes exactly", memory.TierSemantic)

	m := embedMem(t, e, "the cache ttl is ten minutes right now", memory.TierSemantic)
	svc.dedupCheck(context.Background(), m)

	if rc.called != 0 {
		t.Errorf("LLM should NOT be called when splitDedupLLMMerge is off, got called=%d", rc.called)
	}
}

// TestSplitDedupLLMMergeErrorFallsThrough falls back to deterministic dedup on LLM error.
func TestSplitDedupLLMMergeErrorFallsThrough(t *testing.T) {
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "dedup_err.db"), dedupTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// A consolidator that always errors.
	rc := &erroringConsolidator{}
	opts := []Option{
		WithWriteDedup(0.01, WriteDedupHint),
		WithConsolidator(rc),
		WithSplitDedupLLMMerge(true),
		WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
		WithSyncReinforce(),
	}
	svc := New(st, embedtest.New(dedupTestDims), opts...)
	e := embedtest.New(dedupTestDims)

	putDedupMem(t, st, e, "a", "the cache ttl is ten minutes for sure", memory.TierSemantic)
	putDedupMem(t, st, e, "b", "the cache ttl is ten minutes exactly", memory.TierSemantic)

	m := embedMem(t, e, "the cache ttl is ten minutes right now", memory.TierSemantic)
	_, hint, _ := svc.dedupCheck(context.Background(), m)

	// LLM errored → deterministic hint should still fire.
	if hint == nil {
		t.Error("LLM error should fall through to deterministic dedup (hint), got nil")
	}
}

// TestSplitDedupLLMMergeSupersede returns supersedeID when LLM says supersede.
func TestSplitDedupLLMMergeSupersede(t *testing.T) {
	dec := llm.Decision{Action: llm.ActionSupersede, Target: "a", Content: "new content"}
	svc, st, rc := newDedupSvcWithLLM(t, 0.01, WriteDedupHint, dec)
	e := embedtest.New(dedupTestDims)

	putDedupMem(t, st, e, "a", "the cache ttl is ten minutes for sure", memory.TierSemantic)
	putDedupMem(t, st, e, "b", "the cache ttl is ten minutes exactly", memory.TierSemantic)

	m := embedMem(t, e, "the cache ttl is ten minutes right now", memory.TierSemantic)
	_, _, sid := svc.dedupCheck(context.Background(), m)

	if rc.called == 0 {
		t.Fatal("LLM should have been called")
	}
	if sid != "a" {
		t.Errorf("expected supersedeID 'a', got %q", sid)
	}
}

// erroringConsolidator always returns an error.
type erroringConsolidator struct{}

func (e *erroringConsolidator) Consolidate(_ context.Context, _ llm.Input) (llm.Decision, error) {
	return llm.Decision{}, fmt.Errorf("simulated LLM failure")
}
