package service_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/chunk"
	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/rerank"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// The bug chunking fixes, reproduced: the embedder is wrapped in Batched with a
// per-item cap, exactly as cmd/memini wires it, so a long memory's vector covers
// only its prefix. Small numbers keep the fixture readable; the shape is the
// production one.
const (
	testEmbedMaxItemChars = 300
	testChunkSize         = 120
)

func testChunkCfg() chunk.Config {
	return chunk.Config{Size: testChunkSize, Overlap: 20, MinContent: testChunkSize, MaxChunks: 64}
}

// longMemoryWithBuriedPhrase returns content whose distinctive phrase sits past
// testEmbedMaxItemChars, so the document vector cannot represent it.
func longMemoryWithBuriedPhrase(phrase string) string {
	filler := strings.Repeat("the deployment pipeline runs tests in parallel across sixteen cores. ", 12)
	return filler + "\n\n" + phrase
}

func chunkTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "chunk.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// truncatingEmbedder is the production wrapping: Batched truncates any single
// text to maxItemChars before embedding.
func truncatingEmbedder() embed.Embedder {
	return embed.NewBatched(embedtest.New(dims), 20, 0, testEmbedMaxItemChars)
}

func chunkService(t *testing.T, st store.Store, opts ...service.Option) *service.Service {
	t.Helper()
	base := make([]service.Option, 0, 2+len(opts))
	base = append(base,
		service.WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
		service.WithSyncReinforce(),
	)
	return service.New(st, truncatingEmbedder(), append(base, opts...)...)
}

// TestChunkedRecallFindsABuriedPhrase is the whole point of the feature, stated
// as a test: without chunking the tail of a long memory is unreachable by vector
// recall; with it, the same query finds it.
func TestChunkedRecallFindsABuriedPhrase(t *testing.T) {
	ctx := context.Background()
	const phrase = "zarquon calibration uses the tertiary flange"
	content := longMemoryWithBuriedPhrase(phrase)
	if len([]rune(content)) <= testEmbedMaxItemChars {
		t.Fatalf("setup: content must exceed the embed cap to bury the phrase, got %d runes", len([]rune(content)))
	}

	t.Run("without chunking the buried phrase is unreachable by vector recall", func(t *testing.T) {
		st := chunkTestStore(t)
		svc := chunkService(t, st)
		mustRememberLong(t, svc, content)

		// The defect is in the VECTOR leg specifically — keyword search can still
		// find the phrase, which is exactly why the bug is so quiet — so assert
		// on the vector leg rather than on Recall's fused output.
		if hasVectorHit(t, st, "alice", phrase) {
			t.Fatal("precondition failed: the document vector reached the buried phrase, " +
				"so this fixture does not reproduce the truncation bug")
		}
	})

	t.Run("with chunking the buried phrase is found", func(t *testing.T) {
		st := chunkTestStore(t)
		svc := chunkService(t, st, service.WithChunkEmbed(testChunkCfg()))
		mustRememberLong(t, svc, content)

		// Chunks are built by the backfill, not the write path.
		n, err := svc.BackfillChunks(ctx)
		if err != nil {
			t.Fatalf("backfill chunks: %v", err)
		}
		if n != 1 {
			t.Fatalf("backfilled %d memories, want 1", n)
		}

		// Assert on the CHUNK leg, not on Recall. Recall fuses keyword with
		// vector, and keyword search finds this phrase with or without chunking —
		// which is exactly why the bug was so quiet, and would make an assertion
		// on Recall's output pass while proving nothing. (Verified: with the
		// backfill above removed, a Recall-based assertion still passed.)
		if !hasChunkHit(t, st, "alice", phrase) {
			t.Fatal("the chunk leg did not reach the buried phrase — chunked recall is not working")
		}
		// And the whole pipeline still returns it, through the union.
		got, err := svc.Recall(ctx, service.RecallInput{Namespace: "alice", Query: phrase, Limit: 5})
		if err != nil {
			t.Fatalf("recall: %v", err)
		}
		found := false
		for _, r := range got {
			if strings.Contains(r.Memory.Content, phrase) {
				found = true
			}
		}
		if !found {
			t.Fatal("recall did not return the memory containing the buried phrase")
		}
	})
}

