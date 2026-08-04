package memory_test

import (
	"math"
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
		{memory.TierWorking, 72 * time.Hour},
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

func TestLevelValid(t *testing.T) {
	tests := []struct {
		level memory.Level
		want  bool
	}{
		{memory.LevelExplicit, true},
		{memory.LevelDeduced, true},
		{memory.Level(""), false}, // legacy/unknown fails validation
		{memory.Level("bogus"), false},
	}
	for _, tt := range tests {
		if got := tt.level.Valid(); got != tt.want {
			t.Errorf("Level(%q).Valid() = %v, want %v", tt.level, got, tt.want)
		}
	}
}

// closeTo reports whether a is within 1e-12 of want.
func closeTo(a, want float64) bool {
	d := a - want
	if d < 0 {
		d = -d
	}
	return d <= 1e-12
}

func TestSalience(t *testing.T) {
	tests := []struct {
		name       string
		tier       memory.Tier
		importance float64
		want       float64
	}{
		// At importance 1 salience is exactly the tier weight.
		{"procedural full importance", memory.TierProcedural, 1, 0.95},
		{"semantic full importance", memory.TierSemantic, 1, 0.90},
		{"episodic full importance", memory.TierEpisodic, 1, 0.55},
		{"working full importance", memory.TierWorking, 1, 0.30},
		// At importance 0 the modulation bottoms at half the tier weight.
		{"semantic zero importance", memory.TierSemantic, 0, 0.45},
		// Importance is clamped to [0,1] before modulating.
		{"importance above 1 clamps", memory.TierSemantic, 5, 0.90},
		{"negative importance clamps", memory.TierSemantic, -1, 0.45},
		// Unknown tier falls back to the neutral 0.5 weight.
		{"unknown tier neutral", memory.Tier("bogus"), 1, 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &memory.Memory{Tier: tt.tier, Importance: tt.importance}
			if got := m.Salience(); !closeTo(got, tt.want) {
				t.Errorf("Salience() = %v, want %v", got, tt.want)
			}
		})
	}
}

// preBlendSalience is the Salience formula as it stood before the assessed
// blend: tier weight modulated by stored importance alone. The default-weight
// cases below compare against it so a regression that leaks the assessment in at
// weight 0 fails loudly.
func preBlendSalience(tier memory.Tier, importance float64) float64 {
	w, ok := map[memory.Tier]float64{
		memory.TierProcedural: 0.95,
		memory.TierSemantic:   0.90,
		memory.TierEpisodic:   0.55,
		memory.TierWorking:    0.30,
	}[tier]
	if !ok {
		w = 0.5
	}
	imp := importance
	if imp < 0 {
		imp = 0
	} else if imp > 1 {
		imp = 1
	}
	return w * (0.5 + 0.5*imp)
}

func TestSalienceAssessedBlend(t *testing.T) {
	assessed := func(v float64) *float64 { return &v }

	t.Run("weight 0 is an exact no-op", func(t *testing.T) {
		// The package default must already be 0 — this is the shipped behavior.
		if memory.AssessedSalienceWeight != 0 {
			t.Fatalf("AssessedSalienceWeight default = %v, want 0", memory.AssessedSalienceWeight)
		}
		tiers := []memory.Tier{
			memory.TierProcedural, memory.TierSemantic,
			memory.TierEpisodic, memory.TierWorking, memory.Tier("bogus"),
		}
		for _, tier := range tiers {
			for _, imp := range []float64{-1, 0, 0.3, 0.6, 1, 5} {
				want := preBlendSalience(tier, imp)
				// Without an assessment.
				m := &memory.Memory{Tier: tier, Importance: imp}
				if got := m.Salience(); got != want {
					t.Errorf("tier %q imp %v unassessed: Salience() = %v, want exactly %v", tier, imp, got, want)
				}
				// With an assessment that would move the value if it were read.
				m.AssessedImportance = assessed(1 - imp)
				if got := m.Salience(); got != want {
					t.Errorf("tier %q imp %v assessed: Salience() = %v, want exactly %v (bit-identical)", tier, imp, got, want)
				}
			}
		}
	})

	t.Run("weight 0.3 blends an assessed row", func(t *testing.T) {
		orig := memory.AssessedSalienceWeight
		memory.AssessedSalienceWeight = 0.3
		t.Cleanup(func() { memory.AssessedSalienceWeight = orig })

		// semantic (0.90), importance 0.4, assessed 0.9:
		// imp = 0.7*0.4 + 0.3*0.9 = 0.28 + 0.27 = 0.55
		// salience = 0.90*(0.5 + 0.5*0.55) = 0.90*0.775 = 0.6975
		m := &memory.Memory{
			Tier:               memory.TierSemantic,
			Importance:         0.4,
			AssessedImportance: assessed(0.9),
		}
		if got := m.Salience(); !closeTo(got, 0.6975) {
			t.Errorf("blended Salience() = %v, want 0.6975", got)
		}

		// The assessed value is clamped before blending: 5 behaves as 1.
		// imp = 0.7*0.4 + 0.3*1 = 0.58; salience = 0.90*0.79 = 0.711
		m.AssessedImportance = assessed(5)
		if got := m.Salience(); !closeTo(got, 0.711) {
			t.Errorf("clamped-assessed Salience() = %v, want 0.711", got)
		}
	})

	t.Run("weight 0.3 leaves an unassessed row unchanged", func(t *testing.T) {
		orig := memory.AssessedSalienceWeight
		memory.AssessedSalienceWeight = 0.3
		t.Cleanup(func() { memory.AssessedSalienceWeight = orig })

		for _, imp := range []float64{0, 0.4, 1} {
			m := &memory.Memory{Tier: memory.TierSemantic, Importance: imp}
			want := preBlendSalience(memory.TierSemantic, imp)
			if got := m.Salience(); got != want {
				t.Errorf("imp %v: unassessed Salience() = %v, want exactly %v", imp, got, want)
			}
		}
	})
}

