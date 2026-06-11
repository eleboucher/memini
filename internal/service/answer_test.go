package service_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

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
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(ans))
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
	if !strings.Contains(ans.user, "postgres is a relational database") {
		t.Fatalf("reader prompt should include the recalled memory; got %q", ans.user)
	}
}

func TestAnswerRequiresAnswerer(t *testing.T) {
	svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce())
	if _, err := svc.Answer(context.Background(), service.AnswerInput{Namespace: "alice", Query: "x"}); err == nil {
		t.Fatal("want error when no answerer is configured")
	}
}
