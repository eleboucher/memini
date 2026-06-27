package importer

import (
	"maps"

	"github.com/eleboucher/memini/internal/extract"
	"github.com/eleboucher/memini/internal/memory"
)

// ExtractTyped derives durable memories from conversation records (the
// claude-code episodic exchanges): decisions and problems as semantic facts,
// preferences as procedural how-to. Each extraction keeps the source's namespace
// and timestamp, is tagged with its kind, and gets a content-addressed ID via
// finalizeRecords so re-imports stay idempotent. The originals are left
// untouched — callers append the result. The heuristic itself lives in
// internal/extract, shared with write-time extraction.
func ExtractTyped(recs []Record) []Record {
	var out []Record
	for _, r := range recs {
		// Only conversational episodic records carry extractable prose. Skip
		// durable or already-typed records (e.g. a memini re-import of prior
		// extractions) so we don't re-classify and duplicate them.
		if r.Tier != memory.TierEpisodic {
			continue
		}
		if _, typed := r.Metadata["memory_type"]; typed {
			continue
		}
		for _, ex := range extract.Typed(r.Content) {
			meta := make(map[string]any, len(r.Metadata)+1)
			maps.Copy(meta, r.Metadata)
			meta["memory_type"] = string(ex.Kind)
			out = append(out, Record{
				Namespace: r.Namespace,
				Tier:      ex.Kind.Tier(),
				Content:   truncateRunes(ex.Content, maxExchangeBytes),
				Tags:      []string{string(ex.Kind)},
				Metadata:  meta,
				CreatedAt: r.CreatedAt,
			})
		}
	}
	return out
}
