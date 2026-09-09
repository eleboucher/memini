package service

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/store/sqlitevec"

	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// longPrompt is a stand-in for a real handoff: far past the ~400-rune
// classification ceiling, so it exercises the tier cliff the feature exists to
// avoid rather than a toy string that would classify normally.
func longPrompt(title string) string {
	var b strings.Builder
	b.WriteString("# " + title + "\n\n")
	for range 60 {
		b.WriteString("Read the plan, seed the todo list in dependency order, keep the helper.\n")
	}
	return b.String()
}

// rememberHandoff writes one handoff and fails the test if the write is
// rejected or dropped.
func rememberHandoff(t *testing.T, svc *Service, ns, slot, title string) *memory.Memory {
	t.Helper()
	meta := map[string]any{"handoff_harness": "claude-code", "handoff_lines": 212}
	if slot != "" {
		meta[MetaHandoffSlot] = slot
	}
	m, err := svc.Remember(context.Background(), RememberInput{
		Namespace: ns,
		Content:   longPrompt(title),
		Summary:   title,
		Tags:      []string{maintenance.HandoffTag},
		Metadata:  meta,
	})
	if err != nil {
		t.Fatalf("remember handoff %q: %v", title, err)
	}
	if m == nil {
		t.Fatalf("remember handoff %q: write was dropped", title)
	}
	return m
}

// TestHandoffWriteNormalization pins the three stamps a handoff write must
// carry without the client asking: the procedural tier (so a prompt well past
// the classification ceiling does not land in the working tier and expire in
// 72 hours), the memory_type for the UI filter, and the default slot.
func TestHandoffWriteNormalization(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	m := rememberHandoff(t, svc, "proj", "", "execute stage 4")

	if m.Tier != memory.TierProcedural {
		t.Errorf("tier = %q, want %q — a long prompt must not fall to the classification default",
			m.Tier, memory.TierProcedural)
	}
	if m.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil: a handoff must outlive the session that wrote it", m.ExpiresAt)
	}
	if got := m.Metadata["memory_type"]; got != HandoffMemoryType {
		t.Errorf("metadata[memory_type] = %v, want %q", got, HandoffMemoryType)
	}
	if got := m.Metadata[MetaHandoffSlot]; got != DefaultHandoffSlot {
		t.Errorf("metadata[%s] = %v, want %q", MetaHandoffSlot, got, DefaultHandoffSlot)
	}
	if got := m.Metadata[MetaHandoffMarker]; got != handoffMarkerValue {
		t.Errorf("metadata[%s] = %v, want %q — recall excludes on this key",
			MetaHandoffMarker, got, handoffMarkerValue)
	}
}

