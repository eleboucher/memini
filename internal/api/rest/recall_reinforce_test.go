package rest_test

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
)

func TestSearchReinforcementOptOut(t *testing.T) {
	const content = "the kubernetes scheduler assigns nodes"
	for _, tc := range []struct {
		name string
		tier string
		body map[string]any
		want int
	}{
		{"automatic episodic", "episodic", map[string]any{"query": content, "reinforce": false}, 0},
		{"automatic durable", "semantic", map[string]any{"query": content, "reinforce": false}, 0},
		{"explicit default", "episodic", map[string]any{"query": content}, 1},
		{"explicit true", "semantic", map[string]any{"query": content, "reinforce": true}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newServerWithAnswerer(t, service.WithSyncReinforce())
			rec := do(t, h, http.MethodPost, "/v1/memories", "ns", apiKey, map[string]any{
				"content": content, "tier": tc.tier, "confidence": 0.4,
			})
			if rec.Code != http.StatusCreated {
				t.Fatalf("remember: %d %s", rec.Code, rec.Body)
			}
			var original memory.Memory
			mustJSON(t, rec, &original)
			rec = do(t, h, http.MethodGet, "/v1/memories/"+original.ID, "ns", apiKey, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("get: %d %s", rec.Code, rec.Body)
			}
			mustJSON(t, rec, &original)
			rec = do(t, h, http.MethodPost, "/v1/search", "ns", apiKey, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("search: %d %s", rec.Code, rec.Body)
			}
			var results struct {
				Results []any `json:"results"`
			}
			mustJSON(t, rec, &results)
			if len(results.Results) != 1 {
				t.Fatalf("results = %v, want one hit", results.Results)
			}
			rec = do(t, h, http.MethodGet, "/v1/memories/"+original.ID, "ns", apiKey, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("get: %d %s", rec.Code, rec.Body)
			}
			var got memory.Memory
			mustJSON(t, rec, &got)
			if got.AccessCount != tc.want {
				t.Fatalf("access_count = %d, want %d", got.AccessCount, tc.want)
			}
			if tc.tier == "episodic" && (original.ExpiresAt == nil || got.ExpiresAt == nil) {
				t.Fatal("episodic capture must have expiry")
			}
			if tc.want == 0 && (!got.LastAccessedAt.Equal(original.LastAccessedAt) || !reflect.DeepEqual(got.ExpiresAt, original.ExpiresAt) || !reflect.DeepEqual(got.Confidence, original.Confidence)) {
				t.Fatalf("automatic recall changed retention: before=%+v after=%+v", original, got)
			}
		})
	}
}
