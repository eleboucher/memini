package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// noLinkStore embeds a nil store.Store so it satisfies the interface (any
// method call other than the type assertion under test would panic) while
// deliberately not implementing store.LinkStore — a backend that predates
// namespace links.
type noLinkStore struct {
	store.Store
}

func TestLinkStoreOfDegradesGracefully(t *testing.T) {
	if _, err := linkStoreOf(noLinkStore{}); err == nil {
		t.Fatal("expected an error for a backend without LinkStore support")
	} else if !strings.Contains(err.Error(), "namespace links") {
		t.Errorf("error should mention namespace links, got: %v", err)
	}
}

func TestLinkStoreOfSupportedBackend(t *testing.T) {
	st := openTestStore(t)
	if _, err := linkStoreOf(st); err != nil {
		t.Fatalf("sqlitevec should support LinkStore: %v", err)
	}
}

func TestNormalizeLinkNamespace(t *testing.T) {
	got, err := normalizeLinkNamespace("//acme/phoenix/")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got != "acme/phoenix" {
		t.Errorf("got %q, want %q", got, "acme/phoenix")
	}
	if _, err := normalizeLinkNamespace(""); err == nil {
		t.Fatal("expected error for empty namespace")
	}
}

func TestParseLinkTiers(t *testing.T) {
	tiers, err := parseLinkTiers("")
	if err != nil || tiers != nil {
		t.Fatalf("empty input should yield nil, got %v, %v", tiers, err)
	}
	tiers, err = parseLinkTiers("semantic, procedural")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tiers) != 2 || tiers[0] != memory.TierSemantic || tiers[1] != memory.TierProcedural {
		t.Fatalf("unexpected tiers: %v", tiers)
	}
	if _, err := parseLinkTiers("bogus"); err == nil {
		t.Fatal("expected error for invalid tier")
	}
}

// TestAddLinkRejectsSelfLink mirrors the REST handler's self-link rejection
// (rest.go PutLink).
func TestAddLinkRejectsSelfLink(t *testing.T) {
	st := openTestStore(t)
	ls, err := linkStoreOf(st)
	if err != nil {
		t.Fatalf("linkStoreOf: %v", err)
	}
	if _, err := addLink(context.Background(), ls, "acme/phoenix", "acme/phoenix", "", ""); err == nil {
		t.Fatal("expected self-link to be rejected")
	}
}

func TestAddLinkRejectsWildcardDst(t *testing.T) {
	st := openTestStore(t)
	ls, err := linkStoreOf(st)
	if err != nil {
		t.Fatalf("linkStoreOf: %v", err)
	}
	if _, err := addLink(context.Background(), ls, "acme/phoenix", "acme/*", "", ""); err == nil {
		t.Fatal("expected wildcard dst to be rejected")
	}
}

func TestAddLinkRejectsInvalidTier(t *testing.T) {
	st := openTestStore(t)
	ls, err := linkStoreOf(st)
	if err != nil {
		t.Fatalf("linkStoreOf: %v", err)
	}
	if _, err := addLink(context.Background(), ls, "acme/phoenix", "shared/golang", "bogus", ""); err == nil {
		t.Fatal("expected invalid tier to be rejected")
	}
}

// TestAddLinkUpsertIdempotent pins that re-adding the same src/dst replaces
// the tiers/note in place rather than duplicating, matching PutLink's upsert
// semantics (rest.go).
func TestAddLinkUpsertIdempotent(t *testing.T) {
	st := openTestStore(t)
	ls, err := linkStoreOf(st)
	if err != nil {
		t.Fatalf("linkStoreOf: %v", err)
	}
	ctx := context.Background()

	if _, err := addLink(ctx, ls, "acme/phoenix", "shared/golang", "semantic", "first note"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := addLink(ctx, ls, "acme/phoenix", "shared/golang", "procedural", "second note"); err != nil {
		t.Fatalf("re-add: %v", err)
	}

	links, err := ls.ListLinks(ctx, "acme/phoenix")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("want exactly 1 link (upsert, not duplicate), got %d: %+v", len(links), links)
	}
	if links[0].Note != "second note" || len(links[0].Tiers) != 1 || links[0].Tiers[0] != memory.TierProcedural {
		t.Fatalf("re-add should replace tiers/note in place, got %+v", links[0])
	}
}

// TestAddLinkAllowsMissingDestinationNamespace pins the by-design behavior
// that a link may point at a namespace holding no memories yet (namespaces
// exist implicitly), and that the row is actually persisted.
func TestAddLinkAllowsMissingDestinationNamespace(t *testing.T) {
	st := openTestStore(t)
	ls, err := linkStoreOf(st)
	if err != nil {
		t.Fatalf("linkStoreOf: %v", err)
	}
	ctx := context.Background()
	if _, err := addLink(ctx, ls, "acme/phoenix", "acme/not-yet-created", "", ""); err != nil {
		t.Fatalf("linking to a not-yet-existing namespace should be allowed: %v", err)
	}
	links, err := ls.ListLinks(ctx, "acme/phoenix")
	if err != nil {
		t.Fatalf("ListLinks: %v", err)
	}
	if len(links) != 1 || links[0].Src != "acme/phoenix" || links[0].Dst != "acme/not-yet-created" {
		t.Fatalf("stored link mismatch: %+v", links)
	}
}

func TestPrintLinksEmpty(t *testing.T) {
	var buf bytes.Buffer
	printLinks(&buf, nil)
	if got := buf.String(); !strings.Contains(got, "no links") {
		t.Errorf("expected an empty-state message, got %q", got)
	}
}

func TestPrintLinksTable(t *testing.T) {
	var buf bytes.Buffer
	printLinks(&buf, []store.NamespaceLink{
		{Src: "acme/phoenix", Dst: "shared/golang", Tiers: []memory.Tier{memory.TierSemantic}, Note: "shared docs"},
		{Src: "acme/phoenix", Dst: "acme/other"},
	})
	out := buf.String()
	for _, want := range []string{"acme/phoenix", "shared/golang", "semantic", "shared docs", "acme/other"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q, got:\n%s", want, out)
		}
	}
}
