package main

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/importer"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// sortLinks returns links sorted by (Src, Dst) for order-independent comparison.
func sortLinks(links []store.NamespaceLink) []store.NamespaceLink {
	out := make([]store.NamespaceLink, len(links))
	copy(out, links)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Src != out[j].Src {
			return out[i].Src < out[j].Src
		}
		return out[i].Dst < out[j].Dst
	})
	return out
}

// TestLinkExportImportRoundTrip seeds links across several namespaces,
// exports them (gatherExport), wipes by importing into a fresh store, and
// asserts ListAllLinks comes back identical — the gap G6 round trip.
func TestLinkExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := openTestStore(t)
	srcLS, err := linkStoreOf(src)
	if err != nil {
		t.Fatalf("linkStoreOf: %v", err)
	}

	seeded := []store.NamespaceLink{
		{Src: "acme/phoenix/api", Dst: "shared/golang", Tiers: []memory.Tier{memory.TierSemantic}, Note: "shared team docs", CreatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
		{Src: "acme/phoenix", Dst: "acme", Note: "", CreatedAt: time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)},
		{Src: "personal/kit", Dst: "shared/golang", Tiers: []memory.Tier{memory.TierSemantic, memory.TierProcedural}, CreatedAt: time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)},
	}
	for _, l := range seeded {
		if err := srcLS.PutLink(ctx, l); err != nil {
			t.Fatalf("seed link: %v", err)
		}
	}

	namespaces := []string{"acme/phoenix/api", "acme/phoenix", "acme", "shared/golang", "personal/kit"}
	ef, err := gatherExport(ctx, src, namespaces, false, store.Filter{})
	if err != nil {
		t.Fatalf("gatherExport: %v", err)
	}
	if len(ef.Links) != len(seeded) {
		t.Fatalf("want %d exported links, got %d: %+v", len(seeded), len(ef.Links), ef.Links)
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(ef); err != nil {
		t.Fatalf("encode: %v", err)
	}

	// "Wipe" by importing into a fresh store (simulates backup/restore).
	target := openTestStore(t)
	targetLS, err := linkStoreOf(target)
	if err != nil {
		t.Fatalf("linkStoreOf(target): %v", err)
	}

	links, err := loadLinks(importer.SourceMemini, buf.Bytes())
	if err != nil {
		t.Fatalf("loadLinks: %v", err)
	}
	if n, err := restoreLinks(ctx, targetLS, links); err != nil {
		t.Fatalf("restoreLinks: %v", err)
	} else if n != len(seeded) {
		t.Fatalf("restored %d links, want %d", n, len(seeded))
	}

	got, err := targetLS.ListAllLinks(ctx)
	if err != nil {
		t.Fatalf("ListAllLinks: %v", err)
	}
	gotSorted, wantSorted := sortLinks(got), sortLinks(seeded)
	if len(gotSorted) != len(wantSorted) {
		t.Fatalf("got %d links, want %d", len(gotSorted), len(wantSorted))
	}
	for i := range wantSorted {
		g, w := gotSorted[i], wantSorted[i]
		if g.Src != w.Src || g.Dst != w.Dst || g.Note != w.Note {
			t.Errorf("link %d: got %+v, want %+v", i, g, w)
		}
		if len(g.Tiers) != len(w.Tiers) {
			t.Errorf("link %d tiers: got %v, want %v", i, g.Tiers, w.Tiers)
		}
		for j := range w.Tiers {
			if j >= len(g.Tiers) || g.Tiers[j] != w.Tiers[j] {
				t.Errorf("link %d tiers: got %v, want %v", i, g.Tiers, w.Tiers)
				break
			}
		}
		if !g.CreatedAt.Equal(w.CreatedAt) {
			t.Errorf("link %d created_at: got %v, want %v", i, g.CreatedAt, w.CreatedAt)
		}
	}

	// Re-importing (idempotent upsert) must not duplicate rows.
	if _, err := restoreLinks(ctx, targetLS, links); err != nil {
		t.Fatalf("re-restoreLinks: %v", err)
	}
	again, err := targetLS.ListAllLinks(ctx)
	if err != nil {
		t.Fatalf("ListAllLinks after re-import: %v", err)
	}
	if len(again) != len(seeded) {
		t.Fatalf("re-import duplicated links: got %d, want %d", len(again), len(seeded))
	}
}

