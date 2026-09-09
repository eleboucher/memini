package rest_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// handoffWire is the slice of the briefing wire shape these tests care about:
// the handoff pointer index plus the section a handoff must never appear in.
type handoffWire struct {
	Handoffs []struct {
		Id         string `json:"id"`
		Slot       string `json:"slot"`
		Summary    string `json:"summary"`
		Harness    string `json:"harness"`
		Lines      int    `json:"lines"`
		ConsumedAt string `json:"consumed_at"`
		ConsumedBy string `json:"consumed_by"`
	} `json:"handoffs"`
	Procedures []struct {
		Memory struct {
			Id string `json:"id"`
		} `json:"memory"`
	} `json:"procedures"`
}

// handoffPrompt is a stand-in for a real handoff: well past the ~400-rune
// classification ceiling, so it exercises the tier cliff rather than a toy
// string that would classify normally.
func handoffPrompt(title string) string {
	var b strings.Builder
	b.WriteString("# " + title + "\n\n")
	for range 60 {
		b.WriteString("Read the plan, seed the todo list in dependency order, keep the helper.\n")
	}
	return b.String()
}

// postHandoff writes one handoff over the real HTTP surface and returns its id.
func postHandoff(t *testing.T, h http.Handler, ns, slot, title string) string {
	t.Helper()
	meta := map[string]any{"handoff_harness": "claude-code", "handoff_lines": 212}
	if slot != "" {
		meta["handoff_slot"] = slot
	}
	rec := do(t, h, http.MethodPost, "/v1/memories", ns, apiKey, map[string]any{
		"content":  handoffPrompt(title),
		"summary":  title,
		"tags":     []string{"handoff"},
		"metadata": meta,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("post handoff %q: want 201, got %d (%s)", title, rec.Code, rec.Body)
	}
	var out struct {
		Id   string `json:"id"`
		Tier string `json:"tier"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode handoff write: %v", err)
	}
	if out.Tier != "procedural" {
		t.Errorf("handoff stored as tier %q, want procedural — a long prompt must not expire", out.Tier)
	}
	return out.Id
}

// TestBriefingHandoffPointers pins the REST wire contract: a handoff reaches a
// client as a pointer in its own top-level array, carrying the provenance
// needed to decide whether to fetch it, and never as an item in a content
// section.
func TestBriefingHandoffPointers(t *testing.T) {
	h := newServer(t)
	const ns = "alice"

	postHandoff(t, h, ns, "wt-polygons", "polygon work")
	mainID := postHandoff(t, h, ns, "", "execute stage 4")

	rec := do(t, h, http.MethodGet, "/v1/namespaces/briefing", ns, apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("briefing: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var got handoffWire
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode briefing: %v", err)
	}

	if len(got.Handoffs) != 2 {
		t.Fatalf("got %d pointers, want one per slot: %s", len(got.Handoffs), rec.Body)
	}
	if got.Handoffs[0].Slot != "main" || got.Handoffs[1].Slot != "wt-polygons" {
		t.Errorf("pointers not ordered by slot: %q then %q", got.Handoffs[0].Slot, got.Handoffs[1].Slot)
	}
	p := got.Handoffs[0]
	if p.Id != mainID || p.Summary != "execute stage 4" || p.Harness != "claude-code" || p.Lines != 212 {
		t.Errorf("pointer lost fields on the wire: %+v", p)
	}
	if p.ConsumedAt != "" || p.ConsumedBy != "" {
		t.Errorf("an unconsumed handoff reported consumption: %+v", p)
	}
	for _, proc := range got.Procedures {
		if proc.Memory.Id == mainID {
			t.Fatal("a handoff appeared in the procedures section; it must ship only as a pointer")
		}
	}
}

// TestSearchExcludesHandoffs pins the recall contract over HTTP: the default
// search never returns a handoff, and include_handoffs is the only way in.
func TestSearchExcludesHandoffs(t *testing.T) {
	h := newServer(t)
	const ns = "alice"
	id := postHandoff(t, h, ns, "", "execute stage 4")

	search := func(body map[string]any) []string {
		t.Helper()
		rec := do(t, h, http.MethodPost, "/v1/search", ns, apiKey, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("search: want 200, got %d (%s)", rec.Code, rec.Body)
		}
		var out struct {
			Results []struct {
				Memory struct {
					Id string `json:"id"`
				} `json:"memory"`
			} `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode search: %v", err)
		}
		ids := make([]string, 0, len(out.Results))
		for _, r := range out.Results {
			ids = append(ids, r.Memory.Id)
		}
		return ids
	}

	for _, got := range search(map[string]any{"query": "execute stage 4", "limit": 10}) {
		if got == id {
			t.Fatal("default search returned a handoff; it must be reached through the pointer")
		}
	}

	opted := search(map[string]any{"query": "execute stage 4", "limit": 10, "include_handoffs": true})
	found := false
	for _, got := range opted {
		if got == id {
			found = true
		}
	}
	if !found {
		t.Error("include_handoffs did not bring the handoff back; the opt-in is the only way to search them")
	}
}
