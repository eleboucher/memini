// Package importer bulk-loads memories exported from other memory systems
// (agentmemory, mem0, mnemory) or memini's own format. The local backend embeds
// content and writes to the store, preserving source IDs and timestamps; the
// remote backend POSTs to a running memini's REST API.
package importer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	// ForceNamespace, when set, overrides every record's namespace (the explicit
	// "merge everything into one pool" opt-in). The original namespace is
	// preserved in metadata["import_source_namespace"] so the merge is reversible
	// with `memini namespace split`.
	ForceNamespace string
	// Source names the export format, used to stamp a provenance tag
	// (import:<source>:<date>) and to seed deterministic record IDs.
	Source Source
	// DefaultImportance is applied to records whose source carried no importance
	// (reported as 0), so bulk imports rank below curated, source-scored
	// memories. 0 disables the floor.
	DefaultImportance float64
	// Confidence overrides the seed corroboration for durable imported facts
	// (e.g. a trusted re-import). nil uses the low default import seed.
	Confidence *float64
	// SkipExisting checks the store for a record's ID before writing and counts
	// it as a duplicate instead of clobbering an existing memory's access
	// counters. Combined with deterministic IDs this makes re-imports idempotent.
	// Only honored by the local backend.
	SkipExisting bool
	// DryRun resolves namespaces and runs the quality gates but writes nothing;
	// the Report's namespace histogram still reflects where records would land.
	DryRun bool
	// BatchSize bounds how many records are written per batch.
	BatchSize int
	// OnProgress is called after each batch with (processed, total).
	// It may be nil.
	OnProgress func(done, total int)
	// MinContentLen drops records whose trimmed content is shorter than this,
	// filtering out stubs from a low-quality bulk import. 0 disables the gate.
	MinContentLen int
	// MinImportance drops records below this importance. 0 disables the gate;
	// note sources that carry no importance report 0, so any positive value
	// skips them.
	MinImportance float64
}

// Report summarizes an import run.
type Report struct {
	Total      int            `json:"total"`
	Imported   int            `json:"imported"`
	Skipped    int            `json:"skipped"`              // dropped before write (failed a quality gate)
	Duplicates int            `json:"duplicates,omitempty"` // already present, left untouched (SkipExisting)
	Namespaces map[string]int `json:"namespaces,omitempty"` // records resolved per destination namespace
	Errors     []string       `json:"errors,omitempty"`
}

// importIDNamespace seeds deterministic, content-addressed IDs for imported
// records that carry none, so re-importing the same export is idempotent
// instead of inserting duplicates with fresh random IDs.
var importIDNamespace = uuid.MustParse("6f9e4d2a-7c3b-4e15-9a8d-2b1c0f6e5a44")

// deterministicID derives a stable UUIDv5 from the source, destination
// namespace and content, so the same record imported twice yields the same ID.
func deterministicID(src Source, ns, content string) string {
	return uuid.NewSHA1(importIDNamespace, []byte(string(src)+"\x00"+ns+"\x00"+content)).String()
}

const defaultBatchSize = 64

// writeResult is one batch's outcome. A non-nil error from batchWriter aborts
// the run (a failure that would recur for every record, e.g. embedding or auth);
// per-record failures are collected in errs instead.
type writeResult struct {
	imported   int
	duplicates int
	errs       []string
}

// batchWriter persists a batch of records.
type batchWriter func(ctx context.Context, recs []Record, opts Options) (writeResult, error)

// Importer bulk-loads records via a configured backend.
type Importer struct {
	write     batchWriter
	batchSize int
	now       func() time.Time
}

// NewLocal builds an Importer that embeds and writes directly to the store.
func NewLocal(st store.Store, e embed.Embedder) *Importer {
	lw := &localWriter{store: st, embedder: e, now: time.Now}
	return &Importer{write: lw.write, batchSize: defaultBatchSize, now: time.Now}
}

// NewRemote builds an Importer that POSTs records to a remote memini server.
func NewRemote(c *RemoteClient) *Importer {
	return &Importer{write: c.write, batchSize: 32, now: time.Now}
}

