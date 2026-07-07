//go:build bench

package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// Synthesize is the write-time synthesis spike (honcho "dreamer" inductive half,
// minus the tool loop). It clusters a namespace's durable memories by semantic
// proximity and, per cluster, makes ONE LLM call that may emit higher-level
// facts entailed by the cluster — an aggregate, a cross-cutting relation, a
// list, a pattern — that no single member states. Emitted facts are stored as
// new durable memories (Level=deduced, tagged "synthesized", source_ids in
// metadata) so plain single-shot recall can retrieve the combined fact directly.
//
// It is deliberately QUESTION-BLIND: it sees only the store, never the benchmark
// questions. That is the validity crux — production never knows future queries,
// so a lift here must come from unsupervised bottom-up synthesis, not leakage.
//
// It runs offline (after ingest, before answering), sequentially — a spike, not
// the production background worker. If it pays, the core promotes to a
// service.Synthesizer mirroring the consolidator's queue.
func Synthesize(
	ctx context.Context, st store.Store, e embed.Embedder, chat llm.Completer,
	namespace string, now time.Time,
) (SynthStats, error) {
	var stats SynthStats

	durable := []memory.Tier{memory.TierSemantic, memory.TierProcedural}
	mems, err := st.List(ctx, namespace, store.Filter{Tiers: durable, Now: now}, 100000)
	if err != nil {
		return stats, fmt.Errorf("synthesize: list durable: %w", err)
	}
	// Stable order so the run is deterministic given a fixed store.
	sort.Slice(mems, func(i, j int) bool { return mems[i].ID < mems[j].ID })

	visited := make(map[string]bool, len(mems))
	for _, seed := range mems {
		if visited[seed.ID] {
			continue
		}
		visited[seed.ID] = true

		vec, err := embed.EmbedOne(ctx, e, seed.Content)
		if err != nil {
			return stats, fmt.Errorf("synthesize: embed seed %s: %w", seed.ID, err)
		}
		neighbors, err := st.VectorSearch(ctx, namespace, vec,
			store.Filter{Tiers: durable, Now: now}, synthClusterK+1)
		if err != nil {
			return stats, fmt.Errorf("synthesize: neighbor search: %w", err)
		}

		cluster := []*memory.Memory{seed}
		for _, n := range neighbors {
			if n.Memory.ID == seed.ID || visited[n.Memory.ID] || n.Score < synthClusterFloor {
				continue
			}
			visited[n.Memory.ID] = true
			cluster = append(cluster, n.Memory)
			if len(cluster) >= synthClusterMax {
				break
			}
		}
		if len(cluster) < 2 {
			continue // nothing to combine
		}
		stats.Clusters++

		facts, in, out, err := synthesizeCluster(ctx, chat, cluster)
		stats.Calls++
		stats.InTokens += int64(in)
		stats.OutTokens += int64(out)
		if err != nil {
			return stats, err
		}
		for _, f := range facts {
			vec, err := embed.EmbedOne(ctx, e, f.content)
			if err != nil {
				return stats, fmt.Errorf("synthesize: embed fact: %w", err)
			}
			id := fmt.Sprintf("synth-%s", seed.ID)
			if stats.Facts > 0 {
				id = fmt.Sprintf("synth-%s-%d", seed.ID, stats.Facts)
			}
			if err := st.Upsert(ctx, &memory.Memory{
				ID: id, Namespace: namespace, Tier: memory.TierSemantic, Level: memory.LevelDeduced,
				Content: f.content, Tags: []string{"synthesized"},
				Metadata:  map[string]any{"synthesized": true, "source_ids": f.sourceIDs},
				CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, ValidFrom: &now,
				Embedding: vec,
			}); err != nil {
				return stats, fmt.Errorf("synthesize: upsert fact: %w", err)
			}
			stats.Facts++
		}
	}
	return stats, nil
}

// Cluster tuning: how many neighbours to consider, the cosine-fused similarity
// floor to admit one, and the cluster size cap. Deliberately conservative — a
// tight cluster keeps the synthesis grounded and the LLM input small.
const (
	synthClusterK     = 6
	synthClusterMax   = 6
	synthClusterFloor = 0.55
)

// synthFact is one validated synthesized fact and the 1-based cluster members
// it was combined from, resolved to memory ids.
type synthFact struct {
	content   string
	sourceIDs []string
}

const synthSystem = "You combine related memory facts into higher-level facts. Given a numbered list of " +
	"atomic facts, emit only facts that COMBINE two or more of them into something none states alone — an " +
	"aggregate or count, a cross-cutting relationship, a consolidated list, or a general pattern. Every " +
	"emitted fact must be FULLY entailed by the listed facts: invent nothing, add no outside knowledge, and " +
	"do not restate a single fact. If they do not meaningfully combine, return an empty array. Output JSON " +
	"only: {\"facts\":[{\"content\":\"one concise sentence\",\"sources\":[1,3]}]} where sources are the " +
	"1-based indices you combined (at least two)."

// synthesizeCluster makes one LLM call over the cluster and returns the facts
// that pass validation (>=2 in-range sources, non-empty content). Token counts
// use the same ~4 B/token estimate as the other bench cost metrics.
func synthesizeCluster(ctx context.Context, chat llm.Completer, cluster []*memory.Memory) ([]synthFact, int, int, error) {
	var b strings.Builder
	b.WriteString("Facts:\n")
	for i, m := range cluster {
		fmt.Fprintf(&b, "%d. %s\n", i+1, m.Content)
	}
	user := b.String()

	out, err := chat.Complete(ctx, synthSystem, user)
	if err != nil {
		return nil, estimateTokens(synthSystem) + estimateTokens(user), 0, fmt.Errorf("synthesize: complete: %w", err)
	}
	inTok := estimateTokens(synthSystem) + estimateTokens(user)
	outTok := estimateTokens(out)

	var parsed struct {
		Facts []struct {
			Content string `json:"content"`
			Sources []int  `json:"sources"`
		} `json:"facts"`
	}
	if err := json.Unmarshal([]byte(extractJSON(out)), &parsed); err != nil {
		// A malformed reply yields nothing rather than failing the run — the same
		// best-effort stance the consolidator takes on a bad LLM response.
		return nil, inTok, outTok, nil
	}

	var facts []synthFact
	for _, f := range parsed.Facts {
		content := strings.TrimSpace(f.Content)
		if content == "" || len(f.Sources) < 2 {
			continue
		}
		ids := make([]string, 0, len(f.Sources))
		ok := true
		for _, s := range f.Sources {
			if s < 1 || s > len(cluster) {
				ok = false
				break
			}
			ids = append(ids, cluster[s-1].ID)
		}
		if !ok {
			continue
		}
		facts = append(facts, synthFact{content: content, sourceIDs: ids})
	}
	return facts, inTok, outTok, nil
}

// extractJSON returns the substring from the first '{' to the last '}', so a
// model that wraps the object in prose or a ```json fence still parses.
func extractJSON(s string) string {
	i, j := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if i < 0 || j <= i {
		return s
	}
	return s[i : j+1]
}

// SynthStats is a snapshot of one Synthesize run's work: clusters that reached
// the LLM, LLM calls, facts stored, and estimated token cost.
type SynthStats struct {
	Clusters  int   `json:"clusters"`
	Calls     int   `json:"calls"`
	Facts     int   `json:"facts"`
	InTokens  int64 `json:"in_tokens_est"`
	OutTokens int64 `json:"out_tokens_est"`
}