func TestEffectiveImportance(t *testing.T) {
	assessed := func(v float64) *float64 { return &v }
	tests := []struct {
		name       string
		importance float64
		assessed   *float64
		want       float64
	}{
		{"assessed present wins", 0.2, assessed(0.9), 0.9},
		{"assessed present wins when lower", 0.9, assessed(0.2), 0.2},
		{"nil assessed falls back to importance", 0.6, nil, 0.6},
		{"assessed is clamped high", 0.2, assessed(5), 1},
		{"assessed is clamped low", 0.2, assessed(-1), 0},
		{"importance is clamped high", 5, nil, 1},
		{"importance is clamped low", -1, nil, 0},
		// An assessed 0 is a real assessment, not an absent one.
		{"assessed zero is not treated as absent", 0.8, assessed(0), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &memory.Memory{Importance: tt.importance, AssessedImportance: tt.assessed}
			if got := m.EffectiveImportance(); !closeTo(got, tt.want) {
				t.Errorf("EffectiveImportance() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGrowConfidence(t *testing.T) {
	tests := []struct {
		name string
		c    float64
		want float64
	}{
		// Each corroboration closes 10% of the remaining gap to 1.
		{"from zero", 0, 0.1},
		{"representative", 0.4, 0.46},
		{"near cap", 0.9, 0.91},
		{"at cap stays", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := memory.GrowConfidence(tt.c); !closeTo(got, tt.want) {
				t.Errorf("GrowConfidence(%v) = %v, want %v", tt.c, got, tt.want)
			}
		})
	}

	// Repeated growth is strictly increasing and asymptotes below 1.
	c := memory.ConfidenceSeedFresh
	for i := range 100 {
		next := memory.GrowConfidence(c)
		if next <= c {
			t.Fatalf("GrowConfidence not strictly increasing at step %d: %v -> %v", i, c, next)
		}
		if next > 1 {
			t.Fatalf("GrowConfidence overshot 1 at step %d: %v", i, next)
		}
		c = next
	}
	if c < 0.999 {
		t.Errorf("after 100 corroborations confidence = %v, want ~1", c)
	}
}

func TestEffectiveConfidence(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	conf := func(c float64) *float64 { return &c }
	// durable builds a semantic memory whose confidence anchor (UpdatedAt =
	// LastAccessedAt) is `age` before now.
	durable := func(c float64, age time.Duration) *memory.Memory {
		base := now.Add(-age)
		return &memory.Memory{
			Tier: memory.TierSemantic, Confidence: conf(c),
			UpdatedAt: base, LastAccessedAt: base,
		}
	}

	const week = 7 * 24 * time.Hour

	tests := []struct {
		name string
		m    *memory.Memory
		want float64
	}{
		{"short-term tier is neutral", &memory.Memory{
			Tier: memory.TierWorking, Confidence: conf(0.2),
			UpdatedAt: now.Add(-10 * week), LastAccessedAt: now.Add(-10 * week),
		}, 1},
		{"untracked confidence is neutral", &memory.Memory{
			Tier:      memory.TierSemantic,
			UpdatedAt: now.Add(-10 * week), LastAccessedAt: now.Add(-10 * week),
		}, 1},
		// Grace window: no decay for the first week.
		{"inside grace window", durable(0.5, 6*24*time.Hour), 0.5},
		{"exactly one week", durable(0.5, week), 0.5},
		// After the grace week, 0.05 per elapsed week measured from week 1.
		{"two weeks", durable(0.5, 2*week), 0.45},
		{"three weeks", durable(0.5, 3*week), 0.40},
		// Clamped to the floor, never below.
		{"floor clamp", durable(0.1, 200*week), 0.05},
		// Out-of-range stored confidence is clamped before decaying.
		{"stored confidence above 1 clamps", durable(1.5, 2*week), 0.95},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.EffectiveConfidence(now); !closeTo(got, tt.want) {
				t.Errorf("EffectiveConfidence = %v, want %v", got, tt.want)
			}
		})
	}

	// Continuity at the grace-week boundary: one hour past the week costs one
	// hour's worth of decay (0.05/168), not a full week's 0.05 step.
	justPast := durable(0.5, week+time.Hour).EffectiveConfidence(now)
	if want := 0.5 - 0.05/168; !closeTo(justPast, want) {
		t.Errorf("just past grace week = %v, want %v", justPast, want)
	}
	if drop := 0.5 - justPast; drop > 0.001 {
		t.Errorf("discontinuity at grace boundary: dropped %v in one hour", drop)
	}

	// Monotonic non-increasing after the grace window.
	prev := durable(0.5, week).EffectiveConfidence(now)
	for w := 2; w <= 12; w++ {
		cur := durable(0.5, time.Duration(w)*week).EffectiveConfidence(now)
		if cur > prev {
			t.Fatalf("decay not monotonic: week %d = %v > week %d = %v", w, cur, w-1, prev)
		}
		prev = cur
	}

	// A recent recall (LastAccessedAt) re-anchors decay even when the fact
	// itself (UpdatedAt) is old.
	recalled := &memory.Memory{
		Tier: memory.TierSemantic, Confidence: conf(0.5),
		UpdatedAt: now.Add(-10 * week), LastAccessedAt: now.Add(-2 * 24 * time.Hour),
	}
	if got := recalled.EffectiveConfidence(now); got != 0.5 {
		t.Errorf("recently recalled fact decayed: got %v, want 0.5", got)
	}
}

func TestRecency(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	at := func(age time.Duration) *memory.Memory {
		return &memory.Memory{LastAccessedAt: now.Add(-age)}
	}

	tests := []struct {
		name string
		age  time.Duration
		want float64
	}{
		{"accessed now", 0, 1},
		// exp(-age/7d): 1/e per 7 days elapsed.
		{"one time constant (7d)", 7 * 24 * time.Hour, math.Exp(-1)},
		{"two time constants (14d)", 14 * 24 * time.Hour, math.Exp(-2)},
		// A future LastAccessedAt clamps to zero age rather than boosting >1.
		{"future access clamps to 1", -time.Hour, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := at(tt.age).Recency(now); !closeTo(got, tt.want) {
				t.Errorf("Recency(age=%v) = %v, want %v", tt.age, got, tt.want)
			}
		})
	}

	// Strictly decreasing with age.
	if fresh, old := at(24*time.Hour).Recency(now), at(48*time.Hour).Recency(now); fresh <= old {
		t.Errorf("Recency not decreasing: 1d=%v <= 2d=%v", fresh, old)
	}
}

