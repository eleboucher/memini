package rerank

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type rerankRes struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
}

func TestCrossEncoderRerankOrdersByScore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" {
			t.Errorf("path = %q, want /rerank", r.URL.Path)
		}
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
	want := []string{"b", "c", "a"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestCrossEncoderDropsOmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"index":2,"relevance_score":0.9}]}`))
	}))
	defer srv.Close()

	ce, _ := New(Config{BaseURL: srv.URL})
	got, err := ce.Rerank(context.Background(), "q", []Candidate{{ID: "a"}, {ID: "b"}, {ID: "c"}})
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(got) != 1 || got[0] != "c" {
		t.Fatalf("got %v, want [c]", got)
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
	if _, err := ce.Rerank(context.Background(), "q",
		[]Candidate{{ID: "a", Content: "abcdefghij"}, {ID: "b", Content: "ok"}}); err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if got.Documents[0] != "abcde" {
		t.Errorf("doc[0] = %q, want %q", got.Documents[0], "abcde")
	}
	if got.Documents[1] != "ok" {
		t.Errorf("doc[1] = %q, want %q", got.Documents[1], "ok")
	}
}

func TestCrossEncoderCapsResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		giant := strings.Repeat("A", maxRerankBodyBytes+1024)
		_, _ = io.WriteString(w, `{"results":[{"index":0,"relevance_score":0.1,"pad":"`+giant+`"}]}`)
	}))
	defer srv.Close()

	ce, _ := New(Config{BaseURL: srv.URL})
	if _, err := ce.Rerank(context.Background(), "q", []Candidate{{ID: "a"}, {ID: "b"}}); err == nil {
		t.Fatal("want a decode error from the truncated (capped) body, got nil")
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

// TestCrossEncoderErrorsOn200WithErrorBody covers servers that answer an
// unimplemented /rerank route with HTTP 200 and {"error": "..."} (e.g. LM
// Studio) — the status check passes, so without inspecting the error field this
// would be misreported as "empty results" and silently degrade to composite order.
func TestCrossEncoderErrorsOn200WithErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"Unexpected endpoint or method. (POST /v1/rerank)"}`))
	}))
	defer srv.Close()

	ce, _ := New(Config{BaseURL: srv.URL})
	_, err := ce.Rerank(context.Background(), "q", []Candidate{{ID: "a"}, {ID: "b"}})
	if err == nil {
		t.Fatal("want error on 200-with-error-body, got nil")
	}
	if !strings.Contains(err.Error(), "Unexpected endpoint") {
		t.Fatalf("error should surface the server message, got: %v", err)
	}
}

func TestCrossEncoderBatchesAndMergesByScore(t *testing.T) {
	var got [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rerankRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		got = append(got, append([]string(nil), req.Documents...))
		results := make([]rerankRes, len(req.Documents))
		for i := range req.Documents {
			results[i] = rerankRes{Index: i, RelevanceScore: 1.0 - float64(i)*0.1}
		}
		_ = json.NewEncoder(w).Encode(struct {
			Results []rerankRes `json:"results"`
		}{Results: results})
	}))
	defer srv.Close()

	ce, err := New(Config{BaseURL: srv.URL, MaxBatchChars: 250})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	doc100 := strings.Repeat("x", 100)
	cands := []Candidate{
		{ID: "a", Content: doc100},
		{ID: "b", Content: doc100},
		{ID: "c", Content: doc100},
		{ID: "d", Content: doc100},
	}
	gotIDs, err := ce.Rerank(context.Background(), strings.Repeat("q", 50), cands)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 batched requests, got %d", len(got))
	}
	seen := map[string]bool{}
	for _, id := range gotIDs {
		seen[id] = true
	}
	for _, want := range []string{"a", "b", "c", "d"} {
		if !seen[want] {
			t.Errorf("missing %q in merged output %v", want, gotIDs)
		}
	}
}

func TestCrossEncoderBatchScoresMergeAcrossBatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rerankRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		letterScore := func(s string) float64 {
			switch s[0] {
			case 'a':
				return 0.3
			case 'b':
				return 0.5
			case 'c':
				return 0.9
			case 'd':
				return 0.7
			}
			return 0
		}
		results := make([]rerankRes, len(req.Documents))
		for i := range req.Documents {
			results[i] = rerankRes{Index: i, RelevanceScore: letterScore(req.Documents[i])}
		}
		_ = json.NewEncoder(w).Encode(struct {
			Results []rerankRes `json:"results"`
		}{Results: results})
	}))
	defer srv.Close()

	ce, _ := New(Config{BaseURL: srv.URL, MaxBatchChars: 250})
	doc100 := strings.Repeat("x", 100)
	cands := []Candidate{
		{ID: "a", Content: "a" + doc100},
		{ID: "b", Content: "b" + doc100},
		{ID: "c", Content: "c" + doc100},
		{ID: "d", Content: "d" + doc100},
	}
	got, err := ce.Rerank(context.Background(), "q", cands)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	want := []string{"c", "d", "b", "a"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

func TestCrossEncoderSingleBatchSkipsSplit(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"results":[
			{"index":0,"relevance_score":0.5},
			{"index":1,"relevance_score":0.9}
		]}`))
	}))
	defer srv.Close()

	ce, _ := New(Config{BaseURL: srv.URL, MaxBatchChars: 1000})
	got, err := ce.Rerank(context.Background(), "q", []Candidate{
		{ID: "a", Content: "small"},
		{ID: "b", Content: "small"},
	})
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if calls != 1 {
		t.Errorf("got %d HTTP calls, want 1", calls)
	}
	if len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Errorf("order = %v, want [b a]", got)
	}
}

func TestCrossEncoderBatchErrorPropagates(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":0.5}]}`))
			return
		}
		http.Error(w, "boom", http.StatusBadRequest)
	}))
	defer srv.Close()

	ce, _ := New(Config{BaseURL: srv.URL, MaxBatchChars: 250})
	doc100 := strings.Repeat("x", 100)
	cands := []Candidate{
		{ID: "a", Content: doc100},
		{ID: "b", Content: doc100},
		{ID: "c", Content: doc100},
	}
	if _, err := ce.Rerank(context.Background(), "q", cands); err == nil {
		t.Fatal("want error when a batch fails, got nil")
	}
}
