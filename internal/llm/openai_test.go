package llm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eleboucher/memini/internal/llm"
)

func TestConsolidateParsesDecision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":
			"{\"action\":\"update\",\"target\":\"m1\",\"content\":\"merged\",\"summary\":\"s\",\"reason\":\"dup\"}"
		}}]}`))
	}))
	defer srv.Close()

	c, err := llm.NewOpenAI(llm.OpenAIConfig{BaseURL: srv.URL, Model: "m"})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	dec, err := c.Consolidate(context.Background(), llm.Input{New: "x"})
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if dec.Action != llm.ActionUpdate {
		t.Errorf("Action = %q, want update", dec.Action)
	}
	if dec.Target != "m1" || dec.Content != "merged" {
		t.Errorf("decision = %+v", dec)
	}
}

func TestConsolidateRejectsInvalidAction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"action\":\"delete\"}"}}]}`))
	}))
	defer srv.Close()

	c, _ := llm.NewOpenAI(llm.OpenAIConfig{BaseURL: srv.URL, Model: "m"})
	if _, err := c.Consolidate(context.Background(), llm.Input{New: "x"}); err == nil {
		t.Fatal("expected invalid-action error, got nil")
	}
}

func TestCompleteReturnsText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"the answer"}}]}`))
	}))
	defer srv.Close()

	c, _ := llm.NewOpenAI(llm.OpenAIConfig{BaseURL: srv.URL, Model: "m"})
	got, err := c.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "the answer" {
		t.Errorf("Complete = %q, want %q", got, "the answer")
	}
}

func TestCompleteRetriesAfter429(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c, _ := llm.NewOpenAI(llm.OpenAIConfig{BaseURL: srv.URL, Model: "m"})
	got, err := c.Complete(context.Background(), "s", "u")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "ok" {
		t.Errorf("Complete = %q, want ok", got)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Errorf("expected 2 requests (429 then 200), got %d", hits)
	}
}
