package chunk

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// tiny keeps the cases readable: the real defaults are 1200/200, which would
// make every fixture a wall of text.
func tiny() Config { return Config{Size: 20, Overlap: 5, MinContent: 20, MaxChunks: 64} }

func TestShortContentProducesNoChunks(t *testing.T) {
	cfg := tiny()
	for _, tc := range []struct {
		name string
		text string
	}{
		{"empty", ""},
		{"well under", "short"},
		{"exactly at MinContent", strings.Repeat("x", cfg.MinContent)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Not "one chunk" — none. A lone chunk of a short memory duplicates
			// its own document vector, so chunk rows exist only where the
			// document vector is actually falling short.
			if got := Split(tc.text, cfg); len(got.Chunks) != 0 {
				t.Fatalf("Split(%q) = %d chunks, want 0", tc.text, len(got.Chunks))
			}
		})
	}
}

func TestJustOverMinContentSplits(t *testing.T) {
	cfg := tiny()
	got := Split(strings.Repeat("x", cfg.MinContent+1), cfg)
	if len(got.Chunks) == 0 {
		t.Fatal("content one rune over MinContent produced no chunks")
	}
}

// TestNoChunkExceedsSize is the invariant that makes chunking safe: a chunk
// longer than the embedder's budget would itself be truncated, reintroducing
// the exact bug chunking exists to fix.
func TestNoChunkExceedsSize(t *testing.T) {
	cfg := tiny()
	for _, text := range []string{
		strings.Repeat("word ", 200),
		strings.Repeat("a", 500),                         // no separator anywhere
		strings.Repeat("para one.\n\npara two.\n\n", 30), // paragraph-heavy
		strings.Repeat("这个部署流水线在十六个核心上并行运行测试。", 20), // no spaces at all
		strings.Repeat("👍", 300),                     // astral, 2 UTF-16 units each
		"a\n\n" + strings.Repeat("b", 100) + "\n\nc", // one long unbreakable middle
	} {
		for _, c := range Split(text, cfg).Chunks {
			if n := utf8.RuneCountInString(c); n > cfg.Size {
				t.Fatalf("chunk of %d runes exceeds Size=%d: %q", n, cfg.Size, c)
			}
		}
	}
}

// TestCoversAllContent pins that splitting loses nothing: every non-space rune
// of the input must appear in some chunk, or recall silently cannot reach it.
func TestCoversAllContent(t *testing.T) {
	cfg := tiny()
	for _, text := range []string{
		strings.Repeat("word ", 100),
		strings.Repeat("a", 300),
		"first para.\n\nsecond para is a bit longer than the first one.\n\nthird.",
		strings.Repeat("这个部署流水线在十六个核心上并行运行测试。", 10),
	} {
		joined := strings.Join(Split(text, cfg).Chunks, "")
		got := stripSpace(joined)
		want := stripSpace(text)
		// Overlap repeats text, so `got` is a superset of `want` and the two are
		// not comparable by equality or substring — every input rune must appear
		// in order, which is exactly "nothing was dropped".
		if !isSubsequence(want, got) {
			t.Fatalf("chunks dropped content\n want (as subsequence)=%q\n got=%q", want, got)
		}
	}
}

func TestChunksOverlap(t *testing.T) {
	cfg := tiny()
	got := Split(strings.Repeat("abcde ", 40), cfg)
	if len(got.Chunks) < 2 {
		t.Fatalf("want >= 2 chunks to compare, got %d", len(got.Chunks))
	}
	// A fact straddling a boundary must survive whole in one chunk; the tail of
	// each chunk should therefore reappear in its successor.
	tail := lastRunes(got.Chunks[0], 3)
	if !strings.Contains(got.Chunks[1], tail) {
		t.Errorf("chunk 1 does not repeat chunk 0's tail %q:\n  [0]=%q\n  [1]=%q",
			tail, got.Chunks[0], got.Chunks[1])
	}
}

// TestNoChunkIsASuffixOfItsPredecessor pins the boundary floor. Without it,
// the overlap rewind lets the backwards scan re-pick the separator the
// previous chunk already cut at, emitting a chunk that is a pure suffix of
// its predecessor: zero new coverage, one burned MaxChunks slot per section
// boundary. On a real 46k-rune document that wasted half the 64-slot budget,
// fired Truncated, and left the tail unsearchable — so this also asserts the
// document fits without truncation and its final sentence is covered.
func TestNoChunkIsASuffixOfItsPredecessor(t *testing.T) {
	// The reproducing shape: a heading followed by sentence-only prose, so the
	// window after a paragraph-break cut contains no later "\n\n".
	sentence := "The deploy pipeline runs its tests across sixteen cores in parallel. "
	var b strings.Builder
	for section := 0; b.Len() < 46000; section++ {
		fmt.Fprintf(&b, "Heading %d\n\n", section)
		for b.Len() == 0 || !strings.HasSuffix(b.String(), "\n\n") {
			b.WriteString(sentence)
			if strings.Count(b.String(), sentence)%16 == 0 {
				b.WriteString("\n\n")
			}
		}
	}
	sentinel := "The final sentence carries the distinctive tail marker."
	text := b.String() + sentinel

	got := Split(text, DefaultConfig())
	if got.Truncated {
		t.Fatalf("a %d-rune document truncated at %d chunks; the budget covers ~64k runes",
			len([]rune(text)), len(got.Chunks))
	}
	for i := 1; i < len(got.Chunks); i++ {
		if strings.Contains(got.Chunks[i-1], got.Chunks[i]) {
			t.Fatalf("chunk %d is contained in chunk %d — no new coverage:\n [%d]=%q\n [%d]=%q",
				i, i-1, i-1, got.Chunks[i-1], i, got.Chunks[i])
		}
	}
	last := got.Chunks[len(got.Chunks)-1]
	if !strings.Contains(last, sentinel) {
		t.Fatalf("the document tail is not covered; last chunk=%q", lastRunes(last, 120))
	}
}

