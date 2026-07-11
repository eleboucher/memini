package service_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

func openTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "svc.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// fakeAnswerer is a stand-in chat client: it records the user prompt and returns
// a canned answer.
type fakeAnswerer struct {
	resp string
	user string
}

func (f *fakeAnswerer) Complete(_ context.Context, _, user string) (string, error) {
	f.user = user
	return f.resp, nil
}

func TestAnswerGroundsOnRecall(t *testing.T) {
	ctx := context.Background()
	ans := &fakeAnswerer{resp: "postgres"}
	st := openTestStore(t)
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(ans),
		// Frozen clock so the date annotation in the reader prompt is assertable.
		service.WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }))
	if _, err := svc.Remember(ctx, service.RememberInput{Namespace: "alice", Content: "postgres is a relational database", Tier: memory.TierSemantic}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	res, err := svc.Answer(ctx, service.AnswerInput{Namespace: "alice", Query: "which database?", Limit: 5})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if res.Answer != "postgres" {
		t.Fatalf("answer = %q, want postgres", res.Answer)
	}
	if len(res.Sources) == 0 {
		t.Fatal("expected grounding sources")
	}
	// The system prompt instructs the model to resolve relative dates against
	// the [bracketed] date each memory carries — pin that the annotation is
	// actually rendered (1_700_000_000 = 2023-11-14 UTC).
	if !strings.Contains(ans.user, "1. [2023-11-14] postgres is a relational database") {
		t.Fatalf("reader prompt should number and date-prefix the recalled memory; got %q", ans.user)
	}
}

// TestAnswerGroundsOnHomeNamespace pins gap G1: AnswerInput.Home threads into
// every recall the single-shot answer path performs (answer.go:104), so a
// durable fact that lives only in the caller's home namespace (personal/kit)
// is available as grounding when the question is asked from an unrelated
// namespace (acme/phoenix) — and is correctly absent when Home is unset.
func TestAnswerGroundsOnHomeNamespace(t *testing.T) {
	ctx := context.Background()
	ans := &fakeAnswerer{resp: "ed25519"}
	st := openTestStore(t)
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(ans))

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "personal/kit", Content: "jon's personal laptop ssh key is ed25519", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}

	res, err := svc.Answer(ctx, service.AnswerInput{
		Namespace: "acme/phoenix", Home: "personal/kit", Query: "what is the ssh key type", Limit: 5,
	})
	if err != nil {
		t.Fatalf("answer with home: %v", err)
	}
	if len(res.Sources) != 1 || res.Sources[0].Memory.Namespace != "personal/kit" {
		t.Fatalf("answer sources should include the home-namespace memory, got %+v", res.Sources)
	}

	res, err = svc.Answer(ctx, service.AnswerInput{
		Namespace: "acme/phoenix", Query: "what is the ssh key type", Limit: 5,
	})
	if err != nil {
		t.Fatalf("answer without home: %v", err)
	}
	if len(res.Sources) != 0 {
		t.Fatalf("answer without Home must not see the home namespace, got %+v", res.Sources)
	}
}

// TestAnswerExpandGroundsOnHomeNamespace pins gap G1 for the expand reasoning
// strategy (answer_expand.go:113): Home threads into the per-rewrite recalls,
// so a home-namespace memory is still reachable when Reasoning=expand.
func TestAnswerExpandGroundsOnHomeNamespace(t *testing.T) {
	ctx := context.Background()
	ans := &fakeAnswerer{resp: "ed25519"}
	st := openTestStore(t)
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(ans))

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "personal/kit", Content: "jon's personal laptop ssh key is ed25519", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}

	res, err := svc.Answer(ctx, service.AnswerInput{
		Namespace: "acme/phoenix", Home: "personal/kit", Query: "what is the ssh key type",
		Limit: 5, Reasoning: service.ReasoningExpand,
	})
	if err != nil {
		t.Fatalf("answer expand with home: %v", err)
	}
	foundHome := false
	for _, s := range res.Sources {
		if s.Memory.Namespace == "personal/kit" {
			foundHome = true
		}
	}
	if !foundHome {
		t.Fatalf("expand answer sources missing home-namespace memory, got %+v", res.Sources)
	}
}

