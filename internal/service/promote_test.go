package service_test

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
)

// TestPromoteWholeCountsRunesNotBytes pins that whole-content promotion bounds
// its source in runes. On a byte bound, non-ASCII prose blew the 240 cap at a
// third of the nominal length and was never promoted, however often it was
// recalled — so a CJK-writing user's memories could not reach a durable tier.
func TestPromoteWholeCountsRunesNotBytes(t *testing.T) {
	svc := service.New(openTestStore(t), embedtest.New(dims),
		service.WithSyncReinforce(),
		service.WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
		service.WithPromoteMinAccess(1),
	)
	ctx := context.Background()

	// No extractable marker (the markers are English), so this can only reach a
	// durable tier via the whole-content branch. Under the 240-rune cap and over
	// a 240-byte one: the gap the byte comparison silently swallowed.
	cjk := strings.Repeat("这个部署流水线在十六个核心上并行运行测试以最小化延迟。", 7)
	if r, b := utf8.RuneCountInString(cjk), len(cjk); r > 240 || b <= 240 {
		t.Fatalf("setup: want runes<=240 and bytes>240, got runes=%d bytes=%d", r, b)
	}

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Tier: memory.TierEpisodic, Content: cjk,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	// One recall reaches the promoteMinAccess=1 eligibility bar.
	if _, err := svc.Recall(ctx, service.RecallInput{Namespace: "alice", Query: cjk, Limit: 1}); err != nil {
		t.Fatalf("recall: %v", err)
	}

	if _, err := svc.Promote(ctx); err != nil {
		t.Fatalf("promote: %v", err)
	}

	durable, err := svc.List(ctx, service.ListInput{
		Namespace: "alice", Tiers: []memory.Tier{memory.TierSemantic, memory.TierProcedural},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, m := range durable {
		if m.Metadata["promoted_from"] != nil && m.Content == cjk {
			return
		}
	}
	t.Fatal("cjk source under the rune cap was not promoted whole")
}

// TestPromoteWholeASCIIBoundaryUnchanged pins the other half of the rune fix:
// for ASCII, bytes and runes agree, so the 240 boundary must not have moved by
// even one character. Counting alone can't distinguish the two here — only the
// boundary can — so this is what makes "defaults preserve behaviour" a fact
// rather than a claim.
func TestPromoteWholeASCIIBoundaryUnchanged(t *testing.T) {
	const cap = service.DefaultPromoteWholeMaxChars
	for _, tc := range []struct {
		name        string
		n           int
		wantPromote bool
	}{
		{"at the cap", cap, true},
		{"one over the cap", cap + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := service.New(openTestStore(t), embedtest.New(dims),
				service.WithSyncReinforce(),
				service.WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
				service.WithPromoteMinAccess(1),
			)
			ctx := context.Background()

			// Marker-free ASCII, so only the whole-content branch can promote it,
			// and only the length cap can stop that.
			src := "sprint retro notes " + strings.Repeat("x", tc.n-len("sprint retro notes "))
			if len(src) != tc.n || utf8.RuneCountInString(src) != tc.n {
				t.Fatalf("setup: want %d ASCII runes, got %d bytes / %d runes",
					tc.n, len(src), utf8.RuneCountInString(src))
			}
			if _, err := svc.Remember(ctx, service.RememberInput{
				Namespace: "alice", Tier: memory.TierEpisodic, Content: src,
			}); err != nil {
				t.Fatalf("remember: %v", err)
			}
			if _, err := svc.Recall(ctx, service.RecallInput{Namespace: "alice", Query: src, Limit: 1}); err != nil {
				t.Fatalf("recall: %v", err)
			}
			if _, err := svc.Promote(ctx); err != nil {
				t.Fatalf("promote: %v", err)
			}

			durable, err := svc.List(ctx, service.ListInput{
				Namespace: "alice", Tiers: []memory.Tier{memory.TierSemantic, memory.TierProcedural},
			})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			promoted := false
			for _, m := range durable {
				if m.Metadata["promoted_from"] != nil && m.Content == src {
					promoted = true
				}
			}
			if promoted != tc.wantPromote {
				t.Fatalf("%d-rune ASCII source promoted = %v, want %v", tc.n, promoted, tc.wantPromote)
			}
		})
	}
}
