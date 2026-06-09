package embed

import (
	"context"
	"errors"
)

// ErrDisabled is returned when memory operations need embeddings but no
// embeddings endpoint is configured.
var ErrDisabled = errors.New("embeddings endpoint not configured (set MEMINI_EMBED_BASE_URL)")

// Disabled is an Embedder used when no endpoint is configured: the server boots
// but remember/recall fail with ErrDisabled.
type Disabled struct{ D int }

// Dims returns the configured dimensionality.
func (d Disabled) Dims() int { return d.D }

// Embed always fails.
func (d Disabled) Embed(context.Context, []string) ([][]float32, error) { return nil, ErrDisabled }
