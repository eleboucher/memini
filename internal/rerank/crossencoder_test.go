package rerank

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eleboucher/memini/internal/llm"
)

func TestCrossEncoderRerankOrdersByScore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" {
			t.Errorf("path = %q, want /rerank", r.URL.Path)
		}
		// Unsorted on purpose: doc 1 most relevant, then 2, then 0.
		_, _ = w.Write([]byte(`{"results":[
			{"index":0,"relevance_score":0.1},
			{"index":2,"relevance_score":0.9},
			{"index":1,"relevance_score":0.95}
		]}`))
	}))
	defer srv.Close()

	ce, err := New(Config{BaseURL: srv.URL, Model: "m"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	cands := []llm.RerankCandidate{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	got, err := ce.Rerank(context.Background(), "q", cands)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	want := []string{"b", "c", "a"} // index 1 (0.95) > 2 (0.9) > 0 (0.1)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestCrossEncoderAppendsOmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Server returns only one result; the rest must follow in original order.
		_, _ = w.Write([]byte(`{"results":[{"index":2,"relevance_score":0.9}]}`))
	}))
	defer srv.Close()

	ce, _ := New(Config{BaseURL: srv.URL})
	got, err := ce.Rerank(context.Background(), "q", []llm.RerankCandidate{{ID: "a"}, {ID: "b"}, {ID: "c"}})
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	want := []string{"c", "a", "b"} // ranked c, then omitted a,b in original order
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestCrossEncoderErrorsOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ce, _ := New(Config{BaseURL: srv.URL})
	if _, err := ce.Rerank(context.Background(), "q", []llm.RerankCandidate{{ID: "a"}, {ID: "b"}}); err == nil {
		t.Fatal("want error on 500, got nil")
	}
}
