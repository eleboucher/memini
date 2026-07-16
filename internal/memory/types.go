// Package memory defines memini's core domain types.
package memory

import (
	"crypto/sha256"
	"encoding/hex"
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
		return 72 * time.Hour
	case TierEpisodic:
		return 30 * 24 * time.Hour
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

// Level classifies a memory by how it was derived: user-stated versus
// LLM-inferred. Empty string is legacy/unknown (every pre-level row) and
// passes filters unconstrained.
type Level string

const (
	// LevelExplicit is a user-stated or directly-extracted fact, stamped by
	// the heuristic extract-on-write path and by direct RememberInput callers.
	LevelExplicit Level = "explicit"
	// LevelDeduced is an LLM-inferred fact (distilled from episodic material).
	// Traceable to its sources via metadata.source_ids.
	LevelDeduced Level = "deduced"
)

// Valid reports whether l is a known derivation level. Empty (legacy) and any
// unknown value return false.
func (l Level) Valid() bool {
	switch l {
	case LevelExplicit, LevelDeduced:
		return true
	default:
		return false
	}
}

// Memory is a single stored memory, scoped to a namespace.
type Memory struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace"`
	Tier      Tier   `json:"tier"`
	Level     Level  `json:"level,omitempty"`
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

	// Confidence is how corroborated a durable (semantic/procedural) fact is,
	// in [0,1]. It starts low for a fresh or imported fact, grows logistically
	// each time the fact is re-observed, and decays without reinforcement, so a
	// corroborated fact outranks one-off noise. nil means "not tracked" (every
	// short-term memory, and durable memories written before the field existed),
	// treated as fully trusted so existing data is never retroactively penalized.
	Confidence *float64 `json:"confidence,omitempty"`

	// LinkedMemoryIDs references related memories (same entity/topic but distinct
	// facts). Populated by the LLM consolidator when a new memory is related but
	// neither a duplicate nor a contradiction. At recall, IncludeLinked expands
	// results 1-hop via these links. Links are advisory — stale links (target
	// superseded) are resolved at recall time.
	LinkedMemoryIDs []string `json:"linked_memory_ids,omitempty"`

	// Embedding is the dense vector for similarity search. It is required when
	// writing to the store and is omitted from API responses.
	Embedding []float32 `json:"-"`

	// Chunks are per-segment vectors covering content that runs past the
	// per-item embed budget, so recall can match text the Embedding above does
	// not reach. Optional: a store need not implement store.ChunkStore, and
	// short content has none by design (see internal/chunk).
	//
	// On Upsert, nil means the store decides: existing chunk rows are kept
	// when the content is unchanged (same fingerprint) and cleared when it
	// changed. That default fails safe in both directions — stale chunks make
	// recall return a memory whose text no longer contains the passage that
	// matched it, while missing ones are re-created by the backfill loop, so
	// a caller that rewrites content without recomputing loses nothing
	// durable, and a caller that merely stamps metadata cannot wipe an index
	// it never touched. A non-nil slice replaces the rows exactly as given;
	// an empty non-nil slice is an explicit clear, for callers whose chunks
	// went stale without a content change (reembed: the model changed under
	// them).
	Chunks []Chunk `json:"-"`
}

// Chunk is one embedded segment of a memory's content. Idx is its position in
// the content, from 0, so a row is stable across re-splits.
//
// Text is the segment the Embedding was built from. It is stored rather than
// recomputed because it is what the reranker must judge: rerank cuts a
// candidate down to its own budget (300 bytes for the LLM backend, 2048 runes
// for the cross-encoder), so handing it the whole memory means judging a prefix
// that need not contain the passage that retrieved it, and dropping the memory
// chunked recall just found. Recomputing it from content would be possible while
// the split config is unchanged, and wrong the moment it is not.
type Chunk struct {
	Idx       int
	Text      string
	Embedding []float32
}

// Expired reports whether the memory has passed its TTL as of now.
func (m *Memory) Expired(now time.Time) bool {
	return m.ExpiresAt != nil && !m.ExpiresAt.After(now)
}

// PendingEmbedKey/PendingEmbedValue are the metadata flag a degraded write
// carries when it was stored vectorless because the embedder was unreachable;
// the backfill loop re-embeds flagged rows and deletes the key on success.
const (
	PendingEmbedKey   = "pending_embed"
	PendingEmbedValue = "true"
)

// PendingEmbed reports whether this memory is still awaiting its embedding
// (stored vectorless by a degraded write; keyword-retrieval only until the
// backfill clears the flag).
func (m *Memory) PendingEmbed() bool {
	v, _ := m.Metadata[PendingEmbedKey].(string)
	return v == PendingEmbedValue
}

// retentionHalfLife is the time since last access at which the recency factor
// of a memory's retention score halves.
const retentionHalfLife = 7 * 24 * time.Hour

// StabilityK is the spaced-repetition strength (Ebbinghaus stability), read at
// ranking time by Quality. Above 0 it stretches a short-term memory's effective
// half-life with reinforcement — S = retentionHalfLife*(1 + StabilityK*ln(1+
// AccessCount)) — so a frequently-recalled memory forgets more slowly (the curve
// flattens, not just shifts up); at 0 it is an exact no-op (fixed half-life). The
// memini server sets it from MEMINI_STABILITY_K (default 1) in cmd/memini; this
// package-level default stays 0 so direct library callers and unit tests keep
// the unmodulated baseline unless they opt in. See bench/reinforcement_test.go.
var StabilityK = 0.0