// TestAnswerScopeProjectExcludesAncestors pins AnswerInput.Scope threading
// into the single-shot grounding recall: "project" restricts grounding to the
// request namespace's own memories, so an ancestor fact that the default
// (full) cascade would surface is absent — mirroring RecallInput.Scope.
func TestAnswerScopeProjectExcludesAncestors(t *testing.T) {
	ctx := context.Background()
	ans := &fakeAnswerer{resp: "forgejo"}
	st := openTestStore(t)
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(ans))

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "acme", Content: "acme uses forgejo for CI", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}

	res, err := svc.Answer(ctx, service.AnswerInput{
		Namespace: "acme/phoenix", Query: "what CI system", Limit: 5,
	})
	if err != nil {
		t.Fatalf("answer default scope: %v", err)
	}
	if len(res.Sources) != 1 || res.Sources[0].Memory.Namespace != "acme" {
		t.Fatalf("default (full) scope should ground on the ancestor fact, got %+v", res.Sources)
	}

	res, err = svc.Answer(ctx, service.AnswerInput{
		Namespace: "acme/phoenix", Query: "what CI system", Limit: 5, Scope: "project",
	})
	if err != nil {
		t.Fatalf("answer scope=project: %v", err)
	}
	if len(res.Sources) != 0 {
		t.Fatalf(`Scope "project" must not ground on ancestor namespaces, got %+v`, res.Sources)
	}
}

// TestAnswerScopeEverywhereIncludesSubtree pins the other end of the scope
// vocabulary on the single-shot path: "everywhere" adds the request
// namespace's subtree to the grounding read-set, which the default (full)
// scope does not include.
func TestAnswerScopeEverywhereIncludesSubtree(t *testing.T) {
	ctx := context.Background()
	ans := &fakeAnswerer{resp: "helm"}
	st := openTestStore(t)
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(ans))

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "acme/phoenix", Content: "phoenix deploys with helm", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}

	res, err := svc.Answer(ctx, service.AnswerInput{
		Namespace: "acme", Query: "how does phoenix deploy", Limit: 5,
	})
	if err != nil {
		t.Fatalf("answer default scope: %v", err)
	}
	if len(res.Sources) != 0 {
		t.Fatalf("default (full) scope must not ground on nested sub-project memories, got %+v", res.Sources)
	}

	res, err = svc.Answer(ctx, service.AnswerInput{
		Namespace: "acme", Query: "how does phoenix deploy", Limit: 5, Scope: "everywhere",
	})
	if err != nil {
		t.Fatalf("answer scope=everywhere: %v", err)
	}
	if len(res.Sources) != 1 || res.Sources[0].Memory.Namespace != "acme/phoenix" {
		t.Fatalf(`Scope "everywhere" should ground on the sub-project fact, got %+v`, res.Sources)
	}
}

// TestAnswerScopeInvalidErrors pins that an unrecognized Scope value —
// including the legacy "subtree"/"exact" — is a caller error listing the
// valid vocabulary, and that it fails before the LLM is consulted.
func TestAnswerScopeInvalidErrors(t *testing.T) {
	ctx := context.Background()
	ans := &fakeAnswerer{resp: "n/a"}
	svc := service.New(openTestStore(t), embedtest.New(dims), service.WithAnswerer(ans))

	for _, scope := range []string{"bogus", "subtree", "exact"} {
		_, err := svc.Answer(ctx, service.AnswerInput{
			Namespace: "acme", Query: "anything", Scope: scope,
		})
		if err == nil {
			t.Fatalf("Scope %q should be rejected", scope)
		}
		if !strings.Contains(err.Error(), "everywhere") {
			t.Fatalf("Scope %q error = %q, want it to list the valid vocabulary", scope, err)
		}
	}
	if ans.user != "" {
		t.Fatal("an invalid scope must fail before the LLM is consulted")
	}
}

