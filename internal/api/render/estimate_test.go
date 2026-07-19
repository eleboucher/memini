package render

import "testing"

// TestApproxTokensRecipePinned pins the server-side token estimator to the
// plugin client's approxTokens recipe byte-for-byte
// (plugin/scripts/_shared.mjs): 0 for empty text, else
// max(1, ceil(words * 4 / 3)) where words is the whitespace-split word count.
// The two must agree so a client passing the same budget it trims by locally
// sees the server drop the same tail — a drifted estimator silently
// over/under-fills every budgeted response.
func TestApproxTokensRecipePinned(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"empty is zero", "", 0},
		// Whitespace-only is truthy for the client (`if (!text) return 0`
		// misses it), so it hits the max(1, ...) floor — replicate that.
		{"whitespace-only floors to one", "   ", 1},
		{"one word", "word", 2},                                            // ceil(4/3) = 2
		{"two words", "two words", 3},                                      // ceil(8/3) = 3
		{"three words", "one two three", 4},                                // ceil(12/3) = 4
		{"six words", "the deploy key lives in vault", 8},                  // ceil(24/3) = 8
		{"mixed whitespace splits like the client regex", "a\tb\nc  d", 6}, // 4 words → ceil(16/3) = 6
	}
	for _, c := range cases {
		if got := ApproxTokens(c.text); got != c.want {
			t.Errorf("%s: ApproxTokens(%q) = %d, want %d", c.name, c.text, got, c.want)
		}
	}
}

// TestItemOverheadTokens pins the per-item render-skeleton overhead the budget
// charges on top of the shipped text (bullet prefix, score, [m:id] handle...).
// A knob, not a law — but changing it changes every budget's fill point, so it
// is pinned like the estimator itself.
func TestItemOverheadTokens(t *testing.T) {
	if ItemOverheadTokens != 10 {
		t.Fatalf("ItemOverheadTokens = %d, want 10", ItemOverheadTokens)
	}
}
