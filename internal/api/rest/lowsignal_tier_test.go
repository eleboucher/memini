package rest_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestRememberLowSignalReportsEffectiveTier pins that the stored=false
// response carries the tier the write resolved to (auto-classified or
// default), mirroring the MCP tool — without it a caller that omitted the
// tier learns nothing about what the write would have been.
//
// Referenced by docs/how-it-works/write-path.md.
func TestRememberLowSignalReportsEffectiveTier(t *testing.T) {
	h := newServer(t)
	// A turn capture that is pure harness boilerplate strips to empty and is
	// dropped outright; with no tier given it resolves to "working".
	rec := do(t, h, http.MethodPost, "/v1/memories", "alice", apiKey, map[string]any{
		"content":  "<memini-context project=\"x\">noise</memini-context>",
		"metadata": map[string]any{"format": "turn"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (accepted, not stored)", rec.Code, http.StatusOK)
	}
	var out struct {
		Stored bool   `json:"stored"`
		Reason string `json:"reason"`
		Tier   string `json:"tier"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Stored || out.Reason != "low_signal" {
		t.Fatalf("body = %s, want stored=false reason=low_signal", rec.Body.String())
	}
	if out.Tier != "working" {
		t.Fatalf("tier = %q, want the resolved default %q", out.Tier, "working")
	}
}
