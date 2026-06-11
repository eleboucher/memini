package embed

import (
	"context"
	"log/slog"
)

// Batched splits an Embed call into sub-requests bounded by item count and
// character budget to keep payloads under endpoint limits, truncating over-long texts.
type Batched struct {
	inner        Embedder
	maxItems     int
	maxChars     int
	maxItemChars int
}

// NewBatched wraps inner. maxItems caps items per request; maxChars caps total
// characters per request; maxItemChars truncates any single text (0 disables).
func NewBatched(inner Embedder, maxItems, maxChars, maxItemChars int) *Batched {
	if maxItems <= 0 {
		maxItems = 16
	}
	return &Batched{inner: inner, maxItems: maxItems, maxChars: maxChars, maxItemChars: maxItemChars}
}

// Dims returns the wrapped embedder's dimensionality.
func (b *Batched) Dims() int { return b.inner.Dims() }

// Embed splits texts into bounded sub-batches, preserving order.
func (b *Batched) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := 0; i < len(texts); {
		j, chars := i, 0
		var sub []string
		for j < len(texts) && len(sub) < b.maxItems {
			t := truncateRunes(texts[j], b.maxItemChars)
			if len(t) < len(texts[j]) {
				// The stored vector represents only this prefix; recall won't
				// match content beyond it.
				slog.DebugContext(ctx, "embed: truncating over-long text",
					"chars", len(texts[j]), "max", b.maxItemChars)
			}
			if len(sub) > 0 && b.maxChars > 0 && chars+len(t) > b.maxChars {
				break
			}
			sub = append(sub, t)
			chars += len(t)
			j++
		}
		vecs, err := b.inner.Embed(ctx, sub)
		if err != nil {
			return nil, err
		}
		copy(out[i:j], vecs)
		i = j
	}
	return out, nil
}

// truncateRunes caps s to n runes (n <= 0 disables truncation).
func truncateRunes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