// Import writes records, skipping those that fail the quality gates (empty or
// below MinContentLen/MinImportance) and continuing past per-record failures
// (collected in the Report).
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
		if len(strings.TrimSpace(r.Content)) < max(1, opts.MinContentLen) {
			rep.Skipped++
			continue
		}
		if r.Importance < opts.MinImportance {
			rep.Skipped++
			continue
		}
		clean = append(clean, r)
	}

	// Resolve final namespaces, stamp provenance, apply the importance floor and
	// deterministic IDs once — before any backend write, and so the namespace
	// histogram reflects exactly where records land (or would, under DryRun).
	rep.Namespaces = finalizeRecords(clean, opts, im.now().UTC())

	if opts.DryRun {
		if opts.OnProgress != nil {
			opts.OnProgress(rep.Total, rep.Total)
		}
		return rep, nil
	}

	if opts.OnProgress != nil {
		opts.OnProgress(rep.Skipped, rep.Total)
	}

	for start := 0; start < len(clean); start += batch {
		end := min(start+batch, len(clean))
		res, err := im.write(ctx, clean[start:end], opts)
		rep.Imported += res.imported
		rep.Duplicates += res.duplicates
		rep.Errors = append(rep.Errors, res.errs...)
		if opts.OnProgress != nil {
			opts.OnProgress(min(end, len(clean))+rep.Skipped, rep.Total)
		}
		if err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// finalizeRecords resolves each record's destination namespace, stamps import
// provenance (a dated tag plus source metadata), preserves any namespace a merge
// would erase, applies the importance floor to unscored records and assigns a
// deterministic ID to records that carry none. It mutates recs in place and
// returns the per-namespace histogram.
func finalizeRecords(recs []Record, opts Options, now time.Time) map[string]int {
	hist := make(map[string]int, 4)
	today := now.Format("2006-01-02")
	for i := range recs {
		r := &recs[i]
		orig := r.Namespace
		ns := resolveNamespace(r.Namespace, opts.DefaultNamespace)
		if opts.ForceNamespace != "" {
			ns = opts.ForceNamespace
		}
		r.Namespace = ns
		hist[ns]++

		if opts.Source != "" {
			r.Tags = appendUnique(r.Tags, fmt.Sprintf("import:%s:%s", opts.Source, today))
			r.Metadata = withMeta(r.Metadata, "import_source", string(opts.Source))
		}
		// A merge that discards a real source namespace records it so the
		// move/split recovery tool can reconstruct the original scoping.
		if ns != orig && orig != "" {
			r.Metadata = withMeta(r.Metadata, "import_source_namespace", orig)
		}
		if r.Importance == 0 && opts.DefaultImportance > 0 {
			r.Importance = opts.DefaultImportance
		}
		if r.ID == "" {
			r.ID = deterministicID(opts.Source, ns, r.Content)
		}
	}
	return hist
}

// appendUnique appends tag to tags unless it is already present.
func appendUnique(tags []string, tag string) []string {
	if slices.Contains(tags, tag) {
		return tags
	}
	return append(tags, tag)
}

// localWriter embeds a batch and upserts each record into the store.
type localWriter struct {
	store    store.Store
	embedder embed.Embedder
	now      func() time.Time
}

func (lw *localWriter) write(ctx context.Context, recs []Record, opts Options) (writeResult, error) {
	var res writeResult
	// Existence check before the (expensive) embed call: a re-import of an
	// already-stored record costs one Get, not an embed plus upsert that would
	// reset its access counters.
	pending := recs
	if opts.SkipExisting {
		pending = make([]Record, 0, len(recs))
		for _, r := range recs {
			_, err := lw.store.Get(ctx, r.Namespace, r.ID)
			switch {
			case err == nil:
				res.duplicates++
			case errors.Is(err, store.ErrNotFound):
				pending = append(pending, r) // genuinely new
			default:
				// A transient store error must not fall through to a blind
				// upsert that would clobber the record we couldn't verify.
				res.errs = append(res.errs, fmt.Sprintf("%s: existence check: %v", r.ID, err))
			}
		}
	}
	if len(pending) == 0 {
		return res, nil
	}

	texts := make([]string, len(pending))
	for i, r := range pending {
		texts[i] = r.Content
	}
	vecs, err := lw.embedder.Embed(ctx, texts)
	if err != nil {
		return res, fmt.Errorf("import: embed: %w", err)
	}
	for i, r := range pending {
		m := lw.toMemory(r, opts)
		m.Embedding = vecs[i]
		if err := lw.store.Upsert(ctx, m); err != nil {
			res.errs = append(res.errs, fmt.Sprintf("%s: %v", m.ID, err))
			continue
		}
		res.imported++
	}
	return res, nil
}

// toMemory maps a finalized Record (namespace, ID, importance already resolved
// by finalizeRecords) to a stored memory, applying tier and timestamp defaults.
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
		// Untyped imports default to episodic (90d TTL) rather than durable
		// semantic: bulk imports of unknown quality should age out unless recall
		// reinforces them, not live forever.
		tier = memory.TierEpisodic
	}
	m := &memory.Memory{
		ID:             r.ID,
		Namespace:      r.Namespace,
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
	// Durable imports start uncorroborated: a bulk-imported "fact" must earn
	// trust through recall/re-observation before it outranks facts the agent
	// established itself. A caller-supplied value (a trusted import) overrides.
	if tier.Term() == memory.LongTerm {
		c := memory.ConfidenceSeedImported
		if opts.Confidence != nil {
			c = *opts.Confidence
		}
		m.Confidence = &c
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
