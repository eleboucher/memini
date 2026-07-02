package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
)

func TestRememberClassifiesOmittedTier(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	tests := []struct {
		name    string
		content string
		want    memory.Tier
	}{
		{"decision", "We decided to use Postgres instead of MySQL for the vector store.", memory.TierSemantic},
		{"preference", "I prefer table-driven tests, please always use them instead of ad-hoc asserts.", memory.TierProcedural},
		{"chatter", "Met with the platform team about quarterly planning this afternoon.", memory.TierEpisodic},
		{"hedged", "Maybe we should go with Postgres instead of MySQL for this.", memory.TierEpisodic},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := svc.Remember(ctx, service.RememberInput{Namespace: "alice", Content: tc.content})
			if err != nil {
				t.Fatalf("remember: %v", err)
			}
			if m.Tier != tc.want {
				t.Fatalf("tier = %q, want %q", m.Tier, tc.want)
			}
			if tc.want.Term() == memory.LongTerm {
				if m.ExpiresAt != nil {
					t.Fatal("classified durable write should not expire")
				}
				if v, _ := m.Metadata["tier_classified"].(string); v != "marker" {
					t.Fatalf("metadata tier_classified = %v, want marker", m.Metadata["tier_classified"])
				}
			} else if _, ok := m.Metadata["tier_classified"]; ok {
				t.Fatal("episodic fallback must not carry the classification stamp")
			}
		})
	}

	// An explicit tier always wins over classification.
	m, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Tier: memory.TierWorking,
		Content: "We decided to use SQLite instead of Postgres for the tests.",
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if m.Tier != memory.TierWorking {
		t.Fatalf("explicit tier = %q, want working", m.Tier)
	}
}

func TestCorroborationGrowsDurableFact(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	svc := service.New(openTestStore(t), embedtest.New(dims),
		service.WithSyncReinforce(),
		service.WithClock(func() time.Time { return now }),
		service.WithCorroboration(0.7),
	)
	ctx := context.Background()

	fact, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Tier: memory.TierSemantic,
		Content: "the api gateway strips the x-request-id header on retries",
	})
	if err != nil {
		t.Fatalf("remember fact: %v", err)
	}
	if fact.Confidence == nil {
		t.Fatal("durable fact should carry a confidence seed")
	}
	seed := *fact.Confidence

	// Within the cooldown window a restatement must not grow confidence:
	// same-session echo is one observation, not several. Each restatement is
	// worded slightly differently so fingerprint dedup (exact restatements
	// coalesce into each other) stays out of the way — the bag-of-words test
	// embedder keeps them ~0.95-similar to the fact.
	restate := func(content string) {
		t.Helper()
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "alice", Tier: memory.TierEpisodic, Content: content,
		}); err != nil {
			t.Fatalf("remember restatement: %v", err)
		}
		svc.WaitBackground()
	}
	restate("the api gateway strips the x-request-id header on retries today")
	got, err := svc.Get(ctx, "alice", fact.ID)
	if err != nil {
		t.Fatalf("get fact: %v", err)
	}
	if *got.Confidence != seed {
		t.Fatalf("confidence grew inside cooldown: %v -> %v", seed, *got.Confidence)
	}

	// Past the cooldown the restatement corroborates: confidence grows and the
	// fact is reinforced.
	now = now.Add(25 * time.Hour)
	restate("the api gateway strips the x-request-id header on failed retries")
	got, err = svc.Get(ctx, "alice", fact.ID)
	if err != nil {
		t.Fatalf("get fact: %v", err)
	}
	if *got.Confidence <= seed {
		t.Fatalf("confidence = %v, want growth above seed %v", *got.Confidence, seed)
	}
	if got.AccessCount == 0 {
		t.Fatal("corroboration should reinforce the fact")
	}

	// Immediately after, the cooldown re-arms.
	grown := *got.Confidence
	restate("the api gateway always strips the x-request-id header on retries")
	got, err = svc.Get(ctx, "alice", fact.ID)
	if err != nil {
		t.Fatalf("get fact: %v", err)
	}
	if *got.Confidence != grown {
		t.Fatalf("confidence grew again inside re-armed cooldown: %v -> %v", grown, *got.Confidence)
	}
}

func TestPromoteHeuristicWithoutLLM(t *testing.T) {
	svc := service.New(openTestStore(t), embedtest.New(dims),
		service.WithSyncReinforce(),
		service.WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
		service.WithPromoteMinAccess(1),
	)
	ctx := context.Background()

	// One episodic with an extractable decision, one short one with no markers.
	decision := "We decided to use Postgres instead of MySQL because of pgvector support."
	plain := "sprint retro is every second friday at 3pm"
	for _, content := range []string{decision, plain} {
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "alice", Tier: memory.TierEpisodic, Content: content,
		}); err != nil {
			t.Fatalf("remember: %v", err)
		}
		// One recall reaches the promoteMinAccess=1 eligibility bar.
		if _, err := svc.Recall(ctx, service.RecallInput{Namespace: "alice", Query: content, Limit: 1}); err != nil {
			t.Fatalf("recall: %v", err)
		}
	}

	n, err := svc.Promote(ctx)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if n < 2 {
		t.Fatalf("promoted %d facts, want >= 2", n)
	}

	durable, err := svc.List(ctx, service.ListInput{
		Namespace: "alice", Tiers: []memory.Tier{memory.TierSemantic, memory.TierProcedural},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	foundExtracted, foundWhole := false, false
	for _, m := range durable {
		if m.Metadata["promoted_from"] == nil {
			continue
		}
		if m.Content == plain {
			foundWhole = true
		} else {
			foundExtracted = true
		}
	}
	if !foundExtracted {
		t.Fatal("marker-extracted fact missing from durable tiers")
	}
	if !foundWhole {
		t.Fatal("short no-marker source was not promoted whole")
	}

	// A second pass finds nothing new: the sources are stamped.
	if n, err = svc.Promote(ctx); err != nil || n != 0 {
		t.Fatalf("second promote = (%d, %v), want (0, nil)", n, err)
	}
}
