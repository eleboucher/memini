// Package chunk splits long memory content into overlapping segments for
// embedding, so a memory stays searchable past the single-vector budget.
//
// A memory's vector covers only the first EmbedMaxItemChars runes of its
// content (see internal/embed.Batched): the text beyond that is stored and
// returned whole but cannot be matched by vector recall. Splitting the content
// and embedding each piece removes that ceiling instead of moving it.
//
// The rules this package encodes:
//
//   - Short content produces NO chunks. A single chunk of a short memory would
//     be a duplicate of its own document vector — a wasted embedder call, a
//     wasted row, and a duplicate hit to merge away. Chunk rows therefore exist
//     only for the memories that are actually being truncated today.
//   - Cuts prefer a semantic boundary (paragraph, then line, then sentence,
//     then word) over an exact size. memini's corpus is conversational captures
//     and distilled facts, where a fact routinely fits in one sentence, so
//     cutting mid-sentence destroys the very unit being retrieved.
//   - Chunks overlap, so a fact straddling a boundary survives whole in one of
//     them.
//   - Everything is counted in runes, matching every other length bound in the
//     codebase.
//
// Pure and deterministic: no I/O, no clock, no config import.
package chunk

import (
	"strings"
	"unicode"
)

// Config bounds a split. The zero value is not usable; see DefaultConfig.
type Config struct {
	// Size is the maximum runes in one chunk.
	Size int
	// Overlap is how many runes of the previous chunk each chunk repeats. It
	// must be < Size; Split halves it if it is not, rather than failing to make
	// progress.
	Overlap int
	// MinContent is the content length at or below which Split returns nothing.
	// Content this short is already covered whole by its document vector.
	MinContent int
	// MaxChunks caps the chunks produced from one memory. Past it Split stops
	// and reports the truncation through Result.Truncated rather than silently
	// covering a prefix — an honest, observable ceiling replacing the silent one.
	MaxChunks int
}

// DefaultConfig is the built-in split. Sized so a chunk fits any plausible
// embedder: 1200 runes is roughly 300 tokens, well under the 512-token window
// of the small local models (BGE, e5) that MEMINI_EMBED_BASE_URL is often
// pointed at, and far under text-embedding-3-small's 8191. It is also well
// under the default EmbedMaxItemChars (8000), so a chunk can never itself be
// truncated — the bug this package exists to fix cannot recur inside the fix.
//
// The 200-rune overlap is one to two sentences: enough that a fact split across
// a boundary appears whole in the following chunk.
func DefaultConfig() Config {
	return Config{Size: 1200, Overlap: 200, MinContent: 1200, MaxChunks: 64}
}

// Result is a split's output.
type Result struct {
	// Chunks are the segments to embed, in order. Empty when the content is at
	// or under MinContent.
	Chunks []string
	// Truncated reports that MaxChunks stopped the split before the end of the
	// content, so the tail is unchunked and stays unsearchable by chunk recall.
	Truncated bool
}

// separators are tried in descending order of how much meaning a cut there
// preserves. The last resort is any space; failing that Split cuts exactly at
// the size bound.
var separators = []string{"\n\n", "\n", ". ", "? ", "! ", "; ", " "}

// Split segments text under cfg. It returns no chunks for content at or under
// cfg.MinContent, and never returns a chunk longer than cfg.Size runes.
func Split(text string, cfg Config) Result {
	if cfg.Size <= 0 {
		return Result{}
	}
	runes := []rune(text)
	if cfg.MinContent > 0 && len(runes) <= cfg.MinContent {
		return Result{}
	}
	if len(runes) == 0 {
		return Result{}
	}
	overlap := max(cfg.Overlap, 0)
	if overlap >= cfg.Size {
		// An overlap at or past the size would rewind at least as far as each
		// chunk advances, so the split would never reach the end. Halve it
		// rather than spin.
		overlap = cfg.Size / 2
	}

	var out []string
	for start := 0; start < len(runes); {
		if cfg.MaxChunks > 0 && len(out) >= cfg.MaxChunks {
			return Result{Chunks: out, Truncated: true}
		}
		end := start + cfg.Size
		if end >= len(runes) {
			if s := strings.TrimSpace(string(runes[start:])); s != "" {
				out = append(out, s)
			}
			break
		}
		cut := boundary(runes, start, end)
		if s := strings.TrimSpace(string(runes[start:cut])); s != "" {
			out = append(out, s)
		}
		next := cut - overlap
		if next <= start {
			// The overlap would rewind to or behind this chunk's own start.
			// Advancing to the cut loses the overlap for this one boundary,
			// which is strictly better than not terminating.
			next = cut
		}
		start = next
	}
	return Result{Chunks: out}
}

// boundary picks where to cut a chunk that would otherwise end at `end`,
// falling back to end itself (a hard cut) when the window holds no separator.
// The returned index is always > start, so callers always make progress.
//
// Each separator is searched over the whole window, backwards. Backwards is
// what makes this pack rather than fragment: the LAST paragraph break before
// the size limit is the one that uses the most of the budget, so prose with
// regular breaks yields full-size chunks that still end on a boundary. The
// only way to get a short chunk is for the window to hold exactly one early
// boundary and no later one of any strength — and cutting there is right
// anyway, because it is the only structure the text offers.
func boundary(runes []rune, start, end int) int {
	for _, sep := range separators {
		sepRunes := []rune(sep)
		for i := end - len(sepRunes); i > start; i-- {
			if hasAt(runes, i, sepRunes) {
				// Cut after the separator so it stays with the text it ends.
				return i + len(sepRunes)
			}
		}
	}
	return end
}

// hasAt reports whether runes[i:] begins with sep.
func hasAt(runes []rune, i int, sep []rune) bool {
	if i < 0 || i+len(sep) > len(runes) {
		return false
	}
	for j, r := range sep {
		if runes[i+j] != r {
			return false
		}
	}
	return true
}

// IsWhitespaceOnly reports whether s has no non-space rune. Callers skip
// embedding such a chunk: it carries no signal and would cost a vector.
func IsWhitespaceOnly(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
