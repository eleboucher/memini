package bench

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

const benchNamespace = "bench"

// ingestWindow bounds items per Embed call; embed.Batched further splits each
// call into endpoint-safe HTTP sub-batches.
const ingestWindow = 200

func nsOf(group string) string {
	if group == "" {
		return benchNamespace
	}
	return group
}

// NamespaceOf exposes nsOf to the external bench_test package (the synthesis
// spike needs the store namespace a question group maps to).
func NamespaceOf(group string) string { return nsOf(group) }

// docPrefix is prepended to document text before embedding (not stored), for
// asymmetric embedders that need a document-side instruction (e.g. nomic's
// "search_document: "). Empty by default; query-instruction-only models like
// qwen3-embedding leave it unset and use MEMINI_EMBED_QUERY_PREFIX instead.
func docPrefix() string { return os.Getenv("MEMINI_EMBED_DOC_PREFIX") }

// IngestMode selects how the corpus enters the store.
type IngestMode string

const (
	// IngestUpsert writes items directly via store.Upsert (the historical
	// default): pure retrieval measurement, write-path features inert.
	IngestUpsert IngestMode = "upsert"
	// IngestWrite routes items through service.Remember, exercising the shipped
	// write path: tier classification, gates, fingerprint/write dedup,
	// corroboration, and contradiction invalidation all participate.
	IngestWrite IngestMode = "write"
)

// RecallHit is one retrieved memory. IDs usually holds a single dataset item
// ID; write-mode ingest can merge several items into one stored memory
// (fingerprint/write dedup), in which case the hit carries every item ID that
// landed on it. Rows the write path derived itself (extract-on-write) map to
// no item and keep their memory ID, which never matches gold — write-mode
// recall is conservative by construction.
type RecallHit struct {
	IDs     []string
	Content string
}

// System is a memory system under test.
type System interface {
	Name() string
	Ingest(ctx context.Context, items []Item) error
	Recall(ctx context.Context, group, query string, k int) ([]RecallHit, error)
}

// SystemOpts configures MeminiSystemsOpts. The zero value reproduces
// MeminiSystems' historical defaults (fixed benchClock, undated ingest).
type SystemOpts struct {
	Concurrency int
	QueryPrefix string
	FusionAlpha float64 // < 0 uses RRF; >= 0 uses convex score fusion
	PoolFactor  int
	PoolFloor   int
	Mode        IngestMode
	Distiller   llm.Distiller
	// Dated honors Item.Time instead of the fixed benchClock: upsert rows are
	// stamped and dated at Item.Time; write-mode ingest advances a per-item clock
	// (with ValidFrom, never-TTL, and session_id metadata), so contradiction and
	// temporal recall see the real chronology. RecallNow is the clock recall runs
	// under once ingest completes (zero = benchClock). Ignored when Dated is false.
	Dated     bool
	RecallNow time.Time
	// QueryRewrite enables LLM query expansion on the hybrid system's Recall
	// path — the A/B lever for measuring read-path LLM value. Needs Answerer.
	QueryRewrite bool
	// Answerer, when non-nil, enables LLM-backed recall features (query
	// expansion). Must implement llm.Completer.
	Answerer llm.Completer
}

// meminiBackend holds the store/embedder shared across retrieval strategies;
// ingestion runs once and is reused.
type meminiBackend struct {
	store       store.Store
	embedder    embed.Embedder
	svc         *service.Service
	queryPrefix string
	concurrency int
	mode        IngestMode
	distiller   llm.Distiller
	// dated honors Item.Time; clock is the advancing/adjustable time source the
	// service reads when dated (nil-load never happens: set before svc is built),
	// and recallNow is the clock recall runs under after ingest.
	dated     bool
	clock     atomic.Pointer[time.Time]
	recallNow time.Time
	// queryRewrite enables LLM query expansion on the hybrid Recall path.
	queryRewrite bool
	// alias maps stored memory ID -> the dataset item IDs that landed on it
	// (write mode only; built once during ingest, read-only afterwards).
	alias  map[string][]string
	once   sync.Once
	ingErr error
}

// effectiveNow is the time recall (and derived-row listing) runs under: the
// configured RecallNow when dated, otherwise the fixed benchClock.
func (b *meminiBackend) effectiveNow() time.Time {
	if b.dated && !b.recallNow.IsZero() {
		return b.recallNow
	}
	return benchClock()
}

// benchClock pins recall's time source to the ingest timestamp so a benchmark
// measures pure retrieval ranking: with all memories sharing one LastAccessedAt
// the recency factor is uniform and the composite re-ranker reduces to the RRF
// order. Combined with synchronous reinforcement, runs are deterministic and
// free of background writes racing the next query.
var benchClock = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

