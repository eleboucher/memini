package service

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// A handoff is the full fresh-session prompt one session writes for the next:
// typically 100-300 lines naming the next step, its prerequisites, constraints
// and reference files. It is stored as an ordinary memory carrying
// maintenance.HandoffTag, which makes it the inverse of a pinned memory —
// never returned by recall, never rendered into a briefing content section,
// surfaced only as a one-line pointer the agent (or the user) chooses to pull.
//
// Three properties follow from "a prompt, not a fact", and each is enforced
// below rather than left to every harness client to re-implement:
//
//   - It defaults to the procedural tier, bypassing the ~400-rune
//     classification cliff that would otherwise park a long prompt in the
//     working tier with a 72h TTL (see extract.ClassifyWith).
//   - It is held out of recall by default, because a 300-line prompt shares
//     lexical surface with almost any query in the project and would crowd
//     out real facts.
//   - It is superseded per SLOT rather than deduped by content similarity, so
//     the previous prompt stays readable through memory_history instead of
//     being overwritten in place or merged with a near-identical sibling.
const (
	// HandoffMemoryType is the metadata["memory_type"] value stamped on every
	// handoff. It rides the existing typed-extraction axis, so the UI's
	// memory-type filter and store.Filter.ExcludeMetadata both address handoffs
	// with no new schema.
	HandoffMemoryType = "handoff"
	// MetaHandoffSlot names the metadata key holding a handoff's slot.
	MetaHandoffSlot = "handoff_slot"
	// MetaHandoffMarker is a fixed key=value stamped on every handoff purely so
	// recall can exclude handoffs without touching metadata["memory_type"].
	// ExcludeMetadata holds one value per key, so excluding on memory_type
	// would silently overwrite the exclusion of a caller that filters on that
	// same key — a dedicated marker makes the two compose instead.
	MetaHandoffMarker = "handoff"
	// handoffMarkerValue is that marker's value; only its presence matters.
	handoffMarkerValue = "1"
	// DefaultHandoffSlot is the slot a write lands in when it names none.
	DefaultHandoffSlot = "main"
	// metaHandoffHarness, metaHandoffLines and metaHandoffCWD are informational
	// provenance a client may set; the server only reads them back for the
	// briefing pointer. metaHandoffConsumedAt / metaHandoffConsumedBy are
	// stamped by whoever resumes a handoff.
	metaHandoffHarness    = "handoff_harness"
	metaHandoffLines      = "handoff_lines"
	metaHandoffConsumedAt = "consumed_at"
	metaHandoffConsumedBy = "consumed_by"
)

const (
	// maxHandoffSlot bounds a slot name so a caller cannot turn the metadata
	// key into a blob store. Sized for the identifiers slots actually hold —
	// branch and worktree names, which routinely run long — and matching the
	// store's own per-segment namespace limit.
	maxHandoffSlot = 200
	// maxHandoffPointers caps how many slots a briefing advertises. Pointers
	// are exempt from the token budget, so without a cap a namespace that
	// accumulates slots (a script slotting by timestamp, say) would grow the
	// injected block without bound, every session. Well above any real number
	// of parallel work lanes; the excess is reported, never silently dropped.
	maxHandoffPointers = 8
	// maxHandoffSummary bounds a pointer's summary. The pointer is not capped
	// the way a memory bullet is — truncating it could cut the id it exists to
	// convey — but the summary itself is caller-supplied text inside a
	// budget-exempt line, so it gets a generous ceiling rather than none.
	maxHandoffSummary = 200
)

// isHandoff reports whether tags carry the reserved handoff tag.
func isHandoff(tags []string) bool {
	return slices.Contains(tags, maintenance.HandoffTag)
}

// handoffSlot returns the slot named in metadata, normalized, or
// DefaultHandoffSlot.
//
// The fallback is a READ-path guard only, for rows written before slots
// existed or edited by hand. Writes are validated up front by
// validateHandoffSlot, which rejects a malformed slot rather than letting it
// land here: silently collapsing an over-long slot to "main" would make that
// write supersede the main slot's handoff, destroying an unrelated session's
// prompt.
func handoffSlot(meta map[string]any) string {
	raw, _ := meta[MetaHandoffSlot].(string)
	slot := strings.TrimSpace(raw)
	if slot == "" || len(slot) > maxHandoffSlot {
		return DefaultHandoffSlot
	}
	return slot
}

// stampHandoff normalizes a handoff write: it records the memory_type and the
// resolved slot so both are queryable, and is a no-op for every other write.
// Mirrors stampClassifiedTier's "mutate in.Metadata in place" convention.
func stampHandoff(in RememberInput) RememberInput {
	if !isHandoff(in.Tags) {
		return in
	}
	if in.Metadata == nil {
		in.Metadata = map[string]any{}
	}
	in.Metadata["memory_type"] = HandoffMemoryType
	in.Metadata[MetaHandoffMarker] = handoffMarkerValue
	in.Metadata[MetaHandoffSlot] = handoffSlot(in.Metadata)
	return in
}

