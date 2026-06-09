package embedtest

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
)

// Fake is a deterministic bag-of-words embedder: texts that share tokens land
// near each other in vector space, which is enough to test ranking.
type Fake struct{ dims int }

// New returns a Fake embedder of the given dimensionality.
func New(dims int) *Fake { return &Fake{dims: dims} }

// Dims returns the embedding dimensionality.
func (f *Fake) Dims() int { return f.dims }

// Embed produces one normalized vector per text.
func (f *Fake) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = f.vector(t)
	}
	return out, nil
}

func (f *Fake) vector(text string) []float32 {
	v := make([]float32, f.dims)
	for tok := range strings.FieldsSeq(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(tok))
		v[h.Sum32()%uint32(f.dims)] += 1
	}
	// L2-normalize so distances reflect cosine-like similarity.
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		v[0] = 1
		return v
	}
	inv := float32(1 / math.Sqrt(norm))
	for j := range v {
		v[j] *= inv
	}
	return v
}
