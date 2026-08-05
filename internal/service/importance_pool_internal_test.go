package service

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/rerank"
	"github.com/eleboucher/memini/internal/store"
)

// impPool builds a relevance-ordered candidate list with explicit scores and
// stored importances; ids are the position ('a', 'b', ...) so assertions read as
// the resulting order.
func impPool(scores, imps []float64) []store.Scored {
	out := make([]store.Scored, len(scores))
	for i := range scores {
		out[i] = store.Scored{
			Score:  scores[i],
			Memory: &memory.Memory{ID: string(rune('a' + i)), Importance: imps[i]},
		}
	}
	return out
}

func TestReserveImportantPool(t *testing.T) {
	// A canonical score ladder shared by most cases: strictly descending, with
	// enough headroom that the relevance gate passes unless a case says
	// otherwise.
	ladder := []float64{0.9, 0.85, 0.8, 0.75, 0.7, 0.65, 0.6, 0.55}

	tests := []struct {
		name             string
		scores, imps     []float64
		k, poolSize      int
		reserve          int
		minImp           float64
		want             string // full resulting order
		wantPoolContains string // ids that must be inside deduped[:poolSize]
	}{
		{
			name:   "reserve 0 is a no-op",
			scores: ladder, imps: []float64{.1, .1, .1, .1, .1, .9, .9, .9},
			k: 2, poolSize: 5, reserve: 0, minImp: 0.75,
			want: "abcdefgh",
		},
		{
			name:   "pool no deeper than k is a no-op",
			scores: ladder, imps: []float64{.1, .1, .1, .1, .1, .9, .9, .9},
			k: 5, poolSize: 5, reserve: 2, minImp: 0.75,
			want: "abcdefgh",
		},
		{
			name:   "list no deeper than the pool is a no-op",
			scores: ladder[:5], imps: []float64{.1, .1, .1, .9, .9},
			k: 2, poolSize: 5, reserve: 2, minImp: 0.75,
			want: "abcde",
		},
		{
			name:   "reserve already satisfied inside the pool is unchanged",
			scores: ladder, imps: []float64{.1, .1, .9, .9, .1, .9, .1, .1},
			k: 2, poolSize: 5, reserve: 2, minImp: 0.75,
			want: "abcdefgh",
		},
		{
			name:   "one buried important candidate is promoted into the pool",
			scores: ladder, imps: []float64{.1, .1, .1, .1, .1, .9, .1, .1},
			k: 2, poolSize: 5, reserve: 1, minImp: 0.75,
			// 'f' (0.65) clears max(0.5*0.7, 0.4*0.9) = 0.36 and swaps with the
			// lowest-scoring band entry 'e'.
			want: "abcdfegh", wantPoolContains: "f",
		},
		{
			name:   "two promotions fill the reserve, evicting the band bottom up",
			scores: ladder, imps: []float64{.1, .1, .1, .1, .1, .9, .1, .8},
			k: 2, poolSize: 5, reserve: 2, minImp: 0.75,
			// 'f' takes 'e' (band bottom); 'h' (0.55) then clears
			// max(0.5*0.75, 0.4*0.9) = 0.375 against the next evictable band
			// entry 'd'. Promoted entries are never re-evicted.
			want: "abchfegd", wantPoolContains: "fh",
		},
		{
			name:   "relevance gate stops the walk",
			scores: []float64{0.9, 0.85, 0.8, 0.75, 0.7, 0.2, 0.15, 0.1},
			imps:   []float64{.1, .1, .1, .1, .1, .9, .9, .9},
			k:      2, poolSize: 5, reserve: 2, minImp: 0.75,
			// 0.2 < max(0.5*0.7, 0.4*0.9) = 0.36. The list is score-ordered, so
			// 'g' and 'h' are not even considered.
			want: "abcdefgh",
		},
		{
			name:   "top anchor holds on a low-signal band",
			scores: []float64{1.0, 0.2, 0.15, 0.12, 0.1, 0.09, 0.08, 0.07},
			imps:   []float64{.1, .1, .1, .1, .1, .9, .9, .9},
			k:      2, poolSize: 5, reserve: 1, minImp: 0.75,
			// The evictee leg alone (0.5*0.1 = 0.05) would admit 'f'; the anchor
			// leg (0.4*1.0 = 0.4) holds.
			want: "abcdefgh",
		},
		{
			name:   "unimportant prefix entries are never evicted",
			scores: []float64{0.9, 0.85, 0.8, 0.75, 0.7, 0.65},
			imps:   []float64{.1, .1, .9, .9, .9, .9},
			k:      2, poolSize: 4, reserve: 3, minImp: 0.75,
			// The band [2,4) is already all-important, so the only evictable
			// entries left are the two low-importance ones in [0,k) — off limits.
			want: "abcdef",
		},
		{
			name:   "minImp is a floor, not a strict threshold",
			scores: ladder, imps: []float64{.1, .1, .1, .1, .1, .75, .1, .1},
			k: 2, poolSize: 5, reserve: 1, minImp: 0.75,
			want: "abcdfegh", wantPoolContains: "f",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := impPool(tt.scores, tt.imps)
			orig := slices.Clone(in)
			got := reserveImportantPool(in, tt.k, tt.poolSize, tt.reserve, tt.minImp,
				defaultReservePromoteRatio, defaultReserveTopAnchor)

			if ids(got) != tt.want {
				t.Fatalf("order %q, want %q", ids(got), tt.want)
			}
			// THE invariant: the [0,k) prefix is byte-identical to the input's,
			// which is what keeps every non-rerank fallback unchanged.
			if !reflect.DeepEqual(got[:tt.k], orig[:tt.k]) {
				t.Fatalf("top-k prefix changed: got %q, want %q", ids(got[:tt.k]), ids(orig[:tt.k]))
			}
			// The helper must not mutate the caller's slice.
			if !reflect.DeepEqual(in, orig) {
				t.Fatalf("input mutated: %q, want %q", ids(in), ids(orig))
			}
			if tt.wantPoolContains != "" {
				inPool := ids(got[:tt.poolSize])
				for _, c := range tt.wantPoolContains {
					if !slices.Contains([]rune(inPool), c) {
						t.Fatalf("pool %q missing promoted candidate %q", inPool, string(c))
					}
				}
			}
		})
	}
}

