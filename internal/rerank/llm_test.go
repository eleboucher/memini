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

func TestRerankReordersAndAppendsOmitted(t *testing.T) {
	// Model picks 3 then 1, omits 2 — expect [c,a] (ranked) then [b] (original order).
	r := NewLLM(&fakeCompleter{reply: "3, 1"})
	got, err := r.Rerank(context.Background(), "q", cands("a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"c", "a", "b"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestRerankNoneKeepsOriginalOrder(t *testing.T) {
	r := NewLLM(&fakeCompleter{reply: "none"})
	got, _ := r.Rerank(context.Background(), "q", cands("a", "b", "c"))
	for i, id := range []string{"a", "b", "c"} {
		if got[i] != id {
			t.Fatalf("abstention should preserve original order, got %v", got)
		}
	}
}

func TestRerankIgnoresGarbageAndOutOfRange(t *testing.T) {
	// "99" is out of range, "2" valid; duplicates ignored.
	r := NewLLM(&fakeCompleter{reply: "garbage 2 99 2 banana"})
	got, _ := r.Rerank(context.Background(), "q", cands("a", "b", "c"))
	want := []string{"b", "a", "c"} // 2->b first, then originals a,c
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
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
