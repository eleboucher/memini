package rest_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/memory"
)

// listOut mirrors the ListResponse wire shape for ordering assertions.
type listOut struct {
	Memories []struct {
		ID         string         `json:"id"`
		Namespace  string         `json:"namespace"`
		Content    string         `json:"content"`
		Importance float64        `json:"importance"`
		Metadata   map[string]any `json:"metadata"`
		CreatedAt  time.Time      `json:"created_at"`
	} `json:"memories"`
}

func (l listOut) contents() []string {
	out := make([]string, len(l.Memories))
	for i, m := range l.Memories {
		out[i] = m.Content
	}
	return out
}

// TestListSortAndFilters covers the browse page's server-side sort and the new
// filters, through the generated params and into the store's ORDER BY.
func TestListSortAndFilters(t *testing.T) {
	h := newServer(t)

	// Importance ascends in the order written, so a created_at sort and an
	// importance sort disagree — a handler wired to the wrong column can't pass
	// both assertions below by accident.
	for i, c := range []struct {
		content    string
		importance float64
		memType    string
	}{
		{"first written", 0.1, "decision"},
		{"second written", 0.5, "preference"},
		{"third written", 0.9, ""},
	} {
		body := map[string]any{
			"content": c.content, "tier": string(memory.TierSemantic), "importance": c.importance,
		}
		if c.memType != "" {
			body["metadata"] = map[string]any{"memory_type": c.memType}
		}
		rec := do(t, h, http.MethodPost, "/v1/memories", "acme", apiKey, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed %d: want 201, got %d (%s)", i, rec.Code, rec.Body)
		}
		// Keep created_at strictly ordered; the store's tie-break is the id, which
		// would otherwise decide a same-millisecond race.
		time.Sleep(2 * time.Millisecond)
	}

	get := func(t *testing.T, query string) listOut {
		t.Helper()
		rec := do(t, h, http.MethodGet, "/v1/memories"+query, "acme", apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: want 200, got %d (%s)", query, rec.Code, rec.Body)
		}
		var out listOut
		mustJSON(t, rec, &out)
		return out
	}

	t.Run("default is newest first", func(t *testing.T) {
		got := get(t, "").contents()
		want := []string{"third written", "second written", "first written"}
		if !slicesEqual(got, want) {
			t.Fatalf("default order = %v, want %v (newest created first)", got, want)
		}
	})

	t.Run("sort by importance", func(t *testing.T) {
		got := get(t, "?sort=importance&order=asc").contents()
		want := []string{"first written", "second written", "third written"}
		if !slicesEqual(got, want) {
			t.Fatalf("importance asc = %v, want %v", got, want)
		}
		got = get(t, "?sort=importance&order=desc").contents()
		want = []string{"third written", "second written", "first written"}
		if !slicesEqual(got, want) {
			t.Fatalf("importance desc = %v, want %v", got, want)
		}
	})

	t.Run("sort combines with limit", func(t *testing.T) {
		got := get(t, "?sort=importance&order=asc&limit=1").contents()
		if !slicesEqual(got, []string{"first written"}) {
			t.Fatalf("limit under sort = %v, want the least-important memory only", got)
		}
	})

	t.Run("invalid sort and order are 400s", func(t *testing.T) {
		// The last one is the point of the enum whitelist: an unknown sort key is
		// rejected outright rather than reaching the driver's ORDER BY.
		for _, q := range []string{"?sort=bogus", "?order=sideways", "?sort=created_at%3B%20DROP%20TABLE%20memories"} {
			rec := do(t, h, http.MethodGet, "/v1/memories"+q, "acme", apiKey, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("GET %s: want 400, got %d (%s)", q, rec.Code, rec.Body)
			}
		}
	})

	t.Run("memory_type filter ORs its values", func(t *testing.T) {
		got := get(t, "?memory_type=decision").contents()
		if !slicesEqual(got, []string{"first written"}) {
			t.Fatalf("memory_type=decision = %v", got)
		}
		got = get(t, "?memory_type=decision,preference&sort=created_at&order=asc").contents()
		want := []string{"first written", "second written"}
		if !slicesEqual(got, want) {
			t.Fatalf("memory_type=decision,preference = %v, want %v", got, want)
		}
		// The untyped memory is never swept in by a type filter.
		if got := get(t, "?memory_type=decision,preference,nonesuch").contents(); len(got) != 2 {
			t.Fatalf("an unknown type pulled in extra memories: %v", got)
		}
	})

	t.Run("created_after window", func(t *testing.T) {
		all := get(t, "?sort=created_at&order=asc")
		if len(all.Memories) != 3 {
			t.Fatalf("expected 3 seeded memories, got %d", len(all.Memories))
		}
		// Everything created at or after the second memory: excludes the first.
		cutoff := all.Memories[1].CreatedAt.UTC().Format(time.RFC3339Nano)
		got := get(t, fmt.Sprintf("?created_after=%s&sort=created_at&order=asc", cutoff)).contents()
		want := []string{"second written", "third written"}
		if !slicesEqual(got, want) {
			t.Fatalf("created_after=%s = %v, want %v", cutoff, got, want)
		}
	})

	t.Run("namespace narrowing in all_namespaces mode", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, "/v1/memories", "other", apiKey, map[string]any{
			"content": "a memory elsewhere", "tier": string(memory.TierSemantic),
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed other-namespace: %d (%s)", rec.Code, rec.Body)
		}
		// The aggregate sees both namespaces...
		all := get(t, "?all_namespaces=true")
		if len(all.Memories) != 4 {
			t.Fatalf("all_namespaces returned %d memories, want 4", len(all.Memories))
		}
		// ...and narrows to the ones asked for, without touching the header.
		narrowed := get(t, "?all_namespaces=true&namespace=other")
		if len(narrowed.Memories) != 1 || narrowed.Memories[0].Namespace != "other" {
			t.Fatalf("namespace=other returned %+v, want only the 'other' memory", narrowed.contents())
		}
	})
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