func TestReserveImportantPoolUsesAssessedImportance(t *testing.T) {
	// Stored importance is uniformly low; only the LLM's assessment makes 'f'
	// eligible, so promotion proves EffectiveImportance (not Importance) drives
	// the reserve.
	in := impPool(
		[]float64{0.9, 0.85, 0.8, 0.75, 0.7, 0.65, 0.6, 0.55},
		[]float64{.1, .1, .1, .1, .1, .1, .1, .1},
	)
	assessed := 0.95
	in[5].Memory.AssessedImportance = &assessed

	got := reserveImportantPool(in, 2, 5, 1, 0.75, defaultReservePromoteRatio, defaultReserveTopAnchor)
	if ids(got) != "abcdfegh" {
		t.Fatalf("order %q, want %q", ids(got), "abcdfegh")
	}
}

// stubReranker keeps the pool order it is given and records the candidate IDs it
// saw, so a test can assert on rerank-pool MEMBERSHIP.
type stubReranker struct {
	seen []string
	err  error
}

func (r *stubReranker) Rerank(_ context.Context, _ string, c []rerank.Candidate) ([]string, error) {
	r.seen = r.seen[:0]
	for _, cand := range c {
		r.seen = append(r.seen, cand.ID)
	}
	if r.err != nil {
		return nil, r.err
	}
	return slices.Clone(r.seen), nil
}

