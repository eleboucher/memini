package rerank

import (
	"context"
	"strings"
	"testing"
)

// fakeCompleter returns a canned completion and records the prompt it saw.
type fakeCompleter struct {
	reply    string
	lastUser string
}

func (f *fakeCompleter) Complete(_ context.Context, _, user string) (string, error) {
	f.lastUser = user
	return f.reply, nil
}

func cands(ids ...string) []Candidate {
	out := make([]Candidate, len(ids))
	for i, id := range ids {
		out[i] = Candidate{ID: id, Content: "content of " + id}
	}
	return out
}

func TestRerankReordersAndDropsOmitted(t *testing.T) {
	// Model picks 3 then 1, omits 2 — only ranked candidates survive.
	r := NewLLM(&fakeCompleter{reply: "3, 1"}, DefaultLLMMaxChars)
	got, err := r.Rerank(context.Background(), "q", cands("a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"c", "a"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestRerankNoneReturnsEmpty(t *testing.T) {
	r := NewLLM(&fakeCompleter{reply: "none"}, DefaultLLMMaxChars)
	got, _ := r.Rerank(context.Background(), "q", cands("a", "b", "c"))
	if len(got) != 0 {
		t.Fatalf("'none' should return empty, got %v", got)
	}
}

func TestRerankIgnoresGarbageAndOutOfRange(t *testing.T) {
	// "99" is out of range, "2" valid; duplicates ignored.
	r := NewLLM(&fakeCompleter{reply: "garbage 2 99 2 banana"}, DefaultLLMMaxChars)
	got, _ := r.Rerank(context.Background(), "q", cands("a", "b", "c"))
	want := []string{"b"} // only explicitly ranked candidate survives
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestRerankSingleCandidateSkipsLLM(t *testing.T) {
	fc := &fakeCompleter{reply: "should not be called"}
	r := NewLLM(fc, DefaultLLMMaxChars)
	got, _ := r.Rerank(context.Background(), "q", cands("only"))
	if len(got) != 1 || got[0] != "only" || fc.lastUser != "" {
		t.Fatalf("single candidate should bypass the LLM, got %v (prompt %q)", got, fc.lastUser)
	}
}

// TestRerankMaxCharsBounds pins NewLLM's cap contract: a positive cap truncates
// each candidate into the prompt, and 0 disables truncation rather than cutting
// every candidate to nothing — matching CrossEncoder's MaxDocChars.
func TestRerankMaxCharsBounds(t *testing.T) {
	long := strings.Repeat("x", 500)
	for _, tc := range []struct {
		name     string
		maxChars int
		wantFull bool
	}{
		{"positive cap truncates", 100, false},
		{"zero disables truncation", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeCompleter{reply: "1, 2"}
			r := NewLLM(fc, tc.maxChars)
			if _, err := r.Rerank(context.Background(), "q", cands(long, "other")); err != nil {
				t.Fatalf("rerank: %v", err)
			}
			if got := strings.Contains(fc.lastUser, long); got != tc.wantFull {
				t.Fatalf("prompt contains the full candidate = %v, want %v (prompt len %d)",
					got, tc.wantFull, len(fc.lastUser))
			}
		})
	}
}
