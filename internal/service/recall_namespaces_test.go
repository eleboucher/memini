package service_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestRecallExplicitNamespacesSpansGiven: recall with explicit Namespaces
// searches exactly those namespaces, replacing the default cascade (here, a
// configured Home leg) rather than extending it, and a plain recall on the
// same store (no Namespaces set) stays scoped to the request namespace plus
// home, unaffected by the explicit call.
func TestRecallExplicitNamespacesSpansGiven(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	mk := func(ns, content string) {
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns, Content: content, Tier: memory.TierSemantic,
		}); err != nil {
			t.Fatalf("remember %q in %q: %v", content, ns, err)
		}
	}
	mk("A", "widget factory uses a builder pattern")
	mk("B", "widget factory uses a builder pattern too")
	mk("personal/kit", "widget factory home convention")

	explicit, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "A", Query: "widget factory builder pattern", Limit: 10,
		Namespaces: []string{"A", "B"}, Home: "personal/kit",
	})
	if err != nil {
		t.Fatalf("explicit recall: %v", err)
	}
	gotNS := make([]string, 0, len(explicit))
	for _, r := range explicit {
		gotNS = append(gotNS, r.Memory.Namespace)
	}
	if !slices.Contains(gotNS, "A") || !slices.Contains(gotNS, "B") {
		t.Fatalf("explicit recall should span A and B, got namespaces %v", gotNS)
	}
	if slices.Contains(gotNS, "personal/kit") {
		t.Fatal("explicit Namespaces must replace the default read set, not extend it with home")
	}

	plain, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "A", Query: "widget factory builder pattern", Limit: 10, Home: "personal/kit",
	})
	if err != nil {
		t.Fatalf("plain recall: %v", err)
	}
	gotPlainNS := make([]string, 0, len(plain))
	for _, r := range plain {
		if r.Memory.Namespace == "B" {
			t.Fatal("plain recall in A must not leak B without explicit Namespaces")
		}
		gotPlainNS = append(gotPlainNS, r.Memory.Namespace)
	}
	if !slices.Contains(gotPlainNS, "personal/kit") {
		t.Fatal("plain recall in A should still see the configured home namespace")
	}
}

// TestRecallAncestorMergesDurableOnly: a nested namespace implicitly reads
// its ancestor's ("work", the interior node — the design's replacement for
// the deleted work/_shared convention) durable memories read-only, and never
// its episodic ones — end-to-end through the public Recall() call, no
// config required (ancestors are always on).
func TestRecallAncestorMergesDurableOnly(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	mk := func(ns, content string, tier memory.Tier) {
		if _, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: content, Tier: tier}); err != nil {
			t.Fatalf("remember %q in %q: %v", content, ns, err)
		}
	}
	mk("work", "shared durable convention: no AI slop filler comments", memory.TierSemantic)
	mk("work", "shared episodic chatter about lunch", memory.TierEpisodic)
	mk("work/memini", "work/memini deploys with helm charts", memory.TierSemantic)

	has := func(rs []store.Scored, content string) bool {
		for _, r := range rs {
			if r.Memory.Content == content {
				return true
			}
		}
		return false
	}

	res, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "work/memini", Query: "AI slop filler comments", Limit: 10,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !has(res, "shared durable convention: no AI slop filler comments") {
		t.Fatal("ancestor durable memory should surface in work/memini recall")
	}

	res, err = svc.Recall(ctx, service.RecallInput{
		Namespace: "work/memini", Query: "shared episodic chatter lunch", Limit: 10,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if has(res, "shared episodic chatter about lunch") {
		t.Fatal("ancestor episodic memory must not surface in another namespace's recall")
	}
}

// TestRecallSiblingsNeverCrossWithoutLink: the design's isolation guarantee —
// sibling namespaces share nothing unless explicitly linked, because neither
// is an ancestor of the other and no link connects them.
func TestRecallSiblingsNeverCrossWithoutLink(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "work/teamA", Content: "work secret: prod deploy runbook", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember work/teamA: %v", err)
	}

	res, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "personal/blog", Query: "prod deploy runbook", Limit: 10,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	for _, r := range res {
		if r.Memory.Namespace == "work/teamA" {
			t.Fatal("personal/blog recall must never surface work/teamA — sibling isolation without a link")
		}
	}
}

// TestRecallScopeProjectExcludesAncestors: Scope "project" reduces the read
// set to the primary namespace alone, hiding an ancestor durable fact that a
// default-scope recall would otherwise surface.
func TestRecallScopeProjectExcludesAncestors(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "work", Content: "ancestor rule about deploy pipelines", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember work: %v", err)
	}

	res, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "work/memini", Query: "ancestor rule deploy pipelines", Limit: 10, Scope: "project",
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	for _, r := range res {
		if r.Memory.Namespace == "work" {
			t.Fatal(`Scope "project" must not surface ancestor namespaces`)
		}
	}
}

// TestRecallScopeInvalidErrors: an unrecognized Scope value is a caller
// input error, not a silent fallback to the default cascade.
func TestRecallScopeInvalidErrors(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	_, err := svc.Recall(ctx, service.RecallInput{Namespace: "A", Query: "q", Scope: "bogus"})
	if err == nil || !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput for an unrecognized Scope", err)
	}
}

