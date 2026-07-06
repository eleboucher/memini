package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// CodingAgentCategories is the fixed question-category vocabulary for the
// coding-agent-memory suite. The gold audit rejects any other value.
var CodingAgentCategories = map[string]bool{
	"decision":        true,
	"convention":      true,
	"rationale":       true,
	"current-state":   true,
	"synthesis":       true,
	"temporal-update": true,
	"abstention":      true,
}

// codingAgentFile is the on-disk schema: the normalized dataset plus the
// coding-agent extensions (item time/session/source/kind/superseded_by, question
// now/gold_all/provenance) carried as strings the loader parses.
type codingAgentFile struct {
	Name  string `json:"name"`
	Items []struct {
		ID           string `json:"id"`
		Content      string `json:"content"`
		Group        string `json:"group"`
		Time         string `json:"time"`
		Session      string `json:"session"`
		Source       string `json:"source"`
		Kind         string `json:"kind"`
		SupersededBy string `json:"superseded_by"`
	} `json:"items"`
	Questions []struct {
		Query      string   `json:"query"`
		Gold       []string `json:"gold"`
		GoldAll    []string `json:"gold_all"`
		Group      string   `json:"group"`
		Answer     string   `json:"answer"`
		Category   string   `json:"category"`
		Now        string   `json:"now"`
		Provenance string   `json:"provenance"`
	} `json:"questions"`
}

// CodingAgentMeta holds the per-item fields the harness audits but the retrieval
// types (Item) do not carry: item kind and the supersession pointer. Keyed by
// item ID.
type CodingAgentMeta struct {
	Kind         map[string]string
	SupersededBy map[string]string
}

// LoadCodingAgent reads the coding-agent dataset. Unlike the LongMemEval/LoCoMo
// loaders, every item MUST carry a valid RFC3339 time and every question a valid
// now — the suite is temporally ordered, so a missing or malformed timestamp is
// an error, not a silent zero. Items are returned sorted by (time, id) so
// write-mode ingest replays the real chronology deterministically. The returned
// meta carries the audit-only fields (kind, superseded_by).
func LoadCodingAgent(path string) (*Dataset, *CodingAgentMeta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var f codingAgentFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, nil, fmt.Errorf("bench: parse coding-agent dataset: %w", err)
	}
	if len(f.Items) == 0 || len(f.Questions) == 0 {
		return nil, nil, fmt.Errorf("bench: coding-agent dataset %q has no items or questions", path)
	}

	d := &Dataset{Name: f.Name}
	meta := &CodingAgentMeta{Kind: map[string]string{}, SupersededBy: map[string]string{}}
	for _, it := range f.Items {
		t, err := time.Parse(time.RFC3339, it.Time)
		if err != nil {
			return nil, nil, fmt.Errorf("bench: item %q: invalid time %q: %w", it.ID, it.Time, err)
		}
		d.Items = append(d.Items, Item{
			ID: it.ID, Content: it.Content, Group: it.Group, Time: t.UTC(),
			Session: it.Session, Source: it.Source,
		})
		meta.Kind[it.ID] = it.Kind
		if it.SupersededBy != "" {
			meta.SupersededBy[it.ID] = it.SupersededBy
		}
	}
	for qi, q := range f.Questions {
		now, err := time.Parse(time.RFC3339, q.Now)
		if err != nil {
			return nil, nil, fmt.Errorf("bench: question %d (%q): invalid now %q: %w", qi, q.Query, q.Now, err)
		}
		d.Questions = append(d.Questions, Question{
			Query: q.Query, Gold: q.Gold, GoldAll: q.GoldAll, Group: q.Group,
			Answer: q.Answer, Category: q.Category, Now: now.UTC(), Provenance: q.Provenance,
		})
	}

	sort.SliceStable(d.Items, func(i, j int) bool {
		if d.Items[i].Time.Equal(d.Items[j].Time) {
			return d.Items[i].ID < d.Items[j].ID
		}
		return d.Items[i].Time.Before(d.Items[j].Time)
	})
	return d, meta, nil
}
