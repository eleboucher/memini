package bench

import "fmt"

// Poison returns a copy of ds with perGroup debris items added to every group
// that has questions — simulating a low-quality bulk import (e.g. a mem0 export
// of restatements) collapsed into the namespace. The debris shares one content
// template so a dedup pass clusters and collapses it, modelling the realistic
// "exports are full of near-duplicates" case. Use it to measure the Recall@K
// delta a poisoned store suffers, and that dedup/curation recover it.
func Poison(ds *Dataset, perGroup int, filler string) *Dataset {
	if perGroup <= 0 {
		return ds
	}
	if filler == "" {
		filler = "TODO: revisit this later, low-signal note from a bulk import"
	}
	groups := map[string]struct{}{}
	for _, q := range ds.Questions {
		groups[q.Group] = struct{}{}
	}
	out := &Dataset{Name: ds.Name + "-poisoned", Questions: ds.Questions}
	out.Items = append(out.Items, ds.Items...)
	for group := range groups {
		for i := range perGroup {
			out.Items = append(out.Items, Item{
				ID:      fmt.Sprintf("debris/%s/%d", group, i),
				Group:   group,
				Content: filler,
			})
		}
	}
	return out
}
