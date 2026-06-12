package rerank

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
	cands := []Candidate{{ID: "a"}, {ID: "b"}, {ID: "c"}}
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
	got, err := ce.Rerank(context.Background(), "q", []Candidate{{ID: "a"}, {ID: "b"}, {ID: "c"}})
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

func TestCrossEncoderTruncatesDocuments(t *testing.T) {
	var got rerankRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":0.9}]}`))
	}))
	defer srv.Close()

	ce, _ := New(Config{BaseURL: srv.URL, MaxDocChars: 5})
	long := "abcdefghij" // 10 runes, capped to 5
	if _, err := ce.Rerank(context.Background(), "q", []Candidate{{ID: "a", Content: long}, {ID: "b", Content: "ok"}}); err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if got.Documents[0] != "abcde" {
		t.Errorf("doc[0] = %q, want %q", got.Documents[0], "abcde")
	}
	if got.Documents[1] != "ok" {
		t.Errorf("doc[1] = %q, want %q", got.Documents[1], "ok")
	}
}

func TestCrossEncoderErrorsOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ce, _ := New(Config{BaseURL: srv.URL})
	if _, err := ce.Rerank(context.Background(), "q", []Candidate{{ID: "a"}, {ID: "b"}}); err == nil {
		t.Fatal("want error on 500, got nil")
	}
}
