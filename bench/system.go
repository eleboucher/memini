package bench

import (
	"context"
	"sync"
	"time"

	"github.com/eleboucher/memini/internal/embed"
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

// System is a memory system under test.
type System interface {
	Name() string
	Ingest(ctx context.Context, items []Item) error
	Recall(ctx context.Context, group, query string, k int) ([]string, error)
}

// meminiBackend holds the store/embedder shared across retrieval strategies;
// ingestion runs once and is reused.
type meminiBackend struct {
	store       store.Store
	embedder    embed.Embedder
	svc         *service.Service
	concurrency int
	once        sync.Once
	ingErr      error
}

func newMeminiBackend(st store.Store, e embed.Embedder, concurrency int) *meminiBackend {
	if concurrency < 1 {
		concurrency = 1
	}
	return &meminiBackend{store: st, embedder: e, svc: service.New(st, e), concurrency: concurrency}
}

// ingest embeds item windows concurrently and upserts under a single lock
// (sqlite is single-writer). Runs once.
func (b *meminiBackend) ingest(ctx context.Context, items []Item) error {
	b.once.Do(func() {
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

				texts := make([]string, len(window))
				for i, it := range window {
					texts[i] = it.Content
				}
				vecs, err := b.embedder.Embed(ctx, texts)
				if err != nil {
					setErr(err)
					return
				}
				upsertMu.Lock()
				defer upsertMu.Unlock()
				for i, it := range window {
					if err := b.store.Upsert(ctx, &memory.Memory{
						ID: it.ID, Namespace: nsOf(it.Group), Tier: memory.TierSemantic,
						Content: it.Content, CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
						Embedding: vecs[i],
					}); err != nil {
						setErr(err)
						return
					}
				}
			}(items[start:end])
		}
		wg.Wait()
	})
	return b.ingErr
}

// MeminiSystems returns the hybrid, vector-only, and keyword-only retrieval
// strategies sharing one ingested store.
func MeminiSystems(st store.Store, e embed.Embedder, concurrency int) []System {
	b := newMeminiBackend(st, e, concurrency)
	return []System{
		&hybridSystem{b},
		&vectorSystem{b},
		&keywordSystem{b},
	}
}

type hybridSystem struct{ b *meminiBackend }

func (s *hybridSystem) Name() string                                { return "memini-hybrid" }
func (s *hybridSystem) Ingest(ctx context.Context, it []Item) error { return s.b.ingest(ctx, it) }
func (s *hybridSystem) Recall(ctx context.Context, group, q string, k int) ([]string, error) {
	res, err := s.b.svc.Recall(ctx, service.RecallInput{Namespace: nsOf(group), Query: q, Limit: k})
	if err != nil {
		return nil, err
	}
	return scoredIDs(res), nil
}

type vectorSystem struct{ b *meminiBackend }

func (s *vectorSystem) Name() string                                { return "memini-vector" }
func (s *vectorSystem) Ingest(ctx context.Context, it []Item) error { return s.b.ingest(ctx, it) }
func (s *vectorSystem) Recall(ctx context.Context, group, q string, k int) ([]string, error) {
	vec, err := embed.EmbedOne(ctx, s.b.embedder, q)
	if err != nil {
		return nil, err
	}
	res, err := s.b.store.VectorSearch(ctx, nsOf(group), vec, store.Filter{}, k)
	if err != nil {
		return nil, err
	}
	return scoredIDs(res), nil
}

type keywordSystem struct{ b *meminiBackend }

func (s *keywordSystem) Name() string                                { return "memini-keyword" }
func (s *keywordSystem) Ingest(ctx context.Context, it []Item) error { return s.b.ingest(ctx, it) }
func (s *keywordSystem) Recall(ctx context.Context, group, q string, k int) ([]string, error) {
	res, err := s.b.store.KeywordSearch(ctx, nsOf(group), q, store.Filter{}, k)
	if err != nil {
		return nil, err
	}
	return scoredIDs(res), nil
}

func scoredIDs(res []store.Scored) []string {
	out := make([]string, len(res))
	for i, r := range res {
		out[i] = r.Memory.ID
	}
	return out
}
