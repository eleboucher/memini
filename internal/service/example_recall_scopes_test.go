// Validates docs/examples/recall-in-practice.md.
//
// One question asked at three scopes: "project" sees only the request
// namespace's own fact; "full" adds the ancestor's durable fact and the
// home namespace's durable fact — but NOT the ancestor's episodic event,
// because non-primary read-set legs are durable-only; "everywhere" adds the
// PRIMARY namespace's subtree (a child under acme/phoenix/api, not a
// sibling of it). Results carry per-leg "from" provenance via the ReadSet
// out-param. The briefing for the same namespace shows the scope header and
// does NOT bump access counts, while an explicit recall does — the
// log-vs-reinforce invariant events_test.go pins.
package service_test

import (
	"context"
	"testing"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

func TestExampleRecallScopes(t *testing.T) {
	ctx := context.Background()
	svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce())

	remember := func(in service.RememberInput) *memory.Memory {
		t.Helper()
		m, err := svc.Remember(ctx, in)
		if err != nil {
			t.Fatalf("remember %q: %v", in.Content, err)
		}
		if m == nil {
			t.Fatalf("remember %q: dropped by the value gate", in.Content)
		}
		return m
	}

	// Seed four namespaces around the working namespace acme/phoenix/api:
	// its own project fact, a durable team fact AND an episodic team event
	// in the ancestor acme/phoenix, a durable personal preference in the
	// caller's home namespace jon-personal, and a durable fact in the child
	// acme/phoenix/api/worker (inside the primary's subtree).
	projectFact := remember(service.RememberInput{
		Namespace: "acme/phoenix/api",
		Content:   "the api service reads feature flags from postgres",
		Tier:      memory.TierSemantic,
	})
	remember(service.RememberInput{
		Namespace: "acme/phoenix",
		Content:   "the phoenix team standardized on postgres 16 for every service",
		Tier:      memory.TierSemantic,
	})
	remember(service.RememberInput{
		Namespace: "acme/phoenix",
		Content:   "migrated the shared postgres cluster to new hardware on tuesday",
		Tier:      memory.TierEpisodic,
	})
	remember(service.RememberInput{
		Namespace: "jon-personal",
		Content:   "jon prefers postgres over mysql for side projects",
		Tier:      memory.TierSemantic,
	})
	remember(service.RememberInput{
		Namespace: "acme/phoenix/api/worker",
		Content:   "the worker drains the postgres outbox table every minute",
		Tier:      memory.TierSemantic,
	})

	recall := func(scope string, readSet *[]service.ReadSetEntry) []store.Scored {
		t.Helper()
		res, err := svc.Recall(ctx, service.RecallInput{
			Namespace: "acme/phoenix/api",
			Home:      "jon-personal",
			Query:     "postgres",
			Scope:     scope,
			Limit:     10,
			ReadSet:   readSet,
		})
		if err != nil {
			t.Fatalf("recall scope=%q: %v", scope, err)
		}
		return res
	}
	namespacesOf := func(res []store.Scored) map[string]int {
		got := map[string]int{}
		for _, r := range res {
			got[r.Memory.Namespace]++
		}
		return got
	}

	// Scope "project": the request namespace only — no ancestors, no home.
	res := recall("project", nil)
	if len(res) != 1 || res[0].Memory.ID != projectFact.ID {
		t.Fatalf(`scope "project" should return exactly the project fact, got %+v`, namespacesOf(res))
	}

	// Scope "full" (the default): adds the ancestor cascade and the home
	// leg — durable tiers only, so the ancestor's episodic event does NOT
	// appear, and neither does the child (subtree is not part of "full").
	var fullSet []service.ReadSetEntry
	res = recall("full", &fullSet)
	got := namespacesOf(res)
	if len(res) != 3 || got["acme/phoenix/api"] != 1 || got["acme/phoenix"] != 1 || got["jon-personal"] != 1 {
		t.Fatalf(`scope "full" should return the project + ancestor + home durable facts, got %+v`, got)
	}
	for _, r := range res {
		if r.Memory.Tier == memory.TierEpisodic {
			t.Fatalf("the ancestor's episodic event leaked into a non-primary leg: %q", r.Memory.Content)
		}
	}

	// "from" provenance: the origin recorded on each read-set leg renders
	// as the API's "from" field — empty for the primary namespace, the
	// namespace itself for ancestor and home hits.
	origins := service.OriginMap(fullSet)
	wantFrom := map[string]string{
		"acme/phoenix/api": "",
		"acme/phoenix":     "acme/phoenix",
		"jon-personal":     "jon-personal",
	}
	for _, r := range res {
		if from := service.ReadSetFrom(origins, r.Memory.Namespace); from != wantFrom[r.Memory.Namespace] {
			t.Fatalf("from provenance for %q = %q, want %q", r.Memory.Namespace, from, wantFrom[r.Memory.Namespace])
		}
	}

	// Scope "everywhere": "full" plus the PRIMARY namespace's subtree, so
	// the child acme/phoenix/api/worker joins. A sibling of the primary
	// (e.g. acme/phoenix/other) would NOT: the subtree expands under the
	// request namespace, not under its parent.
	var everySet []service.ReadSetEntry
	res = recall("everywhere", &everySet)
	got = namespacesOf(res)
	if len(res) != 4 || got["acme/phoenix/api/worker"] != 1 {
		t.Fatalf(`scope "everywhere" should add the subtree child's fact, got %+v`, got)
	}
	everyOrigins := service.OriginMap(everySet)
	if o := everyOrigins["acme/phoenix/api/worker"]; o != service.OriginPrimary {
		t.Fatalf("subtree member origin = %q, want primary (subtree widens the primary leg)", o)
	}
	if from := service.ReadSetFrom(everyOrigins, "acme/phoenix/api/worker"); from != "" {
		t.Fatalf("subtree hits carry no from annotation, got %q", from)
	}

	// Explicit recall reinforces: the project fact's access count grew.
	afterRecalls, err := svc.Get(ctx, "acme/phoenix/api", projectFact.ID)
	if err != nil {
		t.Fatalf("get after recalls: %v", err)
	}
	if afterRecalls.AccessCount <= projectFact.AccessCount {
		t.Fatalf("access_count = %d after three recalls, want > %d: recall reinforces",
			afterRecalls.AccessCount, projectFact.AccessCount)
	}
	baseline := afterRecalls.AccessCount

	// The briefing for the same namespace: durable facts from every
	// contributing leg, no episodic from the ancestor, a child rollup for
	// the worker namespace, and a scope header naming only the legs that
	// contributed durable memories (acme contributed nothing and is
	// omitted).
	b, err := svc.Briefing(ctx, "acme/phoenix/api", service.BriefingOpts{Home: "jon-personal"})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if len(b.Facts) != 3 {
		t.Fatalf("briefing facts = %d, want the 3 durable facts across primary+ancestor+home", len(b.Facts))
	}
	if len(b.Recent) != 0 {
		t.Fatalf("briefing recent should be empty — the ancestor's episodic event must not cascade, got %+v", b.Recent)
	}
	wantHeader := "Scope: acme/phoenix/api ← acme/phoenix(1) ← jon-personal(1)"
	if b.ScopeHeader != wantHeader {
		t.Fatalf("scope header = %q, want %q", b.ScopeHeader, wantHeader)
	}
	if len(b.Children) != 1 || b.Children[0].NS != "acme/phoenix/api/worker" || b.Children[0].Total != 1 {
		t.Fatalf("briefing children = %+v, want one rollup for acme/phoenix/api/worker", b.Children)
	}

	// The briefing did NOT reinforce: access counts are untouched — the
	// invariant TestBriefingAndGetLogButDoNotReinforce (events_test.go)
	// pins in depth.
	afterBriefing, err := svc.Get(ctx, "acme/phoenix/api", projectFact.ID)
	if err != nil {
		t.Fatalf("get after briefing: %v", err)
	}
	if afterBriefing.AccessCount != baseline {
		t.Fatalf("access_count = %d after briefing, want %d unchanged: briefings never reinforce",
			afterBriefing.AccessCount, baseline)
	}
}