// validateHandoffSlot rejects a malformed slot on a handoff write. A slot is
// load-bearing: it decides which existing handoff this write supersedes, so a
// value that cannot be honored must fail the write rather than be silently
// rewritten to "main" and tombstone another lane's prompt. Failing is safe —
// the caller still holds the prompt and can retry — where guessing is not.
func validateHandoffSlot(in RememberInput) error {
	if !isHandoff(in.Tags) {
		return nil
	}
	raw, ok := in.Metadata[MetaHandoffSlot]
	if !ok || raw == nil {
		return nil // absent: defaults to DefaultHandoffSlot
	}
	slot, isStr := raw.(string)
	if !isStr {
		return invalidInputf("remember: metadata.%s must be a string, got %T", MetaHandoffSlot, raw)
	}
	if trimmed := strings.TrimSpace(slot); trimmed == "" {
		return invalidInputf("remember: metadata.%s is empty; omit it for the %q slot",
			MetaHandoffSlot, DefaultHandoffSlot)
	} else if len(trimmed) > maxHandoffSlot {
		return invalidInputf("remember: metadata.%s is %d bytes, over the %d limit",
			MetaHandoffSlot, len(trimmed), maxHandoffSlot)
	}
	return nil
}

// priorHandoffIDs returns every live handoff this write replaces in its slot,
// which Remember tombstones once the replacement is durably stored. It returns
// nil for a non-handoff write, for an update by ID (rewriting a row in place
// is not a succession), and when the slot is empty — the common first-handoff
// case.
//
// It returns a LIST, not the single newest, so the write reconciles strays.
// The lookup, the insert and the tombstone are three unsynchronized steps, so
// two sessions writing the same slot concurrently can both see predecessor C,
// both insert, and leave two live handoffs with neither superseding the other.
// The briefing then shows the newer and the other is orphaned — live, but
// invisible and never reachable through history. Superseding all of them means
// the next write to that slot cleans up after such a race instead of layering
// another stray on top. It does not prevent the race itself; a real fix needs
// a compare-and-set the store does not offer today.
//
// A lookup failure returns nil rather than an error: losing the tombstone
// leaves an extra live handoff, which the next write reconciles, while failing
// the write would lose the prompt itself.
func (s *Service) priorHandoffIDs(ctx context.Context, m *memory.Memory, in RememberInput) []string {
	if in.ID != "" || !isHandoff(m.Tags) {
		return nil
	}
	prev, err := s.liveHandoffs(ctx, m.Namespace, handoffSlot(m.Metadata))
	if err != nil {
		slog.WarnContext(ctx, "handoff: predecessor lookup failed",
			"namespace", m.Namespace, "err", err)
		return nil
	}
	ids := make([]string, 0, len(prev))
	for _, p := range prev {
		if p.ID != m.ID {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// liveHandoffs returns the live handoffs in a namespace and slot, newest
// first. store.Filter's zero value already excludes expired and superseded
// rows, and List defaults to newest-first, so the slot's current handoff is
// the first result and anything after it is a stray from an earlier race.
func (s *Service) liveHandoffs(ctx context.Context, namespace, slot string) ([]*memory.Memory, error) {
	return s.store.List(ctx, namespace, store.Filter{
		Now:      s.now(),
		Tags:     []string{maintenance.HandoffTag},
		Metadata: map[string]string{MetaHandoffSlot: slot},
	}, 0)
}

// HandoffPointer is the one-line index entry a briefing carries for a slot's
// current handoff. It deliberately excludes Content: the whole point of the
// pointer is that a 100-300 line prompt is fetched on demand (memory_get ID)
// rather than injected into every session's context.
type HandoffPointer struct {
	ID        string    `json:"id"`
	Slot      string    `json:"slot"`
	Summary   string    `json:"summary,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Harness names the agent harness that wrote the prompt (claude-code,
	// cursor, ...), so a session in a different harness knows before it pulls
	// that parts of the prompt may not apply to it.
	Harness string `json:"harness,omitempty"`
	// Lines is the prompt's length as the writer counted it, so a reader can
	// judge the context cost of pulling it.
	Lines int `json:"lines,omitempty"`
	// ConsumedAt / ConsumedBy record that some session already resumed this
	// handoff. It stays listed regardless — a resumed handoff is still the
	// truth about where the work stands, and hiding it would silently strand
	// a session that was interrupted mid-resume.
	ConsumedAt string `json:"consumed_at,omitempty"`
	ConsumedBy string `json:"consumed_by,omitempty"`
}

// handoffPointers returns one pointer per slot with a live handoff in the
// namespace, newest-first within a slot and ordered by slot name.
//
// It reads the primary namespace only, never the ancestor/home/link cascade a
// briefing otherwise draws on: a handoff is "where THIS project's work stands"
// and an inherited one would tell a session to resume work in another repo.
//
// A failure is logged and yields no pointers. The briefing is still worth
// delivering without them, and the sections it does carry are unaffected.
func (s *Service) handoffPointers(ctx context.Context, namespace string) []HandoffPointer {
	mems, err := s.store.List(ctx, namespace, store.Filter{
		Now:  s.now(),
		Tags: []string{maintenance.HandoffTag},
	}, 0)
	if err != nil {
		slog.WarnContext(ctx, "briefing: handoff pointers unavailable",
			"namespace", namespace, "err", err)
		return nil
	}
	bySlot := make(map[string]*memory.Memory, len(mems))
	for _, m := range mems {
		slot := handoffSlot(m.Metadata)
		// List returns newest-first, so the first sighting of a slot is its
		// current handoff; later ones are stragglers a failed tombstone left
		// behind.
		if _, seen := bySlot[slot]; !seen {
			bySlot[slot] = m
		}
	}
	out := make([]HandoffPointer, 0, len(bySlot))
	for slot, m := range bySlot {
		out = append(out, newHandoffPointer(slot, m))
	}
	slices.SortFunc(out, func(a, b HandoffPointer) int { return strings.Compare(a.Slot, b.Slot) })
	if len(out) > maxHandoffPointers {
		// Pointers bypass the token budget, so this cap is the only thing
		// bounding the block. Logged rather than dropped silently: a namespace
		// over the cap has a slot-naming problem worth seeing.
		slog.WarnContext(ctx, "briefing: handoff slots over cap, listing the first by slot name",
			"namespace", namespace, "slots", len(out), "cap", maxHandoffPointers)
		out = out[:maxHandoffPointers]
	}
	return out
}

// newHandoffPointer projects a handoff memory onto its briefing pointer.
func newHandoffPointer(slot string, m *memory.Memory) HandoffPointer {
	p := HandoffPointer{
		ID:         m.ID,
		Slot:       slot,
		Summary:    truncRunes(m.Summary, maxHandoffSummary),
		CreatedAt:  m.CreatedAt,
		Harness:    metaString(m.Metadata, metaHandoffHarness),
		Lines:      metaInt(m.Metadata, metaHandoffLines),
		ConsumedAt: metaString(m.Metadata, metaHandoffConsumedAt),
		ConsumedBy: metaString(m.Metadata, metaHandoffConsumedBy),
	}
	return p
}

// truncRunes caps s at n runes, rune-safe so an astral character at the
// boundary is never split, appending an ellipsis only when it actually cut.
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// metaString reads a top-level string metadata value, or "" when absent or of
// another type.
func metaString(meta map[string]any, key string) string {
	v, _ := meta[key].(string)
	return v
}

// metaInt reads a top-level numeric metadata value. JSON round-trips numbers
// as float64, but a value set in-process arrives as int, so both are accepted.
func metaInt(meta map[string]any, key string) int {
	switch v := meta[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}

// shouldConsolidate reports whether a write goes through the LLM consolidation
// pipeline (dedup / contradiction resolution against existing memories).
//
// A handoff never does. Consolidation judges a write as a claim about the
// world that may restate or contradict a stored claim; a handoff is an
// instruction addressed to the next session. Two consecutive handoffs for the
// same project are near-identical by construction — same constraints, same
// reference paths, a different next step — so a content-similarity pipeline
// reads the pair as a restatement and would merge or rewrite the very lines
// that differ. Slot supersession expresses the same "the new one replaces the
// old one" intent exactly, and losslessly.
func (s *Service) shouldConsolidate(in RememberInput, m *memory.Memory, durable bool) bool {
	if isHandoff(m.Tags) {
		return false
	}
	return in.ID == "" && s.consolidator != nil && durable && s.consolidateMode != ConsolidateOff
}

// shouldSplitDedup reports whether the non-LLM write-time dedup gate runs. It
// excludes handoffs for the reason given on shouldConsolidate — near-identical
// successive prompts are the expected case, not corpus debris — and because
// the dedup gate picks its own supersede target, which would fight the
// slot-scoped one priorHandoffID already chose.
func shouldSplitDedup(in RememberInput, m *memory.Memory, consolidate bool) bool {
	return in.ID == "" && !consolidate && !isHandoff(m.Tags)
}

// handoffExclusion is the store filter clause that drops handoffs. Each
// ExcludeMetadata pair is an independent "drop rows carrying this pair" in
// both backends, so merging it into a caller's exclusions narrows the result
// set without disturbing the caller's own pairs.
func handoffExclusion() map[string]string {
	return map[string]string{MetaHandoffMarker: handoffMarkerValue}
}

// recallExcludeMetadata returns the metadata exclusions a recall runs with:
// the caller's own, plus the handoff exclusion unless the caller opted in.
// The caller's map is never mutated — RecallInput is copied by value but the
// map inside it is shared with whatever built the request, and a long-lived
// client that reuses one options struct would otherwise accumulate exclusions
// it never asked for.
func recallExcludeMetadata(in RecallInput) map[string]string {
	if in.IncludeHandoffs {
		return in.ExcludeMetadata
	}
	out := make(map[string]string, len(in.ExcludeMetadata)+1)
	maps.Copy(out, in.ExcludeMetadata)
	maps.Copy(out, handoffExclusion())
	return out
}