// finalizePool is the candidate list finalizeRecall's tests operate on: eight
// score-ordered memories with a single high-assessed one buried at position 6,
// well below a pool cut of 5.
func finalizePool() []store.Scored {
	in := impPool(
		[]float64{0.9, 0.85, 0.8, 0.75, 0.7, 0.65, 0.6, 0.55},
		[]float64{.1, .1, .1, .1, .1, .1, .9, .1},
	)
	for i := range in {
		in[i].Memory.Content = in[i].Memory.ID // distinct content: no dedup collisions
	}
	return in
}

func finalizeSvc(rr rerank.Reranker, reserve int) *Service {
	return &Service{
		reranker:              rr,
		rerankName:            "stub",
		rerankPool:            5,
		importancePoolReserve: reserve,
		importancePoolMin:     defaultImportancePoolMin,
		reservePromoteRatio:   defaultReservePromoteRatio,
		reserveTopAnchor:      defaultReserveTopAnchor,
		metrics:               nopMetrics{},
	}
}

func TestFinalizeRecallImportanceReservePromotesIntoRerankPool(t *testing.T) {
	ctx := context.Background()

	off := &stubReranker{}
	if got := finalizeSvc(off, 0).finalizeRecall(ctx, "q", finalizePool(), 3); len(got) != 3 {
		t.Fatalf("reserve off: got %d results, want 3", len(got))
	}
	if slices.Contains(off.seen, "g") {
		t.Fatalf("reserve off: %q reached the reranker without a reserve", off.seen)
	}

	on := &stubReranker{}
	got := finalizeSvc(on, 2).finalizeRecall(ctx, "q", finalizePool(), 3)
	if !slices.Contains(on.seen, "g") {
		t.Fatalf("reserve on: buried high-importance candidate never reached the reranker: %q", on.seen)
	}
	if len(on.seen) != 5 {
		t.Fatalf("reserve on: pool size %d, want 5 (%q)", len(on.seen), on.seen)
	}
	// The reranker echoed pool order, so the served top-3 is the composite
	// prefix — the reserve only swapped inside [k, poolSize).
	if ids(got) != "abc" {
		t.Fatalf("reserve on: served %q, want %q", ids(got), "abc")
	}
}

func TestFinalizeRecallImportanceReserveFallbackIsUnchanged(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")

	off := finalizeSvc(&stubReranker{err: boom}, 0).finalizeRecall(ctx, "q", finalizePool(), 3)
	on := finalizeSvc(&stubReranker{err: boom}, 2).finalizeRecall(ctx, "q", finalizePool(), 3)

	if ids(off) != "abc" {
		t.Fatalf("feature-off fallback %q, want %q", ids(off), "abc")
	}
	// Byte-identical, not merely same-length: a rerank failure must return
	// exactly the composite order it returns with the reserve disabled.
	if len(on) != len(off) {
		t.Fatalf("fallback length %d, want %d", len(on), len(off))
	}
	for i := range on {
		if on[i].Memory.ID != off[i].Memory.ID || on[i].Score != off[i].Score {
			t.Fatalf("fallback diverged at %d: %q/%v vs %q/%v",
				i, on[i].Memory.ID, on[i].Score, off[i].Memory.ID, off[i].Score)
		}
	}
}

// The reserve must not resurrect a duplicate that Dedup already dropped: it runs
// on the deduped list, so a promoted candidate can never be a repeat of a pool
// entry.
func TestFinalizeRecallImportanceReserveDedupsFirst(t *testing.T) {
	in := finalizePool()
	in[6].Memory.Content = in[2].Memory.Content // 'g' now duplicates 'c'

	rr := &stubReranker{}
	got := finalizeSvc(rr, 2).finalizeRecall(context.Background(), "q", in, 3)
	if slices.Contains(rr.seen, "g") {
		t.Fatalf("duplicate candidate reached the reranker: %q", rr.seen)
	}
	if ids(got) != "abc" {
		t.Fatalf("served %q, want %q", ids(got), "abc")
	}
}
