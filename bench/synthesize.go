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

	// Cluster over every non-scratch tier, not just the durable ones: the
	// production write path classifies precision-first (extract.Classify rarely
	// promotes to semantic), so on this corpus the facts overwhelmingly land in
	// EPISODIC. Filtering to [semantic,procedural] left a ~5-item pool and formed
	// only 5 clusters — the null runs were measuring an empty synthesizer, not
	// the idea. Synthesized output is still written as semantic/deduced.
	pool := []memory.Tier{memory.TierEpisodic, memory.TierSemantic, memory.TierProcedural}
	mems, err := st.List(ctx, namespace, store.Filter{Tiers: pool, Now: now}, 100000)
	if err != nil {
		return stats, fmt.Errorf("synthesize: list memories: %w", err)
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
			store.Filter{Tiers: pool, Now: now}, synthClusterK+1)
		if err != nil {
			return stats, fmt.Errorf("synthesize: neighbor search: %w", err)
		}

		// Relative top-K clustering: take the seed's nearest unvisited durable
		// neighbours (best-first), NO absolute score floor. On this store vector
		// scores are 1/(1+L2) and are only meaningful as a within-query ranking
		// (recall itself min-max-normalizes them before fusing), so an absolute
		// floor is meaningless — a 0.55 then 0.40 cut both formed ~2-5 clusters
		// over 171 memories and left the idea untested. The LLM prompt is the
		// precision gate: an incoherent cluster costs one call returning [],
		// not a bad fact.
		cluster := []*memory.Memory{seed}
		for _, n := range neighbors {
			if n.Memory.ID == seed.ID || visited[n.Memory.ID] {
				continue
			}
			visited[n.Memory.ID] = true
			cluster = append(cluster, n.Memory)
			if len(cluster) >= synthClusterMax {
				break
			}
		}
		if len(cluster) < 2 {
			continue // seed had no unvisited neighbour left
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

// Cluster tuning: how many nearest neighbours to pull per seed and the cluster
// size cap. There is no absolute similarity floor — see the relative top-K
// rationale at the cluster loop.
const (
	synthClusterK   = 8
	synthClusterMax = 6
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