func TestRetentionScore(t *testing.T) {
	// Pin the package default so a stray StabilityK override can't skew the
	// fixed-half-life expectations below.
	orig := memory.StabilityK
	memory.StabilityK = 0
	t.Cleanup(func() { memory.StabilityK = orig })

	now := time.Unix(1_700_000_000, 0).UTC()
	mk := func(tier memory.Tier, importance float64, access int, age time.Duration) *memory.Memory {
		return &memory.Memory{
			Tier: tier, Importance: importance, AccessCount: access,
			CreatedAt: now.Add(-age), UpdatedAt: now.Add(-age), LastAccessedAt: now.Add(-age),
		}
	}

	// Exact values from salience × usage × recency (confidence untracked = 1):
	// fresh working, importance 1: 0.30·1·1·1 = 0.3.
	if got := mk(memory.TierWorking, 1, 0, 0).RetentionScore(now); !closeTo(got, 0.3) {
		t.Errorf("fresh working score = %v, want 0.3", got)
	}
	// episodic, importance 0.5, 3 accesses, 7d old:
	// 0.55·0.75 × (1+ln 4) × e⁻¹ = 0.36212081236623161.
	if got := mk(memory.TierEpisodic, 0.5, 3, 7*24*time.Hour).RetentionScore(now); !closeTo(got, 0.36212081236623161) {
		t.Errorf("episodic score = %v, want 0.36212081236623161", got)
	}

	// RetentionScore is an alias of Quality.
	m := mk(memory.TierEpisodic, 0.7, 5, 36*time.Hour)
	if rs, q := m.RetentionScore(now), m.Quality(now); rs != q {
		t.Errorf("RetentionScore (%v) != Quality (%v)", rs, q)
	}

	// Ordering properties driving eviction: each factor raises the score.
	base := mk(memory.TierEpisodic, 0.5, 2, 24*time.Hour).RetentionScore(now)
	if hi := mk(memory.TierEpisodic, 0.9, 2, 24*time.Hour).RetentionScore(now); hi <= base {
		t.Errorf("higher importance should score higher: %v <= %v", hi, base)
	}
	if hi := mk(memory.TierEpisodic, 0.5, 10, 24*time.Hour).RetentionScore(now); hi <= base {
		t.Errorf("more accesses should score higher: %v <= %v", hi, base)
	}
	if hi := mk(memory.TierEpisodic, 0.5, 2, time.Hour).RetentionScore(now); hi <= base {
		t.Errorf("fresher access should score higher: %v <= %v", hi, base)
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