// TestChunkBackfillIsIdempotent pins that a second tick does no work:
// ListUnchunked must stop returning a row once it has chunks, or the loop would
// re-embed the same memories forever.
func TestChunkBackfillIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := chunkTestStore(t)
	svc := chunkService(t, st, service.WithChunkEmbed(testChunkCfg()))
	mustRememberLong(t, svc, longMemoryWithBuriedPhrase("a buried detail"))

	first, err := svc.BackfillChunks(ctx)
	if err != nil || first != 1 {
		t.Fatalf("first backfill = (%d, %v), want (1, nil)", first, err)
	}
	second, err := svc.BackfillChunks(ctx)
	if err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if second != 0 {
		t.Fatalf("second backfill did %d memories, want 0 — the loop would re-embed forever", second)
	}
}

// TestChunkBackfillSkipsShortMemories pins the design's structural claim: a
// short memory gets no chunks, so its document vector is never duplicated.
func TestChunkBackfillSkipsShortMemories(t *testing.T) {
	ctx := context.Background()
	st := chunkTestStore(t)
	svc := chunkService(t, st, service.WithChunkEmbed(testChunkCfg()))
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Tier: memory.TierSemantic, Content: "a short durable fact about the api gateway",
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	n, err := svc.BackfillChunks(ctx)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 0 {
		t.Fatalf("backfilled %d short memories, want 0", n)
	}
}

// TestChunkingOffIsANoop pins that the flag off leaves the store exactly as it
// was: no chunk rows, no backfill work, and recall unchanged.
func TestChunkingOffIsANoop(t *testing.T) {
	ctx := context.Background()
	st := chunkTestStore(t)
	svc := chunkService(t, st) // no WithChunkEmbed
	mustRememberLong(t, svc, longMemoryWithBuriedPhrase("a buried detail"))

	n, err := svc.BackfillChunks(ctx)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 0 {
		t.Fatalf("backfill did %d memories with chunking off, want 0", n)
	}
	cs, ok := st.(store.ChunkStore)
	if !ok {
		t.Fatal("sqlitevec should implement ChunkStore")
	}
	un, err := cs.ListUnchunked(ctx, "alice", 1, "", 10)
	if err != nil {
		t.Fatalf("list unchunked: %v", err)
	}
	if len(un) == 0 {
		t.Fatal("the long memory should still be listed as unchunked — nothing should have chunked it")
	}
}

// docOnlyStore hides the ChunkStore capability: embedding the INTERFACE (not a
// concrete driver) forwards every Store method while the wrapper type itself
// never gains the chunk methods — which is exactly what a driver without the
// capability looks like to WithChunkEmbed's type assertion.
type docOnlyStore struct{ store.Store }

// TestChunkEmbedWithoutCapableStoreDegrades pins that the option is advisory: a
// driver without the capability keeps working rather than erroring at boot, and
// the backfill does nothing rather than panicking. The earlier version of this
// test ran against sqlitevec — which implements ChunkStore — so the branch it
// existed for was never taken.
func TestChunkEmbedWithoutCapableStoreDegrades(t *testing.T) {
	ctx := context.Background()
	st := docOnlyStore{chunkTestStore(t)}
	if _, ok := any(st).(store.ChunkStore); ok {
		t.Fatal("setup: the wrapper must hide the ChunkStore capability")
	}
	svc := chunkService(t, st, service.WithChunkEmbed(testChunkCfg()))
	mustRememberLong(t, svc, longMemoryWithBuriedPhrase("a buried detail"))

	if n, err := svc.BackfillChunks(ctx); err != nil || n != 0 {
		t.Fatalf("backfill on an incapable store = (%d, %v), want (0, nil)", n, err)
	}
	if _, err := svc.Recall(ctx, service.RecallInput{Namespace: "alice", Query: "buried detail", Limit: 5}); err != nil {
		t.Fatalf("recall must keep working without the capability: %v", err)
	}
}