func newMeminiBackend(st store.Store, e embed.Embedder, o SystemOpts) *meminiBackend {
	if o.Concurrency < 1 {
		o.Concurrency = 1
	}
	if o.Mode == "" {
		o.Mode = IngestUpsert
	}
	b := &meminiBackend{
		store: st, embedder: e, queryPrefix: o.QueryPrefix, concurrency: o.Concurrency,
		mode: o.Mode, distiller: o.Distiller, dated: o.Dated, recallNow: o.RecallNow,
		queryRewrite: o.QueryRewrite,
	}
	// When dated, the service reads a mutable clock: ingest advances it per item,
	// then parks it at effectiveNow for the recall phase. Undated runs keep the
	// fixed benchClock so historical LongMemEval/LoCoMo numbers are unchanged.
	clockFn := benchClock
	if o.Dated {
		start := benchClock()
		b.clock.Store(&start)
		clockFn = func() time.Time { return *b.clock.Load() }
	}
	opts := []service.Option{
		service.WithClock(clockFn), service.WithSyncReinforce(),
		service.WithQueryPrefix(o.QueryPrefix), service.WithScoreFusion(o.FusionAlpha),
		service.WithRecallPool(o.PoolFactor, o.PoolFloor),
	}
	if o.Answerer != nil {
		opts = append(opts, service.WithAnswerer(o.Answerer))
	}
	if o.Mode == IngestWrite {
		// Mirror the shipped server's write-path wiring (cmd/memini/root.go):
		// write-dedup hint band, corroboration, contradiction invalidation, the
		// episodic low-signal gate, and heuristic extract-on-write. Recall-side
		// settings stay identical to upsert mode so the two modes differ only in
		// how the corpus was written.
		opts = append(opts,
			service.WithWriteDedup(0.625, service.WriteDedupHint),
			service.WithCorroboration(0.70),
			service.WithContradictionDownrank(0.625),
			service.WithEpisodicMinChars(120),
			service.WithExtractOnWrite(true),
		)
		// Distill-on-write mirrors the shipped server's LLM wiring: with a
		// distiller set it supersedes the heuristic extractor (production
		// behavior).
		if o.Distiller != nil {
			opts = append(opts,
				service.WithDistiller(o.Distiller),
				service.WithDistillOnWrite(true),
			)
		}
	}
	b.svc = service.New(st, e, opts...)
	return b
}

// ingest loads the corpus once: direct upserts (upsert mode) or the production
// write path (write mode).
func (b *meminiBackend) ingest(ctx context.Context, items []Item) error {
	b.once.Do(func() {
		if b.mode == IngestWrite {
			b.ingestWrite(ctx, items)
			return
		}
		b.ingestUpsert(ctx, items)
	})
	return b.ingErr
}

// ingestUpsert embeds item windows concurrently and upserts under a single
// lock (sqlite is single-writer).
func (b *meminiBackend) ingestUpsert(ctx context.Context, items []Item) {
	now := time.Unix(1_700_000_000, 0).UTC()
	var (
		wg       sync.WaitGroup
		upsertMu sync.Mutex
		errMu    sync.Mutex
	)
	sem := make(chan struct{}, b.concurrency)
	setErr := func(err error) {
		errMu.Lock()
		if b.ingErr == nil {
			b.ingErr = err
		}
		errMu.Unlock()
	}

	for start := 0; start < len(items); start += ingestWindow {
		end := min(start+ingestWindow, len(items))
		wg.Add(1)
		sem <- struct{}{}
		go func(window []Item) {
			defer wg.Done()
			defer func() { <-sem }()

			dp := docPrefix()
			texts := make([]string, len(window))
			for i, it := range window {
				texts[i] = dp + it.Content
			}
			vecs, err := b.embedder.Embed(ctx, texts)
			if err != nil {
				setErr(err)
				return
			}
			upsertMu.Lock()
			defer upsertMu.Unlock()
			for i, it := range window {
				ts := now
				var validFrom *time.Time
				if b.dated && !it.Time.IsZero() {
					ts = it.Time
					vf := it.Time
					validFrom = &vf
				}
				if err := b.store.Upsert(ctx, &memory.Memory{
					ID: it.ID, Namespace: nsOf(it.Group), Tier: memory.TierSemantic,
					Content: it.Content, CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts,
					ValidFrom: validFrom, Embedding: vecs[i],
				}); err != nil {
					setErr(err)
					return
				}
			}
		}(items[start:end])
	}
	wg.Wait()
	// Park the recall clock at effectiveNow so temporal targeting aims from the
	// query's reference time, not the ingest start.
	if b.dated {
		rn := b.effectiveNow()
		b.clock.Store(&rn)
	}
}

