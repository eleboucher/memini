// Package embed turns text into dense vectors via an external,
// OpenAI-compatible embeddings endpoint; memini never embeds locally.
package embed

import "context"

// Embedder converts text into fixed-dimension vectors.
type Embedder interface {
	// Embed returns one vector per input text, in the same order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Dims is the dimensionality of returned vectors.
	Dims() int
}

// EmbedOne embeds a single string.
func EmbedOne(ctx context.Context, e Embedder, text string) ([]float32, error) {
	vecs, err := e.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}