// tierSalience is the base quality weight of a memory by tier: a durable fact
// or procedure matters more than a session summary, which matters more than a
// raw scratch observation. It is the salience taxonomy (memini scopes by tier,
// so tier carries the role agentmemory's free-text type does).
var tierSalience = map[Tier]float64{
	TierProcedural: 0.95,
	TierSemantic:   0.90,
	TierEpisodic:   0.55,
	TierWorking:    0.30,
}

// Confidence lifecycle constants.
const (
	// ConfidenceSeedFresh is the starting confidence of a durable fact written
	// or promoted from observed activity (some basis, not yet corroborated).
	ConfidenceSeedFresh = 0.4
	// ConfidenceSeedImported is the starting confidence of an uncorroborated
	// bulk import: lower, so imports must earn trust before outranking facts the
	// agent actually established.
	ConfidenceSeedImported = 0.25
	// ConfidenceDemoteFloor is the corroboration below which an old, never-
	// recalled durable memory is treated as uncorroborated debris (demotion
	// eligibility, and the diagnostic's low-confidence count).
	ConfidenceDemoteFloor  = 0.35
	confidenceDecayPerWeek = 0.05
	confidenceFloor        = 0.05
)

// Salience is the base, time-independent quality of a memory in [0,1]: the
// tier's weight modulated by importance. It does not depend on access or age.
func (m *Memory) Salience() float64 {
	w, ok := tierSalience[m.Tier]
	if !ok {
		w = 0.5 // unknown tier: neutral
	}
	return clamp01(w * (0.5 + 0.5*clamp01(m.Importance)))
}

// EffectiveConfidence is a durable memory's corroboration at now: the stored
// confidence lazily decayed for the weeks elapsed since it was last corroborated
// (UpdatedAt) or recalled (LastAccessedAt). Decay is applied at read time so the
// maintenance sweep needs no extra write. Returns 1 (neutral) for short-term
// memories and for any memory that never had confidence recorded — so existing
// data written before the field is treated as fully trusted, not penalized.
func (m *Memory) EffectiveConfidence(now time.Time) float64 {
	if m.Tier.Term() != LongTerm || m.Confidence == nil {
		return 1
	}
	base := m.UpdatedAt
	if m.LastAccessedAt.After(base) {
		base = m.LastAccessedAt
	}
	weeks := now.Sub(base).Hours() / (7 * 24)
	return decayConfidence(clamp01(*m.Confidence), weeks)
}

// GrowConfidence raises a confidence toward 1 logistically on corroboration:
// each re-observation closes 10% of the remaining gap, so confidence asymptotes
// to 1 and never overshoots.
func GrowConfidence(c float64) float64 { return clamp01(c + 0.1*(1-c)) }

func decayConfidence(c, weeks float64) float64 {
	if weeks <= 1 {
		return c
	}
	// Decay from the end of the grace week so confidence is continuous across the
	// boundary rather than dropping a full week's worth at once.
	return math.Max(confidenceFloor, c-confidenceDecayPerWeek*(weeks-1))
}

// Quality scores a memory for both recall ranking and lifecycle decisions
// (higher = more worth keeping and surfacing). It multiplies the base salience
// by corroboration (confidence), reinforcement (access frequency), and — for
// short-term tiers — recency (exponential decay since last access). Durable
// tiers skip the recency factor: they already age through confidence decay,
// and a 7-day half-life would zero out tier salience for any fact not recalled
// recently, burying core knowledge under fresh session trivia.
func (m *Memory) Quality(now time.Time) float64 {
	if m.Tier.Term() == LongTerm {
		return m.DurableScore(now)
	}
	age := max(now.Sub(m.LastAccessedAt), 0)
	usage := 1 + math.Log1p(float64(m.AccessCount))
	// Stability grows with reinforcement (StabilityK>0); at the default 0 this
	// reduces to the fixed retentionHalfLife, so ranking is unchanged.
	stability := float64(retentionHalfLife) * (1 + StabilityK*math.Log1p(float64(m.AccessCount)))
	recency := math.Exp(-float64(age) / stability)
	return m.Salience() * m.EffectiveConfidence(now) * usage * recency
}

// RetentionScore is the legacy short-term-eviction score, retained as an alias
// of Quality so existing callers keep working; new code should call Quality.
func (m *Memory) RetentionScore(now time.Time) float64 { return m.Quality(now) }

// DurableScore ranks a memory as durable knowledge (e.g. a session briefing):
// salience × corroboration × reinforcement, without Quality's recency decay, so
// a core fact unrecalled for weeks is not buried under fresher trivia.
func (m *Memory) DurableScore(now time.Time) float64 {
	usage := 1 + math.Log1p(float64(m.AccessCount))
	return m.Salience() * m.EffectiveConfidence(now) * usage
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
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

// Fingerprint is a content key for exact-restatement dedup: the SHA-256 of the
// normalized content, so writes differing only in case or whitespace collide.
func Fingerprint(content string) string {
	sum := sha256.Sum256([]byte(NormalizeContent(content)))
	return hex.EncodeToString(sum[:])
}