// TestChunkBackfillPreservesTheDocumentVector pins the fix for the worst bug
// this feature shipped with. The backfill's old Get-then-Upsert round-trip
// could never carry the document vector — Get does not load it, and both
// backends read an empty Embedding as clear-the-vector — so it destroyed the
// vector of every memory it chunked. Silently: every other post-backfill
// assertion in this file passes through the chunk or keyword leg. This one
// asserts through the DOCUMENT leg, on the lead of the content, which the
// (truncated) document vector must keep reaching after chunks are attached.
func TestChunkBackfillPreservesTheDocumentVector(t *testing.T) {
	ctx := context.Background()
	const lead = "the omega deployment checklist for the gateway"
	content := lead + ". " + longMemoryWithBuriedPhrase("a buried detail")

	st := chunkTestStore(t)
	svc := chunkService(t, st, service.WithChunkEmbed(testChunkCfg()))
	mustRememberLong(t, svc, content)
	if !hasVectorHit(t, st, "alice", lead) {
		t.Fatal("precondition: the document vector must reach the memory's lead before the backfill")
	}

	if n, err := svc.BackfillChunks(ctx); err != nil || n != 1 {
		t.Fatalf("backfill = (%d, %v), want (1, nil)", n, err)
	}
	if !hasVectorHit(t, st, "alice", lead) {
		t.Fatal("the backfill destroyed the document vector while attaching chunks")
	}
}

// failingEmbedder rejects any batch containing marker — every batch when marker
// is empty — and passes the rest through. It models the two embed-backend
// failures the backfill must tell apart: a poison input the backend rejects
// deterministically (400/413) and an outage that rejects everything.
type failingEmbedder struct {
	inner  embed.Embedder
	marker string
}

func (f *failingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	for _, s := range texts {
		if f.marker == "" || strings.Contains(s, f.marker) {
			return nil, errors.New("embedder rejected the batch")
		}
	}
	return f.inner.Embed(ctx, texts)
}

func (f *failingEmbedder) Dims() int { return f.inner.Dims() }

// TestChunkBackfillSkipsAPoisonRow pins that one deterministically-failing row
// cannot wedge the queue. The queue's order is deterministic (by id), so before
// the run-length failure rule a poison row at the head aborted every tick at
// "first row failed => embedder down" and nothing behind it was ever chunked.
func TestChunkBackfillSkipsAPoisonRow(t *testing.T) {
	ctx := context.Background()
	st := chunkTestStore(t)
	svc := service.New(st,
		&failingEmbedder{inner: truncatingEmbedder(), marker: "poisoned"},
		service.WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
		service.WithSyncReinforce(),
		// The production wiring: a write whose embed fails degrades to a
		// vectorless pending_embed row instead of erroring.
		service.WithWriteEmbedTimeout(time.Second),
		service.WithChunkEmbed(testChunkCfg()))

	// Every chunk of the poison memory carries the marker, so its batch fails
	// every time. Its write-path embed fails too, which only degrades the
	// write (stored without a vector) — the row lands in the queue either way.
	mustRememberLong(t, svc, strings.Repeat("poisoned segment that the embed backend rejects. ", 12))
	mustRememberLong(t, svc, longMemoryWithBuriedPhrase("a buried detail"))

	n, err := svc.BackfillChunks(ctx)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 1 {
		t.Fatalf("backfilled %d memories, want 1: the healthy row must be chunked past the poison one", n)
	}
	if left, err := svc.ChunkBacklog(ctx); err != nil || left != 1 {
		t.Fatalf("backlog = (%d, %v), want the poison row alone", left, err)
	}
}

