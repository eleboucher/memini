package embed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	lru "github.com/hashicorp/golang-lru/v2"
)

// Cached wraps an Embedder with a content-hash LRU cache so identical text is
// embedded only once. Safe for concurrent use.
type Cached struct {
	inner Embedder
	cache *lru.Cache[string, []float32]
}

// NewCached wraps e with an LRU of the given size (number of distinct texts).
func NewCached(e Embedder, size int) (*Cached, error) {
	c, err := lru.New[string, []float32](size)
	if err != nil {
		return nil, err
	}
	return &Cached{inner: e, cache: c}, nil
}

// Dims returns the wrapped embedder's dimensionality.
func (c *Cached) Dims() int { return c.inner.Dims() }

// Embed returns vectors for texts, serving cache hits and embedding only misses.
func (c *Cached) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	var missIdx []int
	var missText []string

	for i, t := range texts {
		if v, ok := c.cache.Get(key(t)); ok {
			out[i] = v
			continue
		}
		missIdx = append(missIdx, i)
		missText = append(missText, t)
	}

	if len(missText) > 0 {
		vecs, err := c.inner.Embed(ctx, missText)
		if err != nil {
			return nil, err
		}
		for j, v := range vecs {
			i := missIdx[j]
			out[i] = v
			c.cache.Add(key(missText[j]), v)
		}
	}
	return out, nil
}

func key(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