// ingestWrite feeds items through service.Remember sequentially in dataset
// order (write-path interactions like corroborate/contradict are
// order-sensitive; sequential ingest keeps runs deterministic). Remember
// generates its own IDs, so the alias map records which item(s) each stored
// memory answers for.
func (b *meminiBackend) ingestWrite(ctx context.Context, items []Item) {
	if docPrefix() != "" {
		fmt.Fprintln(os.Stderr,
			"bench: MEMINI_EMBED_DOC_PREFIX ignored with write-mode ingest (production Remember embeds raw content)")
	}
	b.alias = make(map[string][]string, len(items))
	never := -time.Second
	var gated, merged int
	for _, it := range items {
		in := service.RememberInput{Namespace: nsOf(it.Group), Content: it.Content}
		if b.dated {
			// Advance the per-item clock so corroborate/contradict and valid_to
			// invalidation see the real chronology; never-expire TTL (question
			// dates can fall long after a session); session_id metadata for the
			// session-echo guard.
			if !it.Time.IsZero() {
				t := it.Time.UTC()
				b.clock.Store(&t)
				vf := it.Time.UTC()
				in.ValidFrom = &vf
			}
			in.TTL = &never
			if it.Session != "" {
				in.Metadata = map[string]any{"session_id": it.Session}
			}
		}
		m, err := b.svc.Remember(ctx, in)
		if err != nil {
			b.ingErr = fmt.Errorf("write-mode ingest %s: %w", it.ID, err)
			return
		}
		if m == nil { // dropped by the episodic low-signal gate: accepted, not stored
			gated++
			continue
		}
		if len(b.alias[m.ID]) > 0 { // fingerprint/dedup landed this item on an existing memory
			merged++
		}
		b.alias[m.ID] = append(b.alias[m.ID], it.ID)
	}
	if b.dated {
		rn := b.effectiveNow()
		b.clock.Store(&rn)
	}
	// Settle detached side-effects (extract, corroborate, contradict,
	// auto-supersede) and any queued LLM consolidation before the first recall.
	b.svc.WaitBackground()
	if err := b.svc.FlushConsolidation(ctx); err != nil {
		b.ingErr = fmt.Errorf("write-mode ingest: flush consolidation: %w", err)
		return
	}
	fmt.Fprintf(os.Stderr, "bench: write-mode ingest: %d items -> %d memories, %d gated, %d merged\n",
		len(items), len(b.alias), gated, merged)
	if err := b.attributeDerived(ctx, items); err != nil {
		b.ingErr = fmt.Errorf("write-mode ingest: attribute derived rows: %w", err)
	}
}

// attributeDerived maps rows the write path derived itself (extract-on-write
// facts, distilled facts) back to the dataset items their source episodics
// answer for, via the provenance metadata stamped at derivation (source_id,
// source_ids, promoted_from). Without this a derived row in the top-K can
// never count as a hit even when it carries the gold session's content, so
// fact-building arms would be structurally penalized on recall.
func (b *meminiBackend) attributeDerived(ctx context.Context, items []Item) error {
	namespaces := map[string]bool{}
	for _, it := range items {
		namespaces[nsOf(it.Group)] = true
	}
	derived := 0
	for ns := range namespaces {
		mems, err := b.store.List(ctx, ns, store.Filter{Now: b.effectiveNow()}, 0)
		if err != nil {
			return err
		}
		for _, m := range mems {
			if len(b.alias[m.ID]) > 0 {
				continue
			}
			var itemIDs []string
			seen := map[string]bool{}
			for _, src := range sourceMemoryIDs(m.Metadata) {
				for _, id := range b.alias[src] {
					if !seen[id] {
						seen[id] = true
						itemIDs = append(itemIDs, id)
					}
				}
			}
			if len(itemIDs) > 0 {
				b.alias[m.ID] = itemIDs
				derived++
			}
		}
	}
	if derived > 0 {
		fmt.Fprintf(os.Stderr, "bench: write-mode ingest: %d derived rows attributed to source items\n", derived)
	}
	return nil
}