// TestChunkBackfillDefersTickWhenEmbedderIsDown pins the other side of the
// run-length rule: two different rows failing back to back is an outage, and
// the tick defers (0, nil) rather than erroring — the ticker retries, and a
// warn-per-tick is the right noise level for a down embedder.
func TestChunkBackfillDefersTickWhenEmbedderIsDown(t *testing.T) {
	ctx := context.Background()
	st := chunkTestStore(t)
	svc := service.New(st,
		&failingEmbedder{inner: truncatingEmbedder()}, // no marker: rejects everything
		service.WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
		service.WithSyncReinforce(),
		service.WithWriteEmbedTimeout(time.Second),
		service.WithChunkEmbed(testChunkCfg()))

	mustRememberLong(t, svc, longMemoryWithBuriedPhrase("first buried detail"))
	mustRememberLong(t, svc, strings.Repeat("a second long memory about the gateway. ", 8))

	if n, err := svc.BackfillChunks(ctx); err != nil || n != 0 {
		t.Fatalf("backfill under an outage = (%d, %v), want (0, nil)", n, err)
	}
	if left, err := svc.ChunkBacklog(ctx); err != nil || left != 2 {
		t.Fatalf("backlog = (%d, %v), want both rows still queued", left, err)
	}
}

// mustRememberLong stores content in the "alice" namespace, which every case
// here uses.
func mustRememberLong(t *testing.T, svc *service.Service, content string) {
	t.Helper()
	if _, err := svc.Remember(context.Background(), service.RememberInput{
		Namespace: "alice", Tier: memory.TierSemantic, Content: content,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}
}

// queryVec embeds a query the way recall would, untruncated (a query is short).
func queryVec(t *testing.T, phrase string) []float32 {
	t.Helper()
	vecs, err := embedtest.New(dims).Embed(context.Background(), []string{phrase})
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	return vecs[0]
}

// hasVectorHit reports whether the DOCUMENT vector leg finds the phrase.
func hasVectorHit(t *testing.T, st store.Store, ns, phrase string) bool {
	t.Helper()
	got, err := st.VectorSearch(context.Background(), ns, queryVec(t, phrase), store.Filter{}, 5)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	return containsPhrase(got, phrase)
}

// hasChunkHit reports whether the CHUNK vector leg finds the phrase.
func hasChunkHit(t *testing.T, st store.Store, ns, phrase string) bool {
	t.Helper()
	cs, ok := st.(store.ChunkStore)
	if !ok {
		t.Fatal("store does not implement ChunkStore")
	}
	got, err := cs.ChunkVectorSearch(context.Background(), ns, queryVec(t, phrase), store.Filter{}, 5)
	if err != nil {
		t.Fatalf("chunk search: %v", err)
	}
	return containsPhrase(got, phrase)
}

// containsPhrase reports whether a strongly-scoring hit holds the phrase. The
// 0.5 floor separates "this vector represents the phrase" from the weak
// similarity every memory in a small fixture has to every query.
func containsPhrase(got []store.Scored, phrase string) bool {
	for _, g := range got {
		if strings.Contains(g.Memory.Content, phrase) && g.Score > 0.5 {
			return true
		}
	}
	return false
}

// gaugeRecorder records the chunk-backfill gauge.
type gaugeRecorder struct {
	service.Metrics
	last  int
	calls int
}

func (g *gaugeRecorder) ChunkBackfillPending(n int) { g.last = n; g.calls++ }

// TestChunkBackfillReportsItsBacklog pins the gauge, and specifically pins it
// PAST the batch size. The gauge's first version was computed from the batch
// (found - done), so it could never exceed 25 and read 0 after every healthy
// tick regardless of queue depth — an operator watching it while a 10k-memory
// backlog ground through at 25 a minute saw "done" from the first tick. Only a
// queue deeper than one batch can catch that class of bug, which is why this
// fixture writes 30 rows rather than 1.
func TestChunkBackfillReportsItsBacklog(t *testing.T) {
	ctx := context.Background()
	st := chunkTestStore(t)
	g := &gaugeRecorder{Metrics: service.NopMetrics()}
	svc := chunkService(t, st, service.WithChunkEmbed(testChunkCfg()), service.WithMetrics(g))

	// 30 = chunkBackfillBatch + 5. Contents must differ structurally — not
	// just by an index buried in identical filler — so write-dedup cannot
	// coalesce them into fewer rows than the fixture counts on.
	const rows = 30
	for i := range rows {
		mustRememberLong(t, svc, strings.Repeat(fmt.Sprintf("unique fact %d about subsystem u%dx. ", i, i), 6))
	}

	done, err := svc.BackfillChunks(ctx)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if done != 25 {
		t.Fatalf("first tick chunked %d memories, want the full batch of 25", done)
	}
	if g.calls == 0 {
		t.Fatal("the chunk backfill never reported its backlog: an operator cannot see it stall")
	}
	if g.last != rows-done {
		t.Errorf("pending = %d after one tick over %d rows, want %d: the gauge must report the queue, not the batch",
			g.last, rows, rows-done)
	}

	// The second tick drains the remainder and the gauge reaches 0.
	if _, err := svc.BackfillChunks(ctx); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if g.last != 0 {
		t.Errorf("pending = %d after draining the queue, want 0", g.last)
	}
}

// phraseReranker models a real reranker faithfully in the one way that matters
// here: it judges each candidate on a TRUNCATED view of the content it is
// handed, because that is what both shipped backends do. CrossEncoder cuts to
// MEMINI_RERANK_MAX_DOC_CHARS (2048 runes) at crossencoder.go:111, and the LLM
// reranker cuts to 300 bytes at llm.go:43. A candidate it cannot see the
// evidence in is omitted, and per rerank.Reranker's contract an omitted
// candidate is DROPPED from the results.
type phraseReranker struct {
	phrase   string
	maxChars int               // mirrors the shipped rerankers' per-candidate cut
	saw      map[string]string // id -> the text it actually got to judge
}

func (r *phraseReranker) Rerank(_ context.Context, _ string, cands []rerank.Candidate) ([]string, error) {
	if r.saw == nil {
		r.saw = map[string]string{}
	}
	var keep []string
	for _, c := range cands {
		view := c.Content
		if n := []rune(view); len(n) > r.maxChars {
			view = string(n[:r.maxChars])
		}
		r.saw[c.ID] = view
		if strings.Contains(view, r.phrase) {
			keep = append(keep, c.ID)
		}
	}
	return keep, nil
}

// TestRerankSeesTheMatchedChunk pins the interaction between chunking and
// reranking, which is the same bug as the original one layer up.
//
// Chunked recall surfaces a memory because of a passage deep in its content.
// Rerank is then handed the whole memory and cuts it down to its own budget:
// 300 bytes for the LLM reranker, 2048 runes for the cross-encoder. The passage
// that caused the hit is not in that prefix, so the memory looks irrelevant, and
// because rerank's pool is deliberately deeper than k it changes membership
// rather than just order: the memory is dropped. Chunking pays the embedder to
// find it and the next stage throws it away.
//
// Passing the matched chunk instead means rerank judges the text that actually
// matched.
func TestRerankSeesTheMatchedChunk(t *testing.T) {
	ctx := context.Background()
	const phrase = "zarquon calibration uses the tertiary flange"
	content := longMemoryWithBuriedPhrase(phrase)

	st := chunkTestStore(t)
	// 300 is the LLM reranker's shipped per-candidate budget (rerank/llm.go).
	rr := &phraseReranker{phrase: phrase, maxChars: 300}
	svc := chunkService(t, st,
		service.WithChunkEmbed(testChunkCfg()),
		service.WithReranker(rr, "phrase-test"),
	)
	mustRememberLong(t, svc, content)
	if _, err := svc.BackfillChunks(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	got, err := svc.Recall(ctx, service.RecallInput{Namespace: "alice", Query: phrase, Limit: 5})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}

	var judged string
	for _, v := range rr.saw {
		judged = v
	}
	if judged == "" {
		t.Fatal("the reranker was handed nothing to judge")
	}
	if !strings.Contains(judged, phrase) {
		t.Errorf("rerank judged this memory on text that does not contain the passage that "+
			"retrieved it, so it cannot tell the memory is relevant: it saw %d runes and the "+
			"phrase was not among them", len([]rune(judged)))
	}
	if len(got) == 0 {
		t.Fatal("rerank dropped the only memory containing the phrase: chunked recall found it, " +
			"then rerank discarded it because it was shown the wrong text")
	}
}
