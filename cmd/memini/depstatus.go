package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/rerank"
	"github.com/eleboucher/memini/internal/server"
)

// bootProbeTimeout bounds the one-shot startup embed probe. Short and
// non-fatal: it exists to surface an unreachable embeddings endpoint in the
// startup log instead of on the first write, not to gate boot.
const bootProbeTimeout = 3 * time.Second

// recordingEmbedder wraps an embed.Embedder, recording every call's outcome
// in a server.DepTracker so GET /healthz?verbose=1 can report embedder
// health. This is the single hook point for embedder dep-status: it sits
// outermost in buildServiceStack's wrapping chain, so it sees every Embed
// call the service pipeline makes (including the boot probe below), without
// needing a metrics-shaped interface threaded through the embed package.
type recordingEmbedder struct {
	embed.Embedder
	deps *server.DepTracker
}

func (r recordingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vecs, err := r.Embedder.Embed(ctx, texts)
	r.deps.Record("embedder", err)
	return vecs, err
}

// recordingLLM wraps an llm.Client the same way recordingEmbedder wraps an
// embed.Embedder: every call's outcome feeds the same dep-status tracker under
// the "llm" key. It overrides the three methods below; llm.Client's other two
// (Merge and AssessImportance) reach the embedded client unwrapped, so their
// outcomes do not yet move dep status.
type recordingLLM struct {
	llm.Client
	deps *server.DepTracker
}

func (r recordingLLM) Consolidate(ctx context.Context, in llm.Input) (llm.Decision, error) {
	d, err := r.Client.Consolidate(ctx, in)
	r.deps.Record("llm", err)
	return d, err
}

func (r recordingLLM) Complete(ctx context.Context, system, user string) (string, error) {
	out, err := r.Client.Complete(ctx, system, user)
	r.deps.Record("llm", err)
	return out, err
}

func (r recordingLLM) Distill(ctx context.Context, in llm.DistillInput) ([]llm.Fact, error) {
	facts, err := r.Client.Distill(ctx, in)
	r.deps.Record("llm", err)
	return facts, err
}

// recordingReranker wraps a rerank.Reranker the same way recordingEmbedder
// wraps an embed.Embedder: every recall-path Rerank outcome feeds the
// dep-status tracker under the "reranker" key.
type recordingReranker struct {
	rerank.Reranker
	deps *server.DepTracker
}

func (r recordingReranker) Rerank(ctx context.Context, query string, candidates []rerank.Candidate) ([]string, error) {
	ids, err := r.Reranker.Rerank(ctx, query, candidates)
	r.deps.Record("reranker", err)
	return ids, err
}

// probeEmbedder performs a one-shot tiny embed at startup so an unreachable
// embeddings endpoint is surfaced immediately in the log instead of silently
// on the first write. Non-fatal: the server still starts (recall degrades to
// keyword-only). embedder is expected to already be wrapped in
// recordingEmbedder, so the outcome lands in the tracker without this
// function touching it directly.
func probeEmbedder(ctx context.Context, embedder embed.Embedder, log *slog.Logger) {
	probeCtx, cancel := context.WithTimeout(ctx, bootProbeTimeout)
	defer cancel()
	if _, err := embed.EmbedOne(probeCtx, embedder, "ping"); err != nil {
		log.Warn("embeddings endpoint unreachable at startup — writes will degrade to keyword-only", "err", err)
	}
}
