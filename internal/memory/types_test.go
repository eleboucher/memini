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
		{memory.TierEpisodic, 90 * 24 * time.Hour},
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
