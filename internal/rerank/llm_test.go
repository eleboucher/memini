package rerank

import (
	"context"
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
	r := NewLLM(&fakeCompleter{reply: "3, 1"})
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
	r := NewLLM(&fakeCompleter{reply: "none"})
	got, _ := r.Rerank(context.Background(), "q", cands("a", "b", "c"))
	if len(got) != 0 {
		t.Fatalf("'none' should return empty, got %v", got)
	}
}

func TestRerankIgnoresGarbageAndOutOfRange(t *testing.T) {
	// "99" is out of range, "2" valid; duplicates ignored.
	r := NewLLM(&fakeCompleter{reply: "garbage 2 99 2 banana"})
	got, _ := r.Rerank(context.Background(), "q", cands("a", "b", "c"))
	want := []string{"b"} // only explicitly ranked candidate survives
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestRerankSingleCandidateSkipsLLM(t *testing.T) {
	fc := &fakeCompleter{reply: "should not be called"}
	r := NewLLM(fc)
	got, _ := r.Rerank(context.Background(), "q", cands("only"))
	if len(got) != 1 || got[0] != "only" || fc.lastUser != "" {
		t.Fatalf("single candidate should bypass the LLM, got %v (prompt %q)", got, fc.lastUser)
	}
}
