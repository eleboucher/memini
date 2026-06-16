package maintenance

import (
	"context"
	"fmt"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/store"
)

// ReembedReport summarizes a re-embedding pass.
type ReembedReport struct {
	Namespaces int `json:"namespaces"`
	Total      int `json:"total"`
	Reembedded int `json:"reembedded"`
}

// Reembed recomputes and rewrites every memory's vector with emb, migrating a
// store to a new embedding model in place. It embeds memory content (the text
// the write path embeds), including expired and superseded rows so the vector
// index stays consistent. Dims can't change here (the store rejects mismatched
// widths). namespaces restricts the pass when non-empty. Callers that changed
// the model should record it with store.SetEmbedModel afterwards.
func Reembed(
	ctx context.Context, st store.Store, emb embed.Embedder,
	namespaces []string, batchSize int, onProgress func(done, total int),
) (ReembedReport, error) {
	if batchSize <= 0 {
		batchSize = 128
	}
	var rep ReembedReport

	if len(namespaces) == 0 {
		all, err := st.ListNamespaces(ctx)
		if err != nil {
			return rep, err
		}
		namespaces = all
	}

	// Include expired/superseded rows: their vectors are in the index too, so
	// skipping them would leave the store half-migrated.
	f := store.Filter{IncludeExpired: true, IncludeSuperseded: true}

	for _, ns := range namespaces {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		mems, err := st.List(ctx, ns, f, 0)
		if err != nil {
			return rep, fmt.Errorf("reembed: list %q: %w", ns, err)
		}
		if len(mems) == 0 {
			continue
		}
		rep.Namespaces++
		rep.Total += len(mems)

		for start := 0; start < len(mems); start += batchSize {
			end := min(start+batchSize, len(mems))
			batch := mems[start:end]

			texts := make([]string, len(batch))
			for i, m := range batch {
				texts[i] = m.Content
			}
			vecs, err := emb.Embed(ctx, texts)
			if err != nil {
				return rep, fmt.Errorf("reembed: embed %q: %w", ns, err)
			}
			for i, m := range batch {
				m.Embedding = vecs[i]
				if err := st.Upsert(ctx, m); err != nil {
					return rep, fmt.Errorf("reembed: upsert %q/%s: %w", ns, m.ID, err)
				}
				rep.Reembedded++
				if onProgress != nil {
					onProgress(rep.Reembedded, rep.Total)
				}
			}
		}
	}
	return rep, nil
}