// sourceMemoryIDs collects the provenance pointers a derived row carries.
// source_ids survives the store's JSON metadata roundtrip as []any.
func sourceMemoryIDs(meta map[string]any) []string {
	if meta == nil {
		return nil
	}
	var ids []string
	if s, ok := meta["source_id"].(string); ok && s != "" {
		ids = append(ids, s)
	}
	if s, ok := meta["promoted_from"].(string); ok && s != "" {
		ids = append(ids, s)
	}
	switch v := meta["source_ids"].(type) {
	case []string:
		ids = append(ids, v...)
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				ids = append(ids, s)
			}
		}
	}
	return ids
}

// hits converts scored results to RecallHits, translating stored-memory IDs
// back to dataset item IDs in write mode.
func (b *meminiBackend) hits(res []store.Scored) []RecallHit {
	out := make([]RecallHit, len(res))
	for i, r := range res {
		ids := []string{r.Memory.ID}
		if a, ok := b.alias[r.Memory.ID]; ok {
			ids = a
		}
		out[i] = RecallHit{IDs: ids, Content: r.Memory.Content}
	}
	return out
}

// MeminiSystems returns the hybrid, vector-only, and keyword-only retrieval
// strategies sharing one ingested store. queryPrefix, when non-empty, is
// prepended to query embeddings (hybrid and vector legs), matching
// MEMINI_EMBED_QUERY_PREFIX in production. fusionAlpha < 0 uses RRF; >= 0 uses
// convex-combination score fusion with that vector weight. poolFactor/poolFloor
// override hybrid recall's per-leg pool sizing (non-positive keeps defaults).
// mode selects direct upserts (historical default) or the production write path.
// distiller, non-nil with write mode, enables LLM distill-on-write (nil keeps
// the heuristic extractor).
func MeminiSystems(
	st store.Store, e embed.Embedder, concurrency int, queryPrefix string, fusionAlpha float64,
	poolFactor, poolFloor int, mode IngestMode, distiller llm.Distiller,
) []System {
	return MeminiSystemsOpts(st, e, SystemOpts{
		Concurrency: concurrency, QueryPrefix: queryPrefix, FusionAlpha: fusionAlpha,
		PoolFactor: poolFactor, PoolFloor: poolFloor, Mode: mode, Distiller: distiller,
	})
}

// MeminiSystemsOpts is MeminiSystems with the full option set, including dated
// ingest (SystemOpts.Dated) for temporally-ordered corpora.
func MeminiSystemsOpts(st store.Store, e embed.Embedder, o SystemOpts) []System {
	b := newMeminiBackend(st, e, o)
	return []System{
		&hybridSystem{b},
		&vectorSystem{b},
		&keywordSystem{b},
	}
}

type hybridSystem struct{ b *meminiBackend }

func (s *hybridSystem) Name() string {
	if s.b.queryRewrite {
		return "memini-hybrid-rewrite"
	}
	return "memini-hybrid"
}
func (s *hybridSystem) Ingest(ctx context.Context, it []Item) error { return s.b.ingest(ctx, it) }
func (s *hybridSystem) Recall(ctx context.Context, group, q string, k int) ([]RecallHit, error) {
	res, err := s.b.svc.Recall(ctx, service.RecallInput{Namespace: nsOf(group), Query: q, Limit: k, QueryRewrite: s.b.queryRewrite})
	if err != nil {
		return nil, err
	}
	return s.b.hits(res), nil
}

type vectorSystem struct{ b *meminiBackend }

func (s *vectorSystem) Name() string                                { return "memini-vector" }
func (s *vectorSystem) Ingest(ctx context.Context, it []Item) error { return s.b.ingest(ctx, it) }
func (s *vectorSystem) Recall(ctx context.Context, group, q string, k int) ([]RecallHit, error) {
	vec, err := embed.EmbedOne(ctx, s.b.embedder, s.b.queryPrefix+q)
	if err != nil {
		return nil, err
	}
	res, err := s.b.store.VectorSearch(ctx, nsOf(group), vec, store.Filter{}, k)
	if err != nil {
		return nil, err
	}
	return s.b.hits(res), nil
}

type keywordSystem struct{ b *meminiBackend }

func (s *keywordSystem) Name() string                                { return "memini-keyword" }
func (s *keywordSystem) Ingest(ctx context.Context, it []Item) error { return s.b.ingest(ctx, it) }
func (s *keywordSystem) Recall(ctx context.Context, group, q string, k int) ([]RecallHit, error) {
	res, err := s.b.store.KeywordSearch(ctx, nsOf(group), q, store.Filter{}, k)
	if err != nil {
		return nil, err
	}
	return s.b.hits(res), nil
}

func scoredIDs(res []store.Scored) []string {
	out := make([]string, len(res))
	for i, r := range res {
		out[i] = r.Memory.ID
	}
	return out
}
