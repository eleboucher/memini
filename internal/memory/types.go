// Package memory defines memini's core domain types.
package memory

import (
	"math"
	"strings"
	"time"
)

// Tier classifies a memory by how consolidated it is: working → episodic →
// semantic, with procedural held separately for how-to knowledge.
type Tier string

const (
	// TierWorking holds raw, short-lived observations (typically session-scoped).
	TierWorking Tier = "working"
	// TierEpisodic holds summaries of what happened in a session.
	TierEpisodic Tier = "episodic"
	// TierSemantic holds durable extracted facts ("what I know").
	TierSemantic Tier = "semantic"
	// TierProcedural holds workflows and how-to knowledge.
	TierProcedural Tier = "procedural"
)

// Term is the coarse memory horizon: short-term memories are transient and
// TTL'd; long-term memories are durable and curated.
type Term string

const (
	ShortTerm Term = "short" // working, episodic — transient, decays
	LongTerm  Term = "long"  // semantic, procedural — durable, curated
)

// Term maps a tier to its memory horizon. Working/episodic are short-term;
// semantic/procedural are long-term.
func (t Tier) Term() Term {
	switch t {
	case TierWorking, TierEpisodic:
		return ShortTerm
	default:
		return LongTerm
	}
}

// DefaultTTL is the TTL for a tier; zero means never expires.
func (t Tier) DefaultTTL() time.Duration {
	switch t {
	case TierWorking:
		return 24 * time.Hour
	case TierEpisodic:
		return 90 * 24 * time.Hour
	default: // semantic, procedural: durable
		return 0
	}
}

// Valid reports whether t is a known tier.
func (t Tier) Valid() bool {
	switch t {
	case TierWorking, TierEpisodic, TierSemantic, TierProcedural:
		return true
	default:
		return false
	}
}

// Memory is a single stored memory, scoped to a namespace (tenant/agent).
type Memory struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace"`
	Tier      Tier   `json:"tier"`
	Content   string `json:"content"`
	Summary   string `json:"summary,omitempty"`

	// Metadata is arbitrary structured data, persisted as JSON.
	Metadata map[string]any `json:"metadata,omitempty"`
	// Tags are free-form labels used for keyword retrieval and filtering.
	Tags []string `json:"tags,omitempty"`

	// Importance biases decay and ranking; higher survives longer. Range [0,1].
	Importance float64 `json:"importance"`

	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	LastAccessedAt time.Time  `json:"last_accessed_at"`
	AccessCount    int        `json:"access_count"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`

	// SupersededBy points at the memory that replaced this one during
	// contradiction resolution; non-nil means this record is tombstoned.
	SupersededBy *string `json:"superseded_by,omitempty"`

	// ValidFrom / ValidTo bound the wall-clock interval a fact was true. Both nil
	// means "always" (the common case). ValidTo is stamped when a fact is
	// superseded, so a time-filtered recall (Filter.AsOf) can answer "what was
	// true in March" by surfacing facts valid then even if later replaced.
	ValidFrom *time.Time `json:"valid_from,omitempty"`
	ValidTo   *time.Time `json:"valid_to,omitempty"`

	// Embedding is the dense vector for similarity search. It is required when
	// writing to the store and is omitted from API responses.
	Embedding []float32 `json:"-"`
}

// Expired reports whether the memory has passed its TTL as of now.
func (m *Memory) Expired(now time.Time) bool {
	return m.ExpiresAt != nil && !m.ExpiresAt.After(now)
}

// retentionHalfLife is the time since last access at which the recency factor
// of a memory's retention score halves.
const retentionHalfLife = 7 * 24 * time.Hour

// RetentionScore ranks a memory for short-term eviction (higher = keep longer).
// It rewards importance and access frequency, and decays exponentially with
// time since last access. Used to bound short-term capacity.
func (m *Memory) RetentionScore(now time.Time) float64 {
	age := max(now.Sub(m.LastAccessedAt), 0)
	recency := math.Exp(-float64(age) / float64(retentionHalfLife))
	usage := 1 + math.Log1p(float64(m.AccessCount))
	return (0.2 + m.Importance) * usage * recency
}

// Recency returns an exponentially-decaying [0,1] factor for how recently the
// memory was accessed, halving every retentionHalfLife. Used by recall ranking.
func (m *Memory) Recency(now time.Time) float64 {
	age := max(now.Sub(m.LastAccessedAt), 0)
	return math.Exp(-float64(age) / float64(retentionHalfLife))
}

// NormalizeContent collapses whitespace and case so trivially-duplicated
// memories compare equal. Used by recall dedup and the fsck duplicate audit.
func NormalizeContent(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
