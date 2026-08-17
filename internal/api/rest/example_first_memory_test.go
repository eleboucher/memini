// Validates docs/examples/first-memory.md.
//
// From zero to a remembered fact over the REST API: an empty server lists no
// namespaces; the first POST /v1/memories with the tier omitted comes back
// 201 with an auto-classified tier and materializes the namespace; the
// namespace then appears in GET /v1/namespaces; and POST /v1/search returns
// the fact. Every response shape the doc quotes is asserted here.
package rest_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestExampleFirstMemory(t *testing.T) {
	h := newServer(t)
	const ns = "acme/checkout"
	const fact = "We decided to use Stripe webhooks instead of polling for payment " +
		"status, the reason is polling kept tripping the rate limits."

	// A fresh server knows no namespaces: they are never created explicitly,
	// they exist exactly while a memory carries the name.
	rec := do(t, h, http.MethodGet, "/v1/namespaces", "", apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list namespaces (empty): want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var before struct {
		Namespaces []string `json:"namespaces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &before); err != nil {
		t.Fatalf("decode namespaces: %v", err)
	}
	if len(before.Namespaces) != 0 {
		t.Fatalf("fresh server should list no namespaces, got %v", before.Namespaces)
	}

	// The first remember, tier omitted: the server classifies the decision as
	// a durable semantic fact and stamps the classification for audit.
	rec = do(t, h, http.MethodPost, "/v1/memories", ns, apiKey, map[string]any{
		"content": fact,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var stored struct {
		Id        string         `json:"id"`
		Namespace string         `json:"namespace"`
		Tier      string         `json:"tier"`
		Content   string         `json:"content"`
		Metadata  map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatalf("decode remember response: %v", err)
	}
	if stored.Id == "" {
		t.Fatal("201 response must carry the new memory's id")
	}
	if stored.Namespace != ns {
		t.Fatalf("namespace = %q, want %q (from the X-Memini-Namespace header)", stored.Namespace, ns)
	}
	if stored.Tier != "semantic" {
		t.Fatalf("tier = %q, want the auto-classified %q", stored.Tier, "semantic")
	}
	if v, _ := stored.Metadata["tier_classified"].(string); v != "marker" {
		t.Fatalf("metadata.tier_classified = %v, want %q", stored.Metadata["tier_classified"], "marker")
	}
	if stored.Content != fact {
		t.Fatalf("content round-trip: got %q", stored.Content)
	}

	// That single write materialized the namespace.
	rec = do(t, h, http.MethodGet, "/v1/namespaces", "", apiKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list namespaces: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var after struct {
		Namespaces []string `json:"namespaces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode namespaces: %v", err)
	}
	if len(after.Namespaces) != 1 || after.Namespaces[0] != ns {
		t.Fatalf("namespaces after the first write = %v, want [%q]", after.Namespaces, ns)
	}

	// Recall over REST returns the fact.
	rec = do(t, h, http.MethodPost, "/v1/search", ns, apiKey, map[string]any{
		"query": "webhooks or polling for payment status", "limit": 5,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("search: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var found struct {
		Results []struct {
			Memory struct {
				Id      string `json:"id"`
				Content string `json:"content"`
			} `json:"memory"`
			Score float64 `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &found); err != nil {
		t.Fatalf("decode search response: %v", err)
	}
	if len(found.Results) == 0 {
		t.Fatal("search should return the remembered fact")
	}
	top := found.Results[0]
	if top.Memory.Id != stored.Id || top.Memory.Content != fact {
		t.Fatalf("top search hit = %q (%q), want the stored fact %q", top.Memory.Id, top.Memory.Content, stored.Id)
	}
	if top.Score <= 0 {
		t.Fatalf("search hit score = %v, want > 0", top.Score)
	}
}