// TestRecallExplicitBeatsScopeEverywhere is the end-to-end precedence test
// for gap G8: explicit Namespaces replaces the cascade outright even when
// Scope requests the widest possible default (subtree + ancestors + home +
// links) — explicit always wins.
func TestRecallExplicitBeatsScopeEverywhere(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "work", Content: "ancestor rule about deploy pipelines", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember work: %v", err)
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "work/memini/agent", Content: "child rule about deploy pipelines", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember child: %v", err)
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "other", Content: "explicit rule about deploy pipelines", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember other: %v", err)
	}

	res, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "work/memini", Query: "rule deploy pipelines", Limit: 10,
		Scope: "everywhere", Namespaces: []string{"other"},
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	for _, r := range res {
		if r.Memory.Namespace != "other" {
			t.Fatalf("explicit Namespaces must replace scope %q entirely, got a hit from %q", "everywhere", r.Memory.Namespace)
		}
	}
}

// TestRecallExcludeMetadataAppliesAcrossExplicitNamespaces is the echo-guard
// regression: a caller's session-scoped ExcludeMetadata filter must exclude
// matching memories in every namespace of a multi-namespace fan-out, not just
// the primary — the filter is copied per fan-out leg (service.go's `f :=
// filter` inside the loop), and this proves that survives the read-set
// refactor.
func TestRecallExcludeMetadataAppliesAcrossExplicitNamespaces(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	mk := func(ns, content, sessionID string) {
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns, Content: content, Tier: memory.TierEpisodic,
			Metadata: map[string]any{"session_id": sessionID},
		}); err != nil {
			t.Fatalf("remember %q in %q: %v", content, ns, err)
		}
	}
	mk("A", "session turn about database migrations", "sess-1")
	mk("B", "session turn about database migrations", "sess-1")
	mk("A", "unrelated turn about database migrations", "sess-2")

	res, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "A", Query: "database migrations", Limit: 10,
		Namespaces:      []string{"A", "B"},
		ExcludeMetadata: map[string]string{"session_id": "sess-1"},
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	for _, r := range res {
		if md, _ := r.Memory.Metadata["session_id"].(string); md == "sess-1" {
			t.Fatalf("ExcludeMetadata leaked a sess-1 memory in namespace %q", r.Memory.Namespace)
		}
	}
	found2 := false
	for _, r := range res {
		if r.Memory.Namespace == "A" {
			if md, _ := r.Memory.Metadata["session_id"].(string); md == "sess-2" {
				found2 = true
			}
		}
	}
	if !found2 {
		t.Fatal("recall should still surface the sess-2 memory (only sess-1 is excluded)")
	}
}

// TestRecallIncludeLinkedFetchesFromSourceNamespace: with a multi-namespace
// read-set, a linked-memory expansion must fetch the linked ID from the
// namespace of the result that carried the link (Memory.Namespace), not the
// request's primary namespace — the two only coincide by default. Seeds
// LinkedMemoryIDs directly (normally set by the LLM consolidator) so this
// doesn't depend on that machinery.
func TestRecallIncludeLinkedFetchesFromSourceNamespace(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "linked.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := embedtest.New(dims)
	now := time.Unix(1_700_000_000, 0).UTC()

	embedAndPut := func(id, ns, content string, linked ...string) {
		vec, err := embed.EmbedOne(ctx, e, content)
		if err != nil {
			t.Fatalf("embed: %v", err)
		}
		m := &memory.Memory{
			ID: id, Namespace: ns, Tier: memory.TierSemantic, Content: content,
			CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Embedding: vec,
			LinkedMemoryIDs: linked,
		}
		if err := st.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	// b-link shares no vocabulary with the query, so it only ever surfaces via
	// the LinkedMemoryIDs expansion, not a direct search hit — isolating the
	// behavior under test. It lives in namespace B, linked from a hit that also
	// lives in B; a stale/wrong namespace lookup (e.g. always using the
	// request's primary namespace "A") would 404 and silently drop it.
	embedAndPut("b-hit", "B", "widget rollout plan", "b-link")
	embedAndPut("b-link", "B", "unrelated archived notes about kitchen inventory")
	embedAndPut("a-filler", "A", "unconnected filler text")

	svc := service.New(st, e, service.WithClock(func() time.Time { return now }), service.WithSyncReinforce())
	res, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "A", Query: "widget rollout plan", Limit: 10,
		Namespaces: []string{"A", "B"}, IncludeLinked: true,
		// Gate out a-filler and b-link as direct hits — neither shares any
		// vocabulary with the query — so b-link can only reach the result set
		// through the LinkedMemoryIDs expansion under test.
		MinScore: 0.5,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	foundLink := false
	for _, r := range res {
		if r.Memory.ID == "b-link" {
			foundLink = true
			if r.Memory.Namespace != "B" {
				t.Fatalf("linked memory namespace = %q, want B", r.Memory.Namespace)
			}
		}
	}
	if !foundLink {
		t.Fatal("expected the linked memory b-link to be fetched from its own namespace B")
	}
}