// TestHandoffExplicitTierWins checks the procedural default is a default, not
// an override: a caller that names a tier keeps it.
func TestHandoffExplicitTierWins(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	m, err := svc.Remember(context.Background(), RememberInput{
		Namespace: "proj",
		Content:   longPrompt("scratch handoff"),
		Tier:      memory.TierEpisodic,
		Tags:      []string{maintenance.HandoffTag},
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if m.Tier != memory.TierEpisodic {
		t.Errorf("tier = %q, want %q: an explicit tier must survive normalization", m.Tier, memory.TierEpisodic)
	}
}

// TestHandoffSupersedesWithinSlot is the core lifecycle guarantee: a second
// handoff in the same slot replaces the first, the first stays readable
// through history rather than being overwritten, and a different slot is
// untouched by either.
func TestHandoffSupersedesWithinSlot(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	ctx := context.Background()

	first := rememberHandoff(t, svc, "proj", "", "stage 3")
	other := rememberHandoff(t, svc, "proj", "wt-polygons", "polygon work")
	second := rememberHandoff(t, svc, "proj", "", "stage 4")

	// The supersede runs in the background once the replacement is durably
	// stored; wait for it rather than racing it.
	svc.WaitBackground()

	got, err := svc.Get(ctx, "proj", first.ID)
	if err != nil {
		t.Fatalf("get superseded handoff: %v", err)
	}
	if got.SupersededBy == nil || *got.SupersededBy != second.ID {
		t.Errorf("first.SupersededBy = %v, want %q", got.SupersededBy, second.ID)
	}
	if !strings.Contains(got.Content, "stage 3") {
		t.Error("the superseded prompt's text was lost; supersession must preserve it, not overwrite")
	}

	sibling, err := svc.Get(ctx, "proj", other.ID)
	if err != nil {
		t.Fatalf("get sibling-slot handoff: %v", err)
	}
	if sibling.SupersededBy != nil {
		t.Errorf("a handoff in slot %q was superseded by a write to another slot", "wt-polygons")
	}
}

// TestHandoffExcludedFromRecall covers the reason handoffs are held out of the
// corpus: the prompt lexically matches project queries, so left in it would
// consume top-k slots that belong to real facts.
func TestHandoffExcludedFromRecall(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	ctx := context.Background()
	h := rememberHandoff(t, svc, "proj", "", "execute stage 4")

	res, err := svc.Recall(ctx, RecallInput{Namespace: "proj", Query: "execute stage 4", Limit: 10})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	for _, r := range res {
		if r.Memory.ID == h.ID {
			t.Fatal("recall returned a handoff by default; it must be reached through the briefing pointer")
		}
	}

	opted, err := svc.Recall(ctx, RecallInput{
		Namespace: "proj", Query: "execute stage 4", Limit: 10, IncludeHandoffs: true,
	})
	if err != nil {
		t.Fatalf("recall with IncludeHandoffs: %v", err)
	}
	if !containsID(opted, h.ID) {
		t.Error("IncludeHandoffs did not bring the handoff back; the opt-in is the only way to search them")
	}
}

// TestRecallExcludeMetadataNotMutated guards the shared-map hazard: a
// long-lived client that reuses one options struct must not accumulate
// exclusions the server added on its behalf.
func TestRecallExcludeMetadataNotMutated(t *testing.T) {
	caller := map[string]string{"source": "turn_capture"}
	in := RecallInput{ExcludeMetadata: caller}

	merged := recallExcludeMetadata(in)
	if _, leaked := caller[MetaHandoffMarker]; leaked {
		t.Error("recallExcludeMetadata mutated the caller's map")
	}
	if merged["source"] != "turn_capture" {
		t.Error("the caller's own exclusion was lost in the merge")
	}
	if merged[MetaHandoffMarker] != handoffMarkerValue {
		t.Error("the handoff exclusion was not applied")
	}
}

// TestHandoffBriefingPointers checks the briefing surfaces handoffs as
// pointers only: one per slot, ordered by slot, never inside a content
// section, and carrying the metadata a reader needs to decide whether to pull
// the prompt.
func TestHandoffBriefingPointers(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	ctx := context.Background()

	rememberHandoff(t, svc, "proj", "wt-polygons", "polygon work")
	stage4 := rememberHandoff(t, svc, "proj", "", "execute stage 4")

	b, err := svc.Briefing(ctx, "proj", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}

	if len(b.Handoffs) != 2 {
		t.Fatalf("got %d handoff pointers, want 2 (one per slot)", len(b.Handoffs))
	}
	if b.Handoffs[0].Slot != DefaultHandoffSlot || b.Handoffs[1].Slot != "wt-polygons" {
		t.Errorf("pointers not ordered by slot name: got %q then %q",
			b.Handoffs[0].Slot, b.Handoffs[1].Slot)
	}
	main := b.Handoffs[0]
	if main.ID != stage4.ID {
		t.Errorf("main slot points at %q, want the newest handoff %q", main.ID, stage4.ID)
	}
	if main.Summary != "execute stage 4" || main.Harness != "claude-code" || main.Lines != 212 {
		t.Errorf("pointer lost its provenance: %+v", main)
	}

	for _, m := range b.Procedures {
		if isHandoff(m.Tags) {
			t.Fatal("a handoff was rendered into the How-to section; it must appear only as a pointer")
		}
	}
}

// TestHandoffBriefingHidesSuperseded checks a replaced handoff stops being
// advertised: a slot advertises exactly one prompt, the current one.
func TestHandoffBriefingHidesSuperseded(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	ctx := context.Background()

	rememberHandoff(t, svc, "proj", "", "stage 3")
	second := rememberHandoff(t, svc, "proj", "", "stage 4")
	svc.WaitBackground()

	b, err := svc.Briefing(ctx, "proj", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if len(b.Handoffs) != 1 {
		t.Fatalf("got %d pointers for one slot, want 1", len(b.Handoffs))
	}
	if b.Handoffs[0].ID != second.ID {
		t.Errorf("slot advertises %q, want the current handoff %q", b.Handoffs[0].ID, second.ID)
	}
}

// TestHandoffPointersAreNamespaceLocal checks a handoff never travels the
// inheritance cascade. An ancestor's handoff would tell this session to resume
// work in another project.
func TestHandoffPointersAreNamespaceLocal(t *testing.T) {
	svc, st := newReadsetSvc(t)
	ctx := context.Background()

	rememberHandoff(t, svc, "acme", "", "org-level work")
	seedNamespace(t, st, "acme/phoenix")

	b, err := svc.Briefing(ctx, "acme/phoenix", BriefingOpts{Home: "personal"})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if len(b.Handoffs) != 0 {
		t.Errorf("inherited %d handoff pointer(s) from an ancestor namespace", len(b.Handoffs))
	}
}

// TestHandoffSurvivesDemotionSweep pins the retention guarantee: a handoff is
// never recalled, so it never accrues an access count, which is exactly the
// profile DemoteStale exists to sweep away.
func TestHandoffSurvivesDemotionSweep(t *testing.T) {
	svc, st := newReadsetSvc(t)
	ctx := context.Background()
	h := rememberHandoff(t, svc, "proj", "", "execute stage 4")

	now := svc.now()
	n, err := maintenance.DemoteStale(ctx, st, now.Add(time.Hour), now.Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("demote sweep: %v", err)
	}
	got, err := svc.Get(ctx, "proj", h.ID)
	if err != nil {
		t.Fatalf("get after sweep: %v", err)
	}
	if got.Tier != memory.TierProcedural {
		t.Errorf("handoff demoted to %q by a sweep that touched %d memories", got.Tier, n)
	}
}

func containsID(res []store.Scored, id string) bool {
	for _, r := range res {
		if r.Memory.ID == id {
			return true
		}
	}
	return false
}

// TestHandoffSlotValidation pins the reason a malformed slot fails the write
// instead of falling back to "main": the fallback would make this write
// supersede the main slot's handoff, destroying an unrelated lane's prompt.
// Failing is recoverable; guessing is not.
func TestHandoffSlotValidation(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	ctx := context.Background()

	// A real branch name well past the old 64-byte cap must be accepted, not
	// silently collapsed — that collapse was the data-loss path.
	longBranch := "feature/PROJ-1234-add-a-long-but-entirely-reasonable-branch-name-for-clarity"
	m, err := svc.Remember(ctx, RememberInput{
		Namespace: "proj", Content: longPrompt("branch work"),
		Tags:     []string{maintenance.HandoffTag},
		Metadata: map[string]any{MetaHandoffSlot: longBranch},
	})
	if err != nil {
		t.Fatalf("a %d-char branch slot was rejected: %v", len(longBranch), err)
	}
	if got := m.Metadata[MetaHandoffSlot]; got != longBranch {
		t.Errorf("slot = %v, want the branch name — a silent collapse to main would clobber it", got)
	}

	for name, slot := range map[string]any{
		"over the limit": strings.Repeat("x", maxHandoffSlot+1),
		"not a string":   42,
		"blank":          "   ",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Remember(ctx, RememberInput{
				Namespace: "proj", Content: longPrompt("bad slot"),
				Tags:     []string{maintenance.HandoffTag},
				Metadata: map[string]any{MetaHandoffSlot: slot},
			})
			if err == nil {
				t.Fatal("write accepted; it must fail rather than fall back to the main slot")
			}
		})
	}
}

// TestHandoffSupersedesStrays covers the concurrency aftermath: a race can
// leave two live handoffs in one slot with neither superseding the other, and
// the older is then invisible to the briefing and unreachable through history.
// The next write to that slot must reconcile both, not layer a third on top.
func TestHandoffSupersedesStrays(t *testing.T) {
	svc, st := newReadsetSvc(t)
	ctx := context.Background()

	// Two live handoffs in one slot, as a lost supersede would leave them.
	meta := map[string]any{
		MetaHandoffSlot:   DefaultHandoffSlot,
		MetaHandoffMarker: handoffMarkerValue,
		"memory_type":     HandoffMemoryType,
	}
	now := svc.now()
	for _, id := range []string{"stray-a", "stray-b"} {
		if err := st.Upsert(ctx, &memory.Memory{
			ID: id, Namespace: "proj", Tier: memory.TierProcedural,
			Content: longPrompt(id), Tags: []string{maintenance.HandoffTag}, Metadata: meta,
			CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	next := rememberHandoff(t, svc, "proj", "", "the reconciling write")
	svc.WaitBackground()

	for _, id := range []string{"stray-a", "stray-b"} {
		got, err := svc.Get(ctx, "proj", id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.SupersededBy == nil || *got.SupersededBy != next.ID {
			t.Errorf("%s.SupersededBy = %v, want %q — a stray must not stay live and invisible",
				id, got.SupersededBy, next.ID)
		}
	}
	b, err := svc.Briefing(ctx, "proj", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if len(b.Handoffs) != 1 || b.Handoffs[0].ID != next.ID {
		t.Errorf("slot advertises %+v, want only %q", b.Handoffs, next.ID)
	}
}

// TestHandoffPointerCaps bounds the one block no token budget trims.
func TestHandoffPointerCaps(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	ctx := context.Background()

	for i := range maxHandoffPointers + 3 {
		rememberHandoff(t, svc, "proj", fmt.Sprintf("slot-%02d", i), "work")
	}
	long, err := svc.Remember(ctx, RememberInput{
		Namespace: "proj", Content: longPrompt("verbose"),
		Summary: strings.Repeat("s", maxHandoffSummary*2),
		Tags:    []string{maintenance.HandoffTag},
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}

	b, err := svc.Briefing(ctx, "proj", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if len(b.Handoffs) > maxHandoffPointers {
		t.Errorf("briefing listed %d pointers, over the %d cap", len(b.Handoffs), maxHandoffPointers)
	}
	for _, p := range b.Handoffs {
		if p.ID != long.ID {
			continue
		}
		if n := len([]rune(p.Summary)); n > maxHandoffSummary+1 {
			t.Errorf("summary is %d runes, over the %d cap", n, maxHandoffSummary)
		}
	}
}

// TestHandoffNotADedupCandidate covers the mirror of the write-time guards: it
// is not enough that a handoff never TRIGGERS dedup, it must never be picked as
// another write's near-duplicate either, or an ordinary procedural write could
// tombstone a session's prompt as a side effect.
func TestHandoffNotADedupCandidate(t *testing.T) {
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "dedup.db"), readsetTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := New(st, embedtest.New(readsetTestDims),
		WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
		WithWriteDedup(0.01, WriteDedupSupersede))
	ctx := context.Background()

	h := rememberHandoff(t, svc, "proj", "", "execute stage 4")

	// A procedural write with identical prose: at a 0.0 threshold every
	// candidate clears the bar, so only the candidate-set exclusion can save
	// the handoff here.
	if _, err := svc.Remember(ctx, RememberInput{
		Namespace: "proj", Content: longPrompt("execute stage 4") + "\nand one more line",
		Tier: memory.TierProcedural,
	}); err != nil {
		t.Fatalf("remember neighbour: %v", err)
	}
	svc.WaitBackground()

	got, err := svc.Get(ctx, "proj", h.ID)
	if err != nil {
		t.Fatalf("get handoff: %v", err)
	}
	if got.SupersededBy != nil {
		t.Errorf("an unrelated procedural write tombstoned the handoff (superseded_by=%v)", *got.SupersededBy)
	}
}

// TestHandoffNotInChildRollup covers the briefing path one level removed from
// bucket(): a parent's rollup ships whole memories, so a handoff reaching it
// would dump the entire prompt into the parent's briefing.
func TestHandoffNotInChildRollup(t *testing.T) {
	svc, st := newReadsetSvc(t)
	ctx := context.Background()

	seedNamespace(t, st, "acme")
	h := rememberHandoff(t, svc, "acme/phoenix", "", "child work")

	b, err := svc.Briefing(ctx, "acme", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	for _, c := range b.Children {
		for _, m := range append(append([]*memory.Memory{}, c.Pinned...), c.Recent...) {
			if m.ID == h.ID {
				t.Fatalf("a handoff surfaced in the %s rollup, prompt content and all", c.NS)
			}
		}
	}
}
