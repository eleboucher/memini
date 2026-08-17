package embed

import (
	"context"
	"errors"
)

// ErrDisabled is returned when memory operations need embeddings but no
// embeddings endpoint is configured.
var ErrDisabled = errors.New("embeddings endpoint not configured (set MEMINI_EMBED_BASE_URL)")

// Disabled is an Embedder used when no endpoint is configured: the server
// boots, and every Embed call returns ErrDisabled. Under the default non-zero
// embed timeouts the service swallows that error and degrades — remember
// stores a vectorless pending_embed row and recall falls back to keyword-only
// search. The error is only surfaced to callers when the timeouts are
// explicitly set to 0 (fail-fast mode) or on paths that never degrade
// (reembed, migrate).
type Disabled struct{ D int }

// Dims returns the configured dimensionality.
func (d Disabled) Dims() int { return d.D }

// Embed always fails.
func (d Disabled) Embed(context.Context, []string) ([][]float32, error) { return nil, ErrDisabled }
