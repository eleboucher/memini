package maintenance

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// ScrubReport summarizes a content-quality scrub. Counts are the number of
// memories that were (or, in a preview, would be) deleted in each category.
type ScrubReport struct {
	LifecycleNoise  int `json:"lifecycle_noise"`
	ExactDuplicates int `json:"exact_duplicates"`
	Namespaces      int `json:"namespaces"`
}

// Total returns the number of memories removed across all categories.
func (r ScrubReport) Total() int { return r.LifecycleNoise + r.ExactDuplicates }

// lifecycleNoisePrefixes are the session-lifecycle markers some plugins write to
// the store as episodic memories. They record that a session stopped, not any
// fact worth recalling, so a scrub removes them. Matched case-insensitively on
// the normalized (whitespace-collapsed, lowercased) content.
var lifecycleNoisePrefixes = []string{
	"session ended in ",
	"stop checkpoint in ",
}

// isLifecycleNoise reports whether content is a session-lifecycle marker rather
// than a real memory. normalized must be memory.NormalizeContent(content).
func isLifecycleNoise(normalized string) bool {
	for _, p := range lifecycleNoisePrefixes {
		if strings.HasPrefix(normalized, p) {
			return true
		}
	}
	return false
}

// Scrub removes content-level junk that the namespace-oriented doctor fix and
// the embedding-similarity dedup pass both miss: session-lifecycle markers
// ("Session ended", "Stop checkpoint") and exact-duplicate memories (identical
// normalized content within a namespace, keeping the oldest). It previews when
// apply is false, returning the counts that would be removed without mutating
// the store. Live memories only — tombstones are left alone (already excluded
// from recall and reversible). Returns the per-category report.
func Scrub(ctx context.Context, st store.Store, apply bool) (ScrubReport, error) {
	var rep ScrubReport
	namespaces, err := st.ListNamespaces(ctx)
	if err != nil {
		return rep, err
	}
	rep.Namespaces = len(namespaces)

	del := func(ns, id string) error {
		if !apply {
			return nil
		}
		if err := st.Delete(ctx, ns, id); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return nil
	}

	for _, ns := range namespaces {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		mems, err := st.List(ctx, ns, store.Filter{}, 0)
		if err != nil {
			return rep, err
		}
		// Stable order so the surviving duplicate (the oldest) is deterministic.
		sort.Slice(mems, func(i, j int) bool {
			if !mems[i].CreatedAt.Equal(mems[j].CreatedAt) {
				return mems[i].CreatedAt.Before(mems[j].CreatedAt)
			}
			return mems[i].ID < mems[j].ID
		})

		// Key on (tier, normalized content), matching the per-tier write-time
		// dedup: identical content may legitimately coexist across tiers (a
		// promoted semantic fact and its episodic source), and a namespace-wide
		// key would irreversibly delete those.
		seen := make(map[string]struct{}, len(mems))
		for _, m := range mems {
			norm := memory.NormalizeContent(m.Content)
			switch {
			case isLifecycleNoise(norm):
				if err := del(ns, m.ID); err != nil {
					return rep, err
				}
				rep.LifecycleNoise++
			default:
				key := string(m.Tier) + "\x00" + norm
				if _, dup := seen[key]; dup {
					if err := del(ns, m.ID); err != nil {
						return rep, err
					}
					rep.ExactDuplicates++
					continue
				}
				seen[key] = struct{}{}
			}
		}
	}
	return rep, nil
}
