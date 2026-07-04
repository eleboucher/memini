//go:build bench

package bench_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/eleboucher/memini/bench"
	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/extract"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestEntityBridgeDiagnostic measures — before any recall wiring — whether the
// no-LLM entity extractor (extract.Entities) provides the associative edge
// multi-hop needs on LoCoMo cat-1:
//
//  1. coverage & fan-out: share of memories with ≥1 entity, and the entity
//     document-frequency distribution (an entity in half a conversation's
//     memories is a hub, not a bridge — expansion through it is spray);
//  2. bridge ceiling: for questions a single recall does not fully cover, is
//     every missing gold entity-linked (under a DF cap) to a hit the recall
//     did surface in its top-k window?
//  3. pool position: is the missing gold already inside the deep fused pool
//     (a deep recall at the pool depth), i.e. reachable by promotion, or
//     entirely absent (needs a store-side entity lookup)?
//
// Needs the live embedder; skips otherwise. MEMINI_MULTIHOP_LIMIT caps the
// sampled questions like the ceiling diagnostic.
func TestEntityBridgeDiagnostic(t *testing.T) {
	ctx := context.Background()
	baseURL := envOr("MEMINI_EMBED_BASE_URL", "http://127.0.0.1:8001/v1")
	model := envOr("MEMINI_EMBED_MODEL", "text-embedding-qwen3-embedding-0.6b")
	dims := envIntOr("MEMINI_EMBED_DIMS", 1024)
	data := envOr("MEMINI_LOCOMO_DATA", "data/locomo10.json")

	e, err := embed.NewOpenAI(embed.OpenAIConfig{BaseURL: baseURL, Model: model, Dims: dims})
	if err != nil {
		t.Skipf("embedder config: %v", err)
	}
	if _, err := e.Embed(ctx, []string{"probe"}); err != nil {
		t.Skipf("live embedder unreachable at %s (%s): %v", baseURL, model, err)
	}

	ds, err := bench.LoadLoCoMo(data)
	if err != nil {
		t.Fatalf("load locomo: %v", err)
	}
	var qs []bench.Question
	for _, q := range ds.Questions {
		if q.Category == "1" && len(q.Gold) >= 2 {
			qs = append(qs, q)
		}
	}
	if limit := envIntOr("MEMINI_MULTIHOP_LIMIT", 0); limit > 0 && limit < len(qs) {
		step := float64(len(qs)) / float64(limit)
		sampled := make([]bench.Question, 0, limit)
		for i := range limit {
			sampled = append(sampled, qs[int(float64(i)*step)])
		}
		qs = sampled
	}
	if len(qs) == 0 {
		t.Skip("no multi-hop cat-1 questions in dataset")
	}

	groups := map[string]bool{}
	for _, q := range qs {
		groups[q.Group] = true
	}
	var items []bench.Item
	for _, it := range ds.Items {
		if groups[it.Group] {
			items = append(items, it)
		}
	}

	// --- 1. extraction coverage & entity fan-out, per conversation ---
	// Two candidate edge vocabularies, measured side by side: the shipped
	// capitalized-span extractor, and a throwaway lowercase concept-chunk
	// extractor (stemmed content words) approximating the "noun phrase" tier.
	modes := []struct {
		name    string
		extract func(string) []string
	}{
		{"entities", extract.Entities},
		{"concepts", conceptChunks},
	}
	groupSize := map[string]int{}
	for _, it := range items {
		groupSize[it.Group]++
	}
	type vocab struct {
		of map[string][]string       // item id -> terms
		df map[string]map[string]int // group -> term -> doc count
	}
	vocabs := make([]vocab, len(modes))
	for mi, mode := range modes {
		v := vocab{of: make(map[string][]string, len(items)), df: map[string]map[string]int{}}
		withTerm := 0
		for _, it := range items {
			terms := mode.extract(it.Content)
			v.of[it.ID] = terms
			if len(terms) > 0 {
				withTerm++
			}
			if v.df[it.Group] == nil {
				v.df[it.Group] = map[string]int{}
			}
			for _, en := range terms {
				v.df[it.Group][en]++
			}
		}
		vocabs[mi] = v
		t.Logf("[%s] items=%d with≥1 term=%d (%.1f%%)", mode.name, len(items), withTerm, pct(withTerm, float64(len(items))))
		for _, g := range sortedKeys(groupSize) {
			type ec struct {
				e string
				n int
			}
			var top []ec
			for en, n := range v.df[g] {
				top = append(top, ec{en, n})
			}
			sort.Slice(top, func(i, j int) bool { return top[i].n > top[j].n })
			var line strings.Builder
			for i, x := range top {
				if i >= 8 {
					break
				}
				fmt.Fprintf(&line, " %s=%d", x.e, x.n)
			}
			t.Logf("[%s] group %s: %d items, %d terms; top DF:%s", mode.name, g, groupSize[g], len(v.df[g]), line.String())
		}
	}

	// --- 2+3. bridge ceiling and pool position on single-recall failures ---
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "entitydiag.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := ingestItems(ctx, st, e, items); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	svc := service.New(st, e)

	const k = 10
	deepK := service.RecallPoolSize(k) // the fused pool depth production recall works from
	dfCaps := []float64{1.0, 0.10, 0.05}

	linked := func(v vocab, missing string, anchors []string, group string, dfCap float64) bool {
		limit := int(dfCap * float64(groupSize[group]))
		mset := map[string]bool{}
		for _, en := range v.of[missing] {
			if v.df[group][en] <= limit {
				mset[en] = true
			}
		}
		for _, a := range anchors {
			for _, en := range v.of[a] {
				if mset[en] {
					return true
				}
			}
		}
		return false
	}

	type tally struct {
		missLinked, missLinkedInPool, bridgeable, bridgeableInPool int
		// mechanism simulation: reserve-style promotion of linked pool
		// candidates into the window, two-leg gate (0.5×evictee, 0.4×top).
		simFull, simPromoted, simPromotedGold int
	}
	tallies := make([][]tally, len(modes)) // [mode][dfCap]
	for i := range tallies {
		tallies[i] = make([]tally, len(dfCaps))
	}
	goldSets := make([]map[string]bool, len(qs))
	for qi, q := range qs {
		goldSets[qi] = map[string]bool{}
		for _, g := range q.Gold {
			goldSets[qi][g] = true
		}
	}
	var failures, missTotal, missInPool int
	for qi, q := range qs {
		res, err := svc.Recall(ctx, service.RecallInput{Namespace: q.Group, Query: q.Query, Limit: deepK})
		if err != nil {
			t.Fatalf("recall: %v", err)
		}
		deep := make([]string, len(res))
		for i, r := range res {
			deep[i] = r.Memory.ID
		}
		got := deep
		if len(got) > k {
			got = got[:k]
		}

		// Mechanism simulation on every question (success or failure): the
		// promotion machinery runs unconditionally in production, so noise
		// promotions on already-covered questions count too. Promotion mirrors
		// reserveDurableTiers: evict from the window bottom, two-leg gate,
		// monotone break once the pool score falls below the bar.
		for mi := range modes {
			for ci, dfCap := range dfCaps {
				anchors := got
				if len(anchors) > 3 {
					anchors = anchors[:3]
				}
				const maxPromote = 3
				var promoted []string
				for i := k; i < len(res) && len(promoted) < maxPromote; i++ {
					evict := len(got) - 1 - len(promoted)
					if evict <= 0 {
						break
					}
					bar := max(0.5*res[evict].Score, 0.4*res[0].Score)
					if res[i].Score < bar {
						break // pool scores only fall from here; the bar never does
					}
					if !linked(vocabs[mi], deep[i], anchors, q.Group, dfCap) {
						continue
					}
					promoted = append(promoted, deep[i])
					ta := &tallies[mi][ci]
					ta.simPromoted++
					if goldSets[qi][deep[i]] {
						ta.simPromotedGold++
					}
				}
				inWin := map[string]bool{}
				for _, id := range got[:len(got)-len(promoted)] {
					inWin[id] = true
				}
				for _, id := range promoted {
					inWin[id] = true
				}
				full := true
				for g := range goldSets[qi] {
					if !inWin[g] {
						full = false
						break
					}
				}
				if full {
					tallies[mi][ci].simFull++
				}
			}
		}
		found, total := coverage(got, q.Gold)
		if found == total {
			continue
		}
		failures++
		gotSet := map[string]bool{}
		for _, id := range got {
			gotSet[id] = true
		}
		deepSet := map[string]bool{}
		for _, id := range deep {
			deepSet[id] = true
		}
		anchors := got
		if len(anchors) > 3 {
			anchors = anchors[:3]
		}
		type flag struct{ all, allInPool bool }
		flags := make([][]flag, len(modes))
		for mi := range flags {
			flags[mi] = make([]flag, len(dfCaps))
			for ci := range flags[mi] {
				flags[mi][ci] = flag{true, true}
			}
		}
		for _, g := range q.Gold {
			if gotSet[g] {
				continue
			}
			missTotal++
			inPool := deepSet[g]
			if inPool {
				missInPool++
			}
			for mi := range modes {
				for ci, dfCap := range dfCaps {
					if linked(vocabs[mi], g, anchors, q.Group, dfCap) {
						tallies[mi][ci].missLinked++
						if inPool {
							tallies[mi][ci].missLinkedInPool++
						} else {
							flags[mi][ci].allInPool = false
						}
					} else {
						flags[mi][ci].all = false
						flags[mi][ci].allInPool = false
					}
				}
			}
		}
		for mi := range modes {
			for ci := range dfCaps {
				if flags[mi][ci].all {
					tallies[mi][ci].bridgeable++
				}
				if flags[mi][ci].allInPool {
					tallies[mi][ci].bridgeableInPool++
				}
			}
		}
	}

	t.Logf("questions=%d single-recall failures=%d missing golds=%d (in deep pool top-%d: %d = %.1f%%)",
		len(qs), failures, missTotal, deepK, missInPool, pct(missInPool, float64(missTotal)))
	for mi, mode := range modes {
		for ci, dfCap := range dfCaps {
			ta := tallies[mi][ci]
			t.Logf("[%s] df cap %.2f: linked misses %d/%d (%.1f%%), of which in-pool %d; fully-bridgeable failures %d/%d (%.1f%%), in-pool-only %d (%.1f%%)",
				mode.name, dfCap, ta.missLinked, missTotal, pct(ta.missLinked, float64(missTotal)), ta.missLinkedInPool,
				ta.bridgeable, failures, pct(ta.bridgeable, float64(failures)),
				ta.bridgeableInPool, pct(ta.bridgeableInPool, float64(failures)))
		}
	}
	t.Logf("mechanism simulation (promote ≤3 linked pool candidates, two-leg gate, single k=%d recall):", k)
	for mi, mode := range modes {
		for ci, dfCap := range dfCaps {
			ta := tallies[mi][ci]
			noise := ta.simPromoted - ta.simPromotedGold
			t.Logf("[%s] df cap %.2f: full-gold coverage %d/%d (%.1f%%); promotions %d (gold %d, noise %d)",
				mode.name, dfCap, ta.simFull, len(qs), pct(ta.simFull, float64(len(qs))),
				ta.simPromoted, ta.simPromotedGold, noise)
		}
	}

	// --- 4. entity-anchored 2nd hop: the two-recall union mechanism the
	// ceiling diagnostic's regimes use, but augmenting the query with the
	// top hits' DF-capped terms instead of the top-1 memory's full content.
	// Directly comparable to multihop_test.go's realistic/oracle rows.
	hop := []struct {
		mode  int
		dfCap float64
	}{{0, 1.0}, {0, 0.10}, {1, 1.0}, {1, 0.10}}
	for _, h := range hop {
		v := vocabs[h.mode]
		var full int
		var meanRecall float64
		for _, q := range qs {
			got := recallSet(ctx, t, svc, q.Group, q.Query, k)
			found, total := coverage(got, q.Gold)
			if found == total {
				full++
				meanRecall++
				continue
			}
			limit := int(h.dfCap * float64(groupSize[q.Group]))
			terms := map[string]bool{}
			for i, id := range got {
				if i >= 3 {
					break
				}
				for _, en := range v.of[id] {
					if v.df[q.Group][en] <= limit {
						terms[en] = true
					}
				}
			}
			union2 := got
			if len(terms) > 0 {
				aug := q.Query + " " + strings.Join(sortedSet(terms), " ")
				union2 = union(got, recallSet(ctx, t, svc, q.Group, aug, k))
			}
			f2, t2 := coverage(union2, q.Gold)
			meanRecall += float64(f2) / float64(t2)
			if f2 == t2 {
				full++
			}
		}
		t.Logf("[%s] entity-anchored 2nd hop, df cap %.2f: full-gold %d/%d (%.1f%%), mean gold recall %.1f%%",
			modes[h.mode].name, h.dfCap, full, len(qs), pct(full, float64(len(qs))), meanRecall/float64(len(qs))*100)
	}
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// conceptChunks is the diagnostic-only lowercase concept extractor: stemmed
// content words (and adjacent pairs) with stopwords removed — a stand-in for
// the "noun phrase" tier, to price what lowercase concepts would buy as edges.
func conceptChunks(text string) []string {
	if i := strings.Index(text, "] "); i > 0 && strings.HasPrefix(text, "[") {
		text = text[i+2:] // drop the bench's date prefix
	}
	if i := strings.Index(text, ": "); i > 0 && i < 20 {
		text = text[i+2:] // drop the speaker label
	}
	var words []string
	for w := range strings.FieldsSeq(strings.ToLower(text)) {
		w = strings.TrimFunc(w, func(r rune) bool { return r < 'a' || r > 'z' })
		if len(w) < 4 || conceptStop[w] {
			continue
		}
		words = append(words, stem(w))
	}
	seen := map[string]bool{}
	var out []string
	for i, w := range words {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
		if i+1 < len(words) {
			if pair := w + " " + words[i+1]; !seen[pair] {
				seen[pair] = true
				out = append(out, pair)
			}
		}
	}
	return out
}

// stem is a crude suffix-stripper (ing/ed/es/s), enough to join
// "camped"/"camping"/"camp" for the diagnostic.
func stem(w string) string {
	for _, suf := range []string{"ing", "ed", "es", "s"} {
		if strings.HasSuffix(w, suf) && len(w)-len(suf) >= 4 {
			return w[:len(w)-len(suf)]
		}
	}
	return w
}

var conceptStop = func() map[string]bool {
	words := []string{
		"that", "this", "these", "those", "with", "without", "about", "have",
		"been", "being", "will", "would", "should", "could", "cant", "dont",
		"your", "yours", "their", "them", "they", "what", "when", "where",
		"which", "while", "since", "because", "though", "really", "very",
		"just", "still", "even", "only", "some", "more", "most", "much",
		"many", "other", "another", "such", "same", "here", "there", "then",
		"than", "into", "onto", "over", "under", "after", "before", "during",
		"also", "again", "once", "like", "love", "great", "good", "nice",
		"awesome", "amazing", "glad", "happy", "thanks", "thank", "sounds",
		"know", "think", "want", "need", "going", "gonna", "make", "made",
		"take", "took", "come", "came", "went", "goes", "well", "been",
		"were", "hows", "whats", "thats", "youre", "theyre", "hope", "feel",
		"felt", "keep", "getting", "give", "gave", "look", "looking", "time",
		"things", "thing", "stuff", "back", "everyone", "someone", "something",
		"anything", "everything", "little", "lots", "does", "doing", "done",
	}
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}()

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