// TestAnswerExpandThreadsScope pins Scope threading into the expand reasoning
// strategy's per-rewrite recalls (answer_expand.go), mirroring the Home
// threading test above: with Scope "project", an ancestor fact must not be
// grounding even though the default cascade would reach it.
func TestAnswerExpandThreadsScope(t *testing.T) {
	ctx := context.Background()
	ans := &fakeAnswerer{resp: "forgejo"}
	st := openTestStore(t)
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(ans))

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "acme", Content: "acme uses forgejo for CI", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}

	res, err := svc.Answer(ctx, service.AnswerInput{
		Namespace: "acme/phoenix", Query: "what CI system", Limit: 5,
		Reasoning: service.ReasoningExpand, Scope: "project",
	})
	if err != nil {
		t.Fatalf("answer expand scope=project: %v", err)
	}
	if len(res.Sources) != 0 {
		t.Fatalf(`expand with Scope "project" must not ground on ancestor namespaces, got %+v`, res.Sources)
	}
}

// TestAnswerTagsConflictingMemories pins the deterministic conflict tagging:
// when recall surfaces two live memories the lexical detector classifies as a
// value-swap update, both lines carry a [may conflict with #N] tag so the
// reader can apply the conflict rule instead of silently picking one.
func TestAnswerTagsConflictingMemories(t *testing.T) {
	ctx := context.Background()
	ans := &fakeAnswerer{resp: "60 seconds"}
	st := openTestStore(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	clock := now
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(ans),
		service.WithClock(func() time.Time { return clock }))

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the api timeout is 30 seconds", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember old: %v", err)
	}
	clock = now.Add(24 * time.Hour)
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the api timeout is 60 seconds", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember new: %v", err)
	}

	if _, err := svc.Answer(ctx, service.AnswerInput{Namespace: "alice", Query: "api timeout", Limit: 5}); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if got := strings.Count(ans.user, "[may conflict with #"); got != 2 {
		t.Fatalf("both sides of the value swap should be tagged, got %d tag(s) in %q", got, ans.user)
	}
}

// TestAnswerPrefersValidFromDate pins that a backdated fact is annotated with
// when it was true (ValidFrom), not when it was recorded, so relative time
// references resolve against the right anchor.
func TestAnswerPrefersValidFromDate(t *testing.T) {
	ctx := context.Background()
	ans := &fakeAnswerer{resp: "ok"}
	st := openTestStore(t)
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(ans),
		service.WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }))

	validFrom := time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "moved to berlin for the platform job", Tier: memory.TierSemantic,
		ValidFrom: &validFrom,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if _, err := svc.Answer(ctx, service.AnswerInput{Namespace: "alice", Query: "berlin", Limit: 5}); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if !strings.Contains(ans.user, "[2021-06-01] moved to berlin") {
		t.Fatalf("reader prompt should anchor on ValidFrom; got %q", ans.user)
	}
}

// TestAnswerFiltersGrounding pins that tag/metadata filters narrow the memories
// the answer is grounded on, mirroring Recall — so "answer from my bug_fixes"
// can't be polluted by unrelated memories.
func TestAnswerFiltersGrounding(t *testing.T) {
	ctx := context.Background()
	ans := &fakeAnswerer{resp: "ok"}
	st := openTestStore(t)
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(ans))

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the auth bug was a race condition", Tier: memory.TierSemantic,
		Tags: []string{"auth"}, Metadata: map[string]any{"category": "bug_fixes"},
	}); err != nil {
		t.Fatalf("remember bug: %v", err)
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the auth handler latency improved", Tier: memory.TierSemantic,
		Tags: []string{"auth"}, Metadata: map[string]any{"category": "performance_findings"},
	}); err != nil {
		t.Fatalf("remember perf: %v", err)
	}

	res, err := svc.Answer(ctx, service.AnswerInput{
		Namespace: "alice", Query: "auth", Limit: 5,
		Metadata: map[string]string{"category": "bug_fixes"},
	})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if len(res.Sources) != 1 {
		t.Fatalf("metadata filter should ground on 1 source, got %d", len(res.Sources))
	}
	if !strings.Contains(res.Sources[0].Memory.Content, "race condition") {
		t.Fatalf("grounded on the wrong memory: %q", res.Sources[0].Memory.Content)
	}
}

func TestAnswerRequiresAnswerer(t *testing.T) {
	svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce())
	if _, err := svc.Answer(context.Background(), service.AnswerInput{Namespace: "alice", Query: "x"}); err == nil {
		t.Fatal("want error when no answerer is configured")
	}
}