func TestPrefersSemanticBoundaries(t *testing.T) {
	cfg := Config{Size: 30, Overlap: 0, MinContent: 10, MaxChunks: 64}
	got := Split("alpha beta.\n\ngamma delta epsilon zeta eta theta", cfg)
	if len(got.Chunks) < 2 {
		t.Fatalf("want >= 2 chunks, got %d: %q", len(got.Chunks), got.Chunks)
	}
	// The paragraph break is the strongest separator in range, so the first
	// chunk should end there rather than at exactly 30 runes.
	if got.Chunks[0] != "alpha beta." {
		t.Errorf("chunk 0 = %q, want it to end at the paragraph break", got.Chunks[0])
	}
}

func TestHardCutWhenNoSeparatorIsReachable(t *testing.T) {
	cfg := Config{Size: 10, Overlap: 0, MinContent: 5, MaxChunks: 64}
	got := Split(strings.Repeat("a", 35), cfg)
	if len(got.Chunks) != 4 {
		t.Fatalf("want 4 chunks of an unbreakable 35-rune run, got %d: %q", len(got.Chunks), got.Chunks)
	}
	for _, c := range got.Chunks[:3] {
		if utf8.RuneCountInString(c) != 10 {
			t.Errorf("hard cut produced a %d-rune chunk, want 10: %q", utf8.RuneCountInString(c), c)
		}
	}
}

// TestNeverSplitsAnAstralRune guards the same class of bug the client-side
// capture had: cutting by index rather than by rune.
func TestNeverSplitsAnAstralRune(t *testing.T) {
	cfg := Config{Size: 7, Overlap: 2, MinContent: 3, MaxChunks: 64}
	for _, c := range Split(strings.Repeat("👍", 40), cfg).Chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk is not valid UTF-8: %q", c)
		}
		for _, r := range c {
			if r == utf8.RuneError {
				t.Fatalf("chunk contains a replacement rune (split pair): %q", c)
			}
		}
	}
}

func TestMaxChunksReportsTruncationRatherThanHidingIt(t *testing.T) {
	cfg := Config{Size: 10, Overlap: 0, MinContent: 5, MaxChunks: 3}
	got := Split(strings.Repeat("a", 500), cfg)
	if len(got.Chunks) != 3 {
		t.Fatalf("got %d chunks, want the MaxChunks cap of 3", len(got.Chunks))
	}
	// The old ceiling was silent; this one has to be visible, since the tail is
	// genuinely unreachable by chunk recall.
	if !got.Truncated {
		t.Error("Truncated = false after the cap stopped the split — the caller cannot warn")
	}
}

func TestSplitTerminates(t *testing.T) {
	// An overlap >= size would rewind at least as far as each step advances.
	// Halving it is arbitrary; not terminating is not.
	for _, cfg := range []Config{
		{Size: 10, Overlap: 10, MinContent: 1, MaxChunks: 1000},
		{Size: 10, Overlap: 50, MinContent: 1, MaxChunks: 1000},
		{Size: 1, Overlap: 1, MinContent: 1, MaxChunks: 1000},
		{Size: 10, Overlap: -5, MinContent: 1, MaxChunks: 1000},
	} {
		done := make(chan Result, 1)
		go func() { done <- Split(strings.Repeat("ab cd ", 50), cfg) }()
		select {
		case got := <-done:
			for _, c := range got.Chunks {
				if utf8.RuneCountInString(c) > cfg.Size {
					t.Errorf("cfg %+v: chunk exceeds Size: %q", cfg, c)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("cfg %+v: Split did not terminate", cfg)
		}
	}
}

func TestZeroSizeIsANoop(t *testing.T) {
	if got := Split("some text here", Config{Size: 0, MinContent: 1}); len(got.Chunks) != 0 {
		t.Fatalf("Size=0 produced %d chunks, want 0", len(got.Chunks))
	}
}

func TestDefaultConfigFitsTheEmbedBudget(t *testing.T) {
	c := DefaultConfig()
	// A chunk that exceeded the per-item embed budget would be truncated by
	// internal/embed.Batched, which is the exact failure chunking removes.
	const embedMaxItemCharsDefault = 8000
	if c.Size >= embedMaxItemCharsDefault {
		t.Errorf("Size %d >= the default EmbedMaxItemChars %d: a chunk could be truncated",
			c.Size, embedMaxItemCharsDefault)
	}
	if c.Overlap >= c.Size {
		t.Errorf("Overlap %d >= Size %d", c.Overlap, c.Size)
	}
	if c.MinContent < c.Size {
		t.Errorf("MinContent %d < Size %d: content between them would make one chunk, "+
			"which just duplicates the document vector", c.MinContent, c.Size)
	}
}

func TestIsWhitespaceOnly(t *testing.T) {
	for _, tc := range []struct {
		s    string
		want bool
	}{{"", true}, {"   ", true}, {"\n\t ", true}, {"a", false}, {" a ", false}, {"这", false}} {
		if got := IsWhitespaceOnly(tc.s); got != tc.want {
			t.Errorf("IsWhitespaceOnly(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\t' {
			return -1
		}
		return r
	}, s)
}

func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// isSubsequence reports whether every rune of want appears in got in order.
func isSubsequence(want, got string) bool {
	w := []rune(want)
	i := 0
	for _, r := range got {
		if i < len(w) && r == w[i] {
			i++
		}
	}
	return i == len(w)
}
