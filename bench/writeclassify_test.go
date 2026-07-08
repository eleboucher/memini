//go:build bench

package bench_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// httpClientWithHeaders returns an *http.Client that injects headers from
// the MEMINI_HTTP_HEADERS env var (format: "key:value;key:value"). Returns
// nil when unset, so callers fall back to the SDK default.
func httpClientWithHeaders() *http.Client {
	raw := os.Getenv("MEMINI_HTTP_HEADERS")
	if raw == "" {
		return nil
	}
	headers := make(map[string]string)
	for _, pair := range strings.Split(raw, ";") {
		kv := strings.SplitN(pair, ":", 2)
		if len(kv) == 2 {
			headers[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	if len(headers) == 0 {
		return nil
	}
	return &http.Client{
		Timeout:   60 * time.Second,
		Transport: &headerTransport{base: http.DefaultTransport, headers: headers},
	}
}

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	return t.base.RoundTrip(req)
}

// writeExpect labels the expected write outcome for a (base, candidate) pair.
type writeExpect int

const (
	wantNew        writeExpect = iota // candidate is distinct → should add as new
	wantCoalesce                      // candidate is a restatement/paraphrase → should coalesce or hint
	wantContradict                    // candidate contradicts base → should supersede (via dedup or contradiction routing)
)

type writePair struct {
	base      string
	candidate string
	expect    writeExpect
}

// writeOutcome records what the write pipeline actually did for one pair.
type writeOutcome struct {
	wasNew bool // stored as a new memory (not coalesced/fingerprint-hit, not gated)
	merged bool // landed on an existing memory (fingerprint or write-dedup coalesce)
	hinted bool // MergeHint was returned
	gated  bool // dropped by episodic low-signal gate (nil, nil)
	// contradiction detected: the background contradiction route invalidated the
	// base, detected by checking whether the base was superseded after writes.
	contradicted bool
}

// buildPairsFromTriples extends the existing dedupTriples into classification
// pairs: each triple yields two pairs — (base, paraphrase) expected coalesce,
// and (base, distinct) expected new.
func buildPairsFromTriples() []writePair {
	pairs := make([]writePair, 0, len(dedupTriples)*2)
	for _, tr := range dedupTriples {
		pairs = append(pairs, writePair{tr.base, tr.paraphrase, wantCoalesce})
		pairs = append(pairs, writePair{tr.base, tr.distinct, wantNew})
	}
	return pairs
}

// buildPairsFromQuads extends the existing contradictionQuads into
// classification pairs: each quad yields three pairs — (base, restatement)
// expected coalesce, (base, update) expected contradict, and (base, distinct)
// expected new.
func buildPairsFromQuads() []writePair {
	pairs := make([]writePair, 0, len(contradictionQuads)*3)
	for _, q := range contradictionQuads {
		pairs = append(pairs, writePair{q.base, q.restatement, wantCoalesce})
		pairs = append(pairs, writePair{q.base, q.update, wantContradict})
		pairs = append(pairs, writePair{q.base, q.distinct, wantNew})
	}
	return pairs
}

// runOnePair writes the base, then the candidate, and observes the outcome.
// Returns the outcome and the base memory ID so the caller can check for
// contradiction (supersede) after the writes settle.
func runOnePair(
	ctx context.Context, svc *service.Service, ns string, pair writePair,
) (writeOutcome, string, error) {
	// Write the base at semantic tier so it is durable.
	baseRes, err := svc.Remember(ctx, service.RememberInput{
		Namespace: ns,
		Content:   pair.base,
		Tier:      memory.TierSemantic,
	})
	if err != nil {
		return writeOutcome{}, "", fmt.Errorf("write base: %w", err)
	}
	if baseRes == nil {
		return writeOutcome{}, "", fmt.Errorf("base was gated (unexpected for semantic)")
	}
	baseID := baseRes.ID

	// Write the candidate and observe what happened.
	var hint service.MergeHint
	var autoSuperseded bool
	candidateRes, err := svc.Remember(ctx, service.RememberInput{
		Namespace:      ns,
		Content:        pair.candidate,
		Tier:           memory.TierSemantic,
		MergeHint:      &hint,
		AutoSuperseded: &autoSuperseded,
	})
	if err != nil {
		return writeOutcome{}, "", fmt.Errorf("write candidate: %w", err)
	}

	out := writeOutcome{}
	if candidateRes == nil {
		out.gated = true
		return out, baseID, nil
	}

	// If the returned ID == baseID, the write was coalesced (fingerprint or
	// write-dedup coalesce).
	if candidateRes.ID == baseID {
		out.merged = true
		return out, baseID, nil
	}
	if candidateRes.ID != baseID {
		out.wasNew = true
	}
	if hint.SimilarID != "" {
		out.hinted = true
	}
	// Auto-supersede fires when write-dedup action is supersede; contradiction
	// routing fires in background separately. Record only auto-supersede here;
	// contradiction will be checked after WaitBackground.
	if autoSuperseded {
		out.contradicted = true
	}
	// LLM consolidation supersede (sync mode): consolidateSync handles the
	// supersede internally and returns early, so autoSuperseded is never set.
	// Detect by checking if the base memory was tombstoned.
	if candidateRes.ID != baseID && !autoSuperseded {
		base, err := svc.Get(ctx, ns, baseID)
		if err == nil && base.SupersededBy != nil && *base.SupersededBy == candidateRes.ID {
			out.contradicted = true
			out.wasNew = false
		}
	}
	return out, baseID, nil
}

// classifyScoreboard holds per-class precision/recall counts and computes
// precision, recall, F1, and overall accuracy.
type classifyScoreboard struct {
	n                                     int // total pairs
	trueNew, trueCoalesce, trueContradict int

	// Cells for the 3×3 confusion matrix: rows are expected classes, columns
	// are predicted classes.
	conf [3][3]int
}

const (
	colNew    = 0
	colCoal   = 1
	colContra = 2
)

func (sb *classifyScoreboard) record(pair writePair, out writeOutcome) {
	sb.n++

	predCoalesce := out.merged || out.hinted || out.gated
	predContra := out.contradicted

	col := colNew
	if predCoalesce {
		col = colCoal
	} else if predContra {
		col = colContra
	}

	switch pair.expect {
	case wantNew:
		if col == colNew {
			sb.trueNew++
		}
		sb.conf[0][col]++
	case wantCoalesce:
		if col == colCoal {
			sb.trueCoalesce++
		}
		sb.conf[1][col]++
	case wantContradict:
		if col == colContra {
			sb.trueContradict++
		}
		sb.conf[2][col]++
	}
}

func (sb *classifyScoreboard) report(t *testing.T) {
	t.Helper()

	pc := func(n, d int) float64 {
		if d == 0 {
			return 0
		}
		return float64(n) / float64(d)
	}
	f1 := func(prec, rec float64) float64 {
		if prec+rec == 0 {
			return 0
		}
		return 2 * prec * rec / (prec + rec)
	}

	// Sum each row (expected) and column (predicted).
	rowSum := [3]int{
		sb.conf[0][0] + sb.conf[0][1] + sb.conf[0][2],
		sb.conf[1][0] + sb.conf[1][1] + sb.conf[1][2],
		sb.conf[2][0] + sb.conf[2][1] + sb.conf[2][2],
	}
	colSum := [3]int{
		sb.conf[0][0] + sb.conf[1][0] + sb.conf[2][0],
		sb.conf[0][1] + sb.conf[1][1] + sb.conf[2][1],
		sb.conf[0][2] + sb.conf[1][2] + sb.conf[2][2],
	}

	precNew := pc(sb.trueNew, colSum[colNew])
	recNew := pc(sb.trueNew, rowSum[0])
	precCoal := pc(sb.trueCoalesce, colSum[colCoal])
	recCoal := pc(sb.trueCoalesce, rowSum[1])
	precContra := pc(sb.trueContradict, colSum[colContra])
	recContra := pc(sb.trueContradict, rowSum[2])

	correct := sb.trueNew + sb.trueCoalesce + sb.trueContradict
	accuracy := pc(correct, sb.n)

	// false-duplicate rate: fraction of distinct items wrongly classified as
	// coalesce or contradict.
	falseDupRate := pc(sb.conf[0][colCoal]+sb.conf[0][colContra], rowSum[0])
	// false-novelty rate: fraction of dup/contradict items wrongly classified
	// as new.
	falseNoveltyRate := pc(sb.conf[1][colNew]+sb.conf[2][colNew], rowSum[1]+rowSum[2])

	t.Logf("========== Write-Classification Scoreboard ==========")
	t.Logf("pairs: %d", sb.n)
	t.Logf("")
	t.Logf("class             | precision | recall   | F1")
	t.Logf("------------------+-----------+----------+------")
	t.Logf("new (distinct)    |    %5.1f%% |   %5.1f%% | %.3f", precNew*100, recNew*100, f1(precNew, recNew))
	t.Logf("coalesce (dup)    |    %5.1f%% |   %5.1f%% | %.3f", precCoal*100, recCoal*100, f1(precCoal, recCoal))
	t.Logf("contradict        |    %5.1f%% |   %5.1f%% | %.3f", precContra*100, recContra*100, f1(precContra, recContra))
	t.Logf("")
	t.Logf("overall accuracy: %.1f%%  (%d/%d correct)", accuracy*100, correct, sb.n)
	t.Logf("")
	t.Logf("false-duplicate rate (distinct→dup/coalesce): %.1f%% (%d/%d)",
		falseDupRate*100, sb.conf[0][colCoal]+sb.conf[0][colContra], rowSum[0])
	t.Logf("false-novelty rate  (dup/contra→new):          %.1f%% (%d/%d)",
		falseNoveltyRate*100, sb.conf[1][colNew]+sb.conf[2][colNew], rowSum[1]+rowSum[2])
	t.Logf("")

	t.Logf("confusion matrix (predicted → expected):")
	t.Logf("               pred new | pred coalesce | pred contrad")
	t.Logf("exp new         %-7d |    %-9d |    %-9d", sb.conf[0][0], sb.conf[0][1], sb.conf[0][2])
	t.Logf("exp coalesce    %-7d |    %-9d |    %-9d", sb.conf[1][0], sb.conf[1][1], sb.conf[1][2])
	t.Logf("exp contrad    %-7d |    %-9d |    %-9d", sb.conf[2][0], sb.conf[2][1], sb.conf[2][2])
}

// TestWriteClassifyOffline runs every dedup-triple and contradiction-quad pair
// through the write pipeline with a fake embedder and no LLM consolidator. This
// measures the non-LLM write-path classification: exact-restatement fingerprint,
// write-dedup hint/coalesce/supersede, and contradiction routing.
//
// Run offline (no external services needed):
//
//	go test -tags bench -run TestWriteClassifyOffline ./bench/
func TestWriteClassifyOffline(t *testing.T) {
	ctx := context.Background()
	const dims = 128

	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "wc_offline.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Build a service mirroring the shipped write-path wiring — the same set
	// newMeminiBackend applies for IngestWrite.
	e := embedtest.New(dims)
	now := time.Unix(1_700_000_000, 0).UTC()
	svc := service.New(st, e,
		service.WithClock(func() time.Time { return now }),
		service.WithSyncReinforce(),
		service.WithWriteDedup(0.625, service.WriteDedupHint),
		service.WithContradictionDownrank(0.625),
		service.WithCorroboration(0.70),
	)

	const ns = "wc-offline"
	var sb classifyScoreboard

	// Run pairs from dedup triples.
	for _, pair := range buildPairsFromTriples() {
		out, _, err := runOnePair(ctx, svc, ns, pair)
		if err != nil {
			t.Fatalf("run pair: %v", err)
		}
		sb.record(pair, out)
	}
	svc.WaitBackground()

	// Run pairs from contradiction quads (in a separate namespace so they don't
	// interfere with the dedup triples).
	const nsContra = "wc-offline-contra"
	for _, pair := range buildPairsFromQuads() {
		out, _, err := runOnePair(ctx, svc, nsContra, pair)
		if err != nil {
			t.Fatalf("run pair: %v", err)
		}
		sb.record(pair, out)
	}
	svc.WaitBackground()

	sb.report(t)
}

// TestWriteClassifyOfflineAutoDedup is like TestWriteClassifyOffline but with
// write-dedup action set to supersede (auto-tombstone not just hint). This
// measures how many contradictions are detected automatically by the non-LLM
// path (write-dedup + contradiction routing).
func TestWriteClassifyOfflineAutoDedup(t *testing.T) {
	ctx := context.Background()
	const dims = 128

	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "wc_auto.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	e := embedtest.New(dims)
	now := time.Unix(1_700_000_000, 0).UTC()
	// Auto-supersede mode: at >= 0.625 similarity, tombstone the old memory.
	svc := service.New(st, e,
		service.WithClock(func() time.Time { return now }),
		service.WithSyncReinforce(),
		service.WithWriteDedup(0.625, service.WriteDedupSupersede),
		service.WithContradictionDownrank(0.625),
		service.WithCorroboration(0.70),
	)

	const ns = "wc-auto"
	var sb classifyScoreboard

	for _, pair := range buildPairsFromTriples() {
		out, _, err := runOnePair(ctx, svc, ns, pair)
		if err != nil {
			t.Fatalf("run pair: %v", err)
		}
		sb.record(pair, out)
	}
	svc.WaitBackground()

	const nsContra = "wc-auto-contra"
	for _, pair := range buildPairsFromQuads() {
		out, _, err := runOnePair(ctx, svc, nsContra, pair)
		if err != nil {
			t.Fatalf("run pair: %v", err)
		}
		sb.record(pair, out)
	}
	svc.WaitBackground()

	sb.report(t)
}

// TestWriteClassifyLive runs every pair through the write pipeline with a live
// embedder and, when available, LLM consolidation. This measures the full
// production write-path classification.
//
// Configuration via env vars:
//
//	MEMINI_EMBED_BASE_URL  (default http://127.0.0.1:8001/v1)
//	MEMINI_EMBED_MODEL      (default text-embedding-qwen3-embedding-0.6b)
//	MEMINI_EMBED_DIMS       (default 1024)
//	MEMINI_LLM_BASE_URL     (optional, enables LLM consolidation)
//
// Run live:
//
//	go test -tags bench -run TestWriteClassifyLive ./bench/
func TestWriteClassifyLive(t *testing.T) {
	ctx := context.Background()

	baseURL := envOr("MEMINI_EMBED_BASE_URL", "http://127.0.0.1:8001/v1")
	model := envOr("MEMINI_EMBED_MODEL", "text-embedding-qwen3-embedding-0.6b")
	dims := envIntOr("MEMINI_EMBED_DIMS", 1024)

	e, err := embed.NewOpenAI(embed.OpenAIConfig{BaseURL: baseURL, Model: model, Dims: dims, HTTPClient: httpClientWithHeaders()})
	if err != nil {
		t.Skipf("embedder config: %v", err)
	}
	probe, err := e.Embed(ctx, []string{"connectivity probe"})
	if err != nil {
		t.Skipf("live embedder unreachable at %s (%s): %v", baseURL, model, err)
	}
	if len(probe) != 1 || len(probe[0]) != dims {
		t.Skipf("embedder returned %d-dim vectors, configured for %d — set MEMINI_EMBED_DIMS", len(probe[0]), dims)
	}

	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "wc_live.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Unix(1_700_000_000, 0).UTC()
	opts := []service.Option{
		service.WithClock(func() time.Time { return now }),
		service.WithSyncReinforce(),
		service.WithWriteDedup(0.625, service.WriteDedupHint),
		service.WithContradictionDownrank(0.625),
		service.WithCorroboration(0.70),
	}

	// Wire LLM consolidation when the env var is set.
	llmURL := os.Getenv("MEMINI_LLM_BASE_URL")
	if llmURL != "" {
		llmModel := envOr("MEMINI_LLM_MODEL", "qwen3-coder-30b-a3b-instruct")
		client, cerr := llm.NewOpenAI(llm.Config{BaseURL: llmURL, Model: llmModel, HTTPClient: httpClientWithHeaders()})
		if cerr != nil {
			t.Fatalf("llm config: %v", cerr)
		}
		opts = append(opts, service.WithConsolidator(client))
		// Sync mode: each write blocks until the LLM decision is applied,
		// so the test sees the consolidation result before recording.
		opts = append(opts, service.WithConsolidateMode(service.ConsolidateSync))
		t.Logf("LLM consolidation enabled (sync): %s (%s)", llmURL, llmModel)
	} else {
		t.Logf("LLM consolidation disabled (set MEMINI_LLM_BASE_URL to enable)")
	}

	svc := service.New(st, e, opts...)

	const ns = "wc-live"
	var sb classifyScoreboard

	for _, pair := range buildPairsFromTriples() {
		out, _, err := runOnePair(ctx, svc, ns, pair)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pair error (skipping): %v\n", err)
			continue
		}
		sb.record(pair, out)
	}
	svc.WaitBackground()
	// Async consolidation needs a flush.
	if llmURL != "" {
		if err := svc.FlushConsolidation(ctx); err != nil {
			t.Logf("flush consolidation: %v", err)
		}
	}

	const nsContra = "wc-live-contra"
	for _, pair := range buildPairsFromQuads() {
		out, _, err := runOnePair(ctx, svc, nsContra, pair)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pair error (skipping): %v\n", err)
			continue
		}
		sb.record(pair, out)
	}
	svc.WaitBackground()
	if llmURL != "" {
		if err := svc.FlushConsolidation(ctx); err != nil {
			t.Logf("flush consolidation: %v", err)
		}
	}

	sb.report(t)
}