// TestGatherExportFiltersLinksByNamespaceSet pins that a link is included
// only when its src OR dst is in the exported namespace set.
func TestGatherExportFiltersLinksByNamespaceSet(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	ls, err := linkStoreOf(st)
	if err != nil {
		t.Fatalf("linkStoreOf: %v", err)
	}
	links := []store.NamespaceLink{
		{Src: "acme/phoenix", Dst: "shared/golang"}, // included: src in set
		{Src: "shared/golang", Dst: "acme/phoenix"}, // included: dst in set
		{Src: "other/one", Dst: "other/two"},        // excluded: neither in set
	}
	for _, l := range links {
		if err := ls.PutLink(ctx, l); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	ef, err := gatherExport(ctx, st, []string{"acme/phoenix"}, false, store.Filter{})
	if err != nil {
		t.Fatalf("gatherExport: %v", err)
	}
	if len(ef.Links) != 2 {
		t.Fatalf("want 2 links (src or dst match), got %d: %+v", len(ef.Links), ef.Links)
	}
}

// TestGatherExportAllNamespacesIncludesEveryLink pins the --all-namespaces
// case: every link is exported, even one whose src/dst namespace holds no
// memories at all (ListNamespaces only returns namespaces with a memory row,
// but a link is allowed to point at a namespace that has none — namespaces
// exist implicitly).
func TestGatherExportAllNamespacesIncludesEveryLink(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	ls, err := linkStoreOf(st)
	if err != nil {
		t.Fatalf("linkStoreOf: %v", err)
	}
	// Neither namespace below has ever held a memory, so ListNamespaces
	// returns none of them — the memory-export namespace set would be empty.
	if err := ls.PutLink(ctx, store.NamespaceLink{Src: "acme/phoenix", Dst: "acme/not-yet-created"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	names, err := st.ListNamespaces(ctx)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("expected no memory-bearing namespaces yet, got %v", names)
	}

	ef, err := gatherExport(ctx, st, names, true, store.Filter{})
	if err != nil {
		t.Fatalf("gatherExport: %v", err)
	}
	if len(ef.Links) != 1 {
		t.Fatalf("--all-namespaces must export every link regardless of the memory namespace set, got %+v", ef.Links)
	}
}

// TestLoadLinksOldFormatWithoutLinksKey pins that importing an export
// document from before links existed (no top-level "links" key) keeps
// working: loadLinks returns nil, nil rather than an error.
func TestLoadLinksOldFormatWithoutLinksKey(t *testing.T) {
	old := []byte(`{"memories":[{"id":"m1","namespace":"acme","tier":"semantic","content":"x","importance":0.5}]}`)
	links, err := loadLinks(importer.SourceMemini, old)
	if err != nil {
		t.Fatalf("loadLinks on old-format export: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("want no links from an old-format export, got %+v", links)
	}

	// A bare array (the other historical native shape) must not error either.
	bareArray := []byte(`[{"id":"m1","namespace":"acme","tier":"semantic","content":"x","importance":0.5}]`)
	if links, err := loadLinks(importer.SourceMemini, bareArray); err != nil || len(links) != 0 {
		t.Fatalf("loadLinks on bare-array export: links=%v err=%v", links, err)
	}
}

// TestLoadLinksNonMeminiSourceReturnsNil pins that only the native memini
// source carries links; other importer sources never attempt to parse them.
func TestLoadLinksNonMeminiSourceReturnsNil(t *testing.T) {
	links, err := loadLinks(importer.SourceMem0, []byte(`{"links":[{"src":"a","dst":"b"}]}`))
	if err != nil {
		t.Fatalf("loadLinks: %v", err)
	}
	if links != nil {
		t.Fatalf("non-memini source should never yield links, got %+v", links)
	}
}

// TestLoadLinksRejectsInvalidTier pins that a corrupted/hand-edited export
// with an unknown tier name fails loudly rather than silently restoring a
// link with a bogus tier.
func TestLoadLinksRejectsInvalidTier(t *testing.T) {
	bad := []byte(`{"links":[{"src":"a","dst":"b","tiers":["bogus"]}]}`)
	if _, err := loadLinks(importer.SourceMemini, bad); err == nil {
		t.Fatal("expected an error for an invalid tier in an imported link")
	}
}
