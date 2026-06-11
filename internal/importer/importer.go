// Package importer bulk-loads memories exported from other memory systems
// (agentmemory, mem0, mnemory) or memini's own format. The local backend embeds
// content and writes to the store, preserving source IDs and timestamps; the
// remote backend POSTs to a running memini's REST API.
package importer

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// Record is the portable, source-agnostic shape every adapter produces.
type Record struct {
	ID         string
	Namespace  string
	Tier       memory.Tier
	Content    string
	Summary    string
	Tags       []string
	Metadata   map[string]any
	Importance float64
	CreatedAt  time.Time  // zero -> import time
	UpdatedAt  time.Time  // zero -> CreatedAt
	ExpiresAt  *time.Time // nil -> tier default measured from CreatedAt
}

// Options tune an import run.
type Options struct {
	// DefaultNamespace scopes records whose source carried no namespace.
	DefaultNamespace string
	// BatchSize bounds how many records are written per batch.
	BatchSize int
	// OnProgress is called after each batch with (processed, total).
	// It may be nil.
	OnProgress func(done, total int)
}

// Report summarizes an import run.
type Report struct {
	Total    int      `json:"total"`
	Imported int      `json:"imported"`
	Skipped  int      `json:"skipped"` // dropped before write (empty content)
	Errors   []string `json:"errors,omitempty"`
}

const defaultBatchSize = 64

// batchWriter persists a batch of records. A non-nil error aborts the run (a
// failure that would recur for every record, e.g. embedding or auth); per-record
// failures are returned in errs instead.
type batchWriter func(ctx context.Context, recs []Record, opts Options) (imported int, errs []string, err error)

// Importer bulk-loads records via a configured backend.
type Importer struct {
	write     batchWriter
	batchSize int
}

// NewLocal builds an Importer that embeds and writes directly to the store.
func NewLocal(st store.Store, e embed.Embedder) *Importer {
	lw := &localWriter{store: st, embedder: e, now: time.Now, newID: uuid.NewString}
	return &Importer{write: lw.write, batchSize: defaultBatchSize}
}

// NewRemote builds an Importer that POSTs records to a remote memini server.
func NewRemote(c *RemoteClient) *Importer {
	return &Importer{write: c.write, batchSize: 1}
}

// Import writes records, skipping empty-content ones and continuing past
// per-record failures (collected in the Report).
func (im *Importer) Import(ctx context.Context, recs []Record, opts Options) (Report, error) {
	rep := Report{Total: len(recs)}
	batch := opts.BatchSize
	if batch <= 0 {
		batch = im.batchSize
	}
	if batch <= 0 {
		batch = defaultBatchSize
	}

	clean := make([]Record, 0, len(recs))
	for _, r := range recs {
		if r.Content == "" {
			rep.Skipped++
			continue
		}
		clean = append(clean, r)
	}

	if opts.OnProgress != nil {
		opts.OnProgress(rep.Skipped, rep.Total)
	}

	for start := 0; start < len(clean); start += batch {
		end := min(start+batch, len(clean))
		imported, errs, err := im.write(ctx, clean[start:end], opts)
		rep.Imported += imported
		rep.Errors = append(rep.Errors, errs...)
		if opts.OnProgress != nil {
			opts.OnProgress(min(end, len(clean))+rep.Skipped, rep.Total)
		}
		if err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// localWriter embeds a batch and upserts each record into the store.
type localWriter struct {
	store    store.Store
	embedder embed.Embedder
	now      func() time.Time
	newID    func() string
}

func (lw *localWriter) write(ctx context.Context, recs []Record, opts Options) (int, []string, error) {
	texts := make([]string, len(recs))
	for i, r := range recs {
		texts[i] = r.Content
	}
	vecs, err := lw.embedder.Embed(ctx, texts)
	if err != nil {
		return 0, nil, fmt.Errorf("import: embed: %w", err)
	}
	var imported int
	var errs []string
	for i, r := range recs {
		m := lw.toMemory(r, opts)
		m.Embedding = vecs[i]
		if err := lw.store.Upsert(ctx, m); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", m.ID, err))
			continue
		}
		imported++
	}
	return imported, errs, nil
}

// toMemory maps a Record to a stored memory, applying defaults.
func (lw *localWriter) toMemory(r Record, opts Options) *memory.Memory {
	now := lw.now().UTC()
	created := r.CreatedAt
	if created.IsZero() {
		created = now
	}
	updated := r.UpdatedAt
	if updated.IsZero() {
		updated = created
	}
	tier := r.Tier
	if !tier.Valid() {
		tier = memory.TierSemantic
	}
	id := r.ID
	if id == "" {
		id = lw.newID()
	}
	m := &memory.Memory{
		ID:             id,
		Namespace:      resolveNamespace(r.Namespace, opts.DefaultNamespace),
		Tier:           tier,
		Content:        r.Content,
		Summary:        r.Summary,
		Tags:           r.Tags,
		Metadata:       r.Metadata,
		Importance:     min(max(r.Importance, 0), 1),
		CreatedAt:      created,
		UpdatedAt:      updated,
		LastAccessedAt: updated,
	}
	if r.ExpiresAt != nil {
		m.ExpiresAt = r.ExpiresAt
	} else if ttl := tier.DefaultTTL(); ttl > 0 {
		exp := created.Add(ttl)
		m.ExpiresAt = &exp
	}
	return m
}

// resolveNamespace picks the record's namespace, the run default, or "default".
func resolveNamespace(ns, fallback string) string {
	if ns != "" {
		return ns
	}
	if fallback != "" {
		return fallback
	}
	return "default"
}
