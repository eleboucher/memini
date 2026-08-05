package service

import (
	"context"
	"maps"

	"github.com/eleboucher/memini/internal/memory"
)

// UpdateInput describes a partial edit to an existing memory. Every field is
// optional: a nil pointer (or nil slice/map) keeps whatever the stored record
// holds, so a caller enriching one field never has to resend the rest.
//
// The pointers are deliberate. An earlier ""-sentinel form could not tell "omit
// to keep" from "set this to empty", which made a summary impossible to clear.
// A non-nil pointer to the zero value is an explicit write of that value.
type UpdateInput struct {
	Namespace string
	ID        string
	// Home is the caller's personal namespace, and Author the named API key
	// behind the edit — both forwarded to RememberInput. See its docs.
	Home   string
	Author string

	Content *string
	Summary *string
	Tier    *memory.Tier
	Level   *memory.Level
	// Known limitation: an explicit 0 is silently ignored and the stored
	// importance kept, because resolveImportance uses a `!= 0` sentinel on
	// RememberInput.Importance (a float64, not a pointer). Fixing it means
	// pointer-izing that field across every write path. Do not document an
	// update surface as being able to set importance to 0.
	Importance *float64
	Confidence *float64

	// Tags replaces the stored set wholesale: nil keeps it, a non-nil empty
	// slice clears it. Not a *[]string — nil-vs-empty already carries both
	// meanings, and it is the shape both surfaces decode into natively.
	Tags []string

	// Metadata merges into the stored metadata key-by-key rather than replacing
	// it (RFC 7386 style): nil leaves metadata untouched, and an explicit nil
	// value deletes that key. This is the one place Update deliberately differs
	// from RememberInput.Metadata, which replaces wholesale — see mergeMetadata.
	Metadata map[string]any
}

// Update applies a partial edit to an existing memory and returns the stored
// result. Returns store.ErrNotFound when id is absent, and ErrInvalidInput when
// the edit is rejected.
//
// It composes Get + Remember with the current ID (the documented upsert path)
// rather than writing fields directly, so an edit still runs the full write
// lifecycle: validation, secret redaction, and content sanitization all apply to
// updated content exactly as they would to a fresh write. Content is re-embedded
// only when it actually changes (see reusableVector), so a tags- or
// metadata-only edit costs no embedder call. Remember's dedup, consolidation and
// corroborate/contradict routing are fresh-write-only and never fire here.
func (s *Service) Update(ctx context.Context, in UpdateInput) (*memory.Memory, error) {
	cur, err := s.getVerbatim(ctx, in.Namespace, in.ID)
	if err != nil {
		return nil, err
	}
	// Seed every field from the stored record so anything the caller omitted
	// carries over untouched.
	upd := RememberInput{
		Namespace: in.Namespace, Home: in.Home, Author: in.Author, ID: cur.ID,
		Content: cur.Content, Summary: cur.Summary, Tier: cur.Tier,
		Tags: cur.Tags, Metadata: mergeMetadata(cur.Metadata, in.Metadata),
		Importance: cur.Importance, Confidence: cur.Confidence,
		Level: cur.Level, ValidFrom: cur.ValidFrom, ValidTo: cur.ValidTo,
	}
	if in.Content != nil {
		upd.Content = *in.Content
	}
	if in.Summary != nil {
		upd.Summary = *in.Summary
	}
	if in.Tier != nil {
		if !in.Tier.Valid() {
			return nil, invalidInputf("invalid tier %q: want working|episodic|semantic|procedural", *in.Tier)
		}
		upd.Tier = *in.Tier
	}
	if in.Level != nil {
		if !in.Level.Valid() {
			return nil, invalidInputf("invalid level %q: want explicit|deduced", *in.Level)
		}
		upd.Level = *in.Level
	}
	if in.Tags != nil {
		upd.Tags = in.Tags
	}
	if in.Importance != nil {
		upd.Importance = *in.Importance
		// A caller who names an importance overrides the LLM's assessment of it,
		// even when the number matches what is stored — the seeded
		// upd.Importance above makes value equality unreadable on its own.
		upd.ClearAssessedImportance = true
	}
	if in.Confidence != nil {
		upd.Confidence = in.Confidence
	}

	m, err := s.Remember(ctx, upd)
	if err != nil {
		return nil, err
	}
	// The episodic value gate returns (nil, nil) when it drops a low-signal
	// write. For a fresh Remember that is a stored=false result, but here the
	// caller asked to change an existing memory and nothing changed — surface it
	// as an error rather than dereferencing nil or claiming success.
	if m == nil {
		return nil, invalidInputf("update dropped: the new content is below the episodic value gate " +
			"(too short/low-signal); provide more substantive content or set a durable tier")
	}
	return m, nil
}

// mergeMetadata overlays patch onto cur key-by-key, deleting any key whose patch
// value is nil. It always allocates: Remember mutates the Metadata map it is
// given (stampClassifiedTier, scrubInput and embedForRemember all write to it),
// so handing it the map that came off the stored record would let a write scribble
// on its own input.
func mergeMetadata(cur, patch map[string]any) map[string]any {
	out := make(map[string]any, len(cur)+len(patch))
	maps.Copy(out, cur)
	for k, v := range patch {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}
