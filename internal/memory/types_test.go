package memory_test

import (
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/memory"
)

func TestTierValid(t *testing.T) {
	tests := []struct {
		tier memory.Tier
		want bool
	}{
		{memory.TierWorking, true},
		{memory.TierEpisodic, true},
		{memory.TierSemantic, true},
		{memory.TierProcedural, true},
		{memory.Tier(""), false},
		{memory.Tier("bogus"), false},
	}
	for _, tt := range tests {
		if got := tt.tier.Valid(); got != tt.want {
			t.Errorf("Tier(%q).Valid() = %v, want %v", tt.tier, got, tt.want)
		}
	}
}

func TestTierDefaultTTL(t *testing.T) {
	tests := []struct {
		tier memory.Tier
		want time.Duration
	}{
		{memory.TierWorking, 24 * time.Hour},
		{memory.TierEpisodic, 30 * 24 * time.Hour},
		{memory.TierSemantic, 0},
		{memory.TierProcedural, 0},
		{memory.Tier("unknown"), 0},
	}
	for _, tt := range tests {
		if got := tt.tier.DefaultTTL(); got != tt.want {
			t.Errorf("Tier(%q).DefaultTTL() = %v, want %v", tt.tier, got, tt.want)
		}
	}
}

func TestMemoryExpired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	tests := []struct {
		name      string
		expiresAt *time.Time
		want      bool
	}{
		{"no expiry", nil, false},
		{"expired in past", &past, true},
		{"expires in future", &future, false},
		{"expires exactly now", &now, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &memory.Memory{ExpiresAt: tt.expiresAt}
			if got := m.Expired(now); got != tt.want {
				t.Errorf("Expired(%v) = %v, want %v", now, got, tt.want)
			}
		})
	}
}

func TestQualityDurableSkipsRecencyDecay(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-60 * 24 * time.Hour) // ~8 recency half-lives

	mk := func(tier memory.Tier) *memory.Memory {
		return &memory.Memory{
			Tier: tier, Importance: 0.5,
			CreatedAt: stale, UpdatedAt: now, LastAccessedAt: stale,
		}
	}

	// A stale semantic fact keeps its salience-driven quality instead of
	// collapsing toward zero with the short-term recency factor.
	sem := mk(memory.TierSemantic).Quality(now)
	if want := mk(memory.TierSemantic).DurableScore(now); sem != want {
		t.Fatalf("semantic Quality = %v, want DurableScore %v", sem, want)
	}
	epi := mk(memory.TierEpisodic).Quality(now)
	if sem <= epi {
		t.Fatalf("stale semantic (%v) should outscore stale episodic (%v)", sem, epi)
	}

	// Short-term tiers still decay: fresh episodic beats stale episodic.
	freshEpi := mk(memory.TierEpisodic)
	freshEpi.LastAccessedAt = now
	if freshEpi.Quality(now) <= epi {
		t.Fatalf("fresh episodic (%v) should outscore stale episodic (%v)", freshEpi.Quality(now), epi)
	}
}
