// Validates docs/examples/namespace-lifecycle.md.
//
// One test walks a namespace's whole life against an in-process sqlite
// store: an empty server lists no namespaces; the first write materializes
// acme/phoenix/api (there is no create call); the child's read-set already
// includes its ancestors even though they hold no rows (ancestors are
// lexical, no existence check); a visibility write from the child lands in
// acme/phoenix, materializing it; scope "everywhere" from acme discovers
// the subtree (only namespaces with rows); and deleting the memories makes
// the namespaces vanish from the listing again.
package service_test

import (
	"context"
	"testing"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
)

func TestExampleNamespaceLifecycle(t *testing.T) {
	ctx := context.Background()
	svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce())

	// Stage 1: a fresh server has no namespaces — the listing is empty
	// because namespaces exist only as the distinct namespace strings of
	// stored rows.
	ns, err := svc.Namespaces(ctx)
	if err != nil {
		t.Fatalf("namespaces on empty server: %v", err)
	}
	if len(ns) != 0 {
		t.Fatalf("empty server should list no namespaces, got %v", ns)
	}

	// Stage 2: the first write materializes acme/phoenix/api. No create
	// call exists (or is needed) anywhere in the API.
	apiFact, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "acme/phoenix/api",
		Content:   "the api service reads feature flags from postgres",
		Tier:      memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("first remember: %v", err)
	}
	if apiFact.Namespace != "acme/phoenix/api" {
		t.Fatalf("first write landed in %q, want acme/phoenix/api", apiFact.Namespace)
	}
	ns, err = svc.Namespaces(ctx)
	if err != nil {
		t.Fatalf("namespaces after first write: %v", err)
	}
	if len(ns) != 1 || ns[0] != "acme/phoenix/api" {
		t.Fatalf("after the first write the listing should be exactly [acme/phoenix/api], got %v", ns)
	}

	// Stage 3: the child's default read-set already cascades through its
	// ancestors — acme/phoenix and acme — even though NEITHER holds a row.
	// Ancestors are computed lexically from the namespace path; there is no
	// existence check.
	readSet, err := svc.ResolveReadSetInfo(ctx, "acme/phoenix/api", "")
	if err != nil {
		t.Fatalf("resolve read set: %v", err)
	}
	want := []service.ReadSetEntry{
		{NS: "acme/phoenix/api", Origin: service.OriginPrimary},
		{NS: "acme/phoenix", Origin: service.OriginAncestor},
		{NS: "acme", Origin: service.OriginAncestor},
	}
	if len(readSet) != len(want) {
		t.Fatalf("read set = %+v, want 3 legs (primary + two lexical ancestors)", readSet)
	}
	for i, w := range want {
		if readSet[i].NS != w.NS || readSet[i].Origin != w.Origin {
			t.Fatalf("read set[%d] = %+v, want NS=%q Origin=%q", i, readSet[i], w.NS, w.Origin)
		}
	}
	// The ancestor legs are durable-only (semantic + procedural).
	for _, e := range readSet[1:] {
		if len(e.Tiers) != 2 || e.Tiers[0] != memory.TierSemantic || e.Tiers[1] != memory.TierProcedural {
			t.Fatalf("ancestor leg %q should be restricted to [semantic procedural], got %v", e.NS, e.Tiers)
		}
	}

	// Stage 4: a team-wide fact written UP from the child with visibility
	// "phoenix" — the unambiguous last segment of the ancestor acme/phoenix
	// — lands there and materializes it.
	teamFact, err := svc.Remember(ctx, service.RememberInput{
		Namespace:  "acme/phoenix/api",
		Visibility: "phoenix",
		Content:    "the phoenix team deploys through forgejo actions",
		Tier:       memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("visibility write: %v", err)
	}
	if teamFact.Namespace != "acme/phoenix" {
		t.Fatalf("visibility \"phoenix\" should land the write in acme/phoenix, got %q", teamFact.Namespace)
	}
	ns, err = svc.Namespaces(ctx)
	if err != nil {
		t.Fatalf("namespaces after visibility write: %v", err)
	}
	if len(ns) != 2 || ns[0] != "acme/phoenix" || ns[1] != "acme/phoenix/api" {
		t.Fatalf("listing after the visibility write should be [acme/phoenix acme/phoenix/api], got %v", ns)
	}

	// Stage 5: scope "everywhere" from acme discovers the subtree. The
	// read-set holds only the primary plus namespaces that actually have
	// rows — no phantom entries — and every subtree member is origin
	// "primary" (subtree widens the primary leg, it is not a cascade leg).
	var everywhereSet []service.ReadSetEntry
	results, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "acme",
		Query:     "feature flags postgres",
		Scope:     "everywhere",
		Limit:     10,
		ReadSet:   &everywhereSet,
	})
	if err != nil {
		t.Fatalf("recall scope=everywhere: %v", err)
	}
	gotNS := make([]string, len(everywhereSet))
	for i, e := range everywhereSet {
		if e.Origin != service.OriginPrimary {
			t.Fatalf("subtree leg %q has origin %q, want primary", e.NS, e.Origin)
		}
		gotNS[i] = e.NS
	}
	if len(gotNS) != 3 || gotNS[0] != "acme" || gotNS[1] != "acme/phoenix" || gotNS[2] != "acme/phoenix/api" {
		t.Fatalf("everywhere read set = %v, want [acme acme/phoenix acme/phoenix/api]", gotNS)
	}
	foundAPI := false
	for _, r := range results {
		if r.Memory.ID == apiFact.ID && r.Memory.Namespace == "acme/phoenix/api" {
			foundAPI = true
		}
	}
	if !foundAPI {
		t.Fatalf("everywhere recall from acme should surface the grandchild's fact, got %+v", results)
	}

	// Stage 6: forgetting the memories empties the namespaces, and empty
	// namespaces vanish from the listing — there is no delete-namespace
	// bookkeeping to run, because there was never a namespace object.
	if err := svc.Forget(ctx, "acme/phoenix/api", apiFact.ID); err != nil {
		t.Fatalf("forget api fact: %v", err)
	}
	if err := svc.Forget(ctx, "acme/phoenix", teamFact.ID); err != nil {
		t.Fatalf("forget team fact: %v", err)
	}
	ns, err = svc.Namespaces(ctx)
	if err != nil {
		t.Fatalf("namespaces after deletes: %v", err)
	}
	if len(ns) != 0 {
		t.Fatalf("after deleting every memory the listing should be empty again, got %v", ns)
	}
}
