package rest_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// updateNS is the namespace every PATCH test seeds into. Kept as one constant so
// the seed helper and the request paths cannot drift apart.
const updateNS = "alice"

// apiMemoryBody is the subset of the Memory response these tests assert on.
type apiMemoryBody struct {
	ID       string         `json:"id"`
	Content  string         `json:"content"`
	Summary  string         `json:"summary"`
	Tier     string         `json:"tier"`
	Tags     []string       `json:"tags"`
	Metadata map[string]any `json:"metadata"`
}

// seedMemory stores one memory over REST and returns its id.
func seedMemory(t *testing.T, h http.Handler, body map[string]any) string {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/v1/memories", updateNS, apiKey, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var out apiMemoryBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode seed: %v", err)
	}
	return out.ID
}

func decodeMemory(t *testing.T, body []byte) apiMemoryBody {
	t.Helper()
	var out apiMemoryBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode memory: %v", err)
	}
	return out
}

// patchMemory issues a PATCH against a seeded id.
func patchMemory(t *testing.T, h http.Handler, id string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, h, http.MethodPatch, "/v1/memories/"+id, updateNS, apiKey, body)
}

// TestPatchMemoryPartialUpdate: the fields the body omits must survive.
func TestPatchMemoryPartialUpdate(t *testing.T) {
	h := newServer(t)
	id := seedMemory(t, h, map[string]any{
		"content": "the deploy runbook lives in the ops wiki", "summary": "runbook location",
		"tier": "semantic", "tags": []string{"ops"},
	})

	rec := patchMemory(t, h, id, map[string]any{"tags": []string{"ops", "runbook"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	got := decodeMemory(t, rec.Body.Bytes())
	if got.Content != "the deploy runbook lives in the ops wiki" {
		t.Fatalf("Content = %q, want the stored content kept", got.Content)
	}
	if got.Summary != "runbook location" {
		t.Fatalf("Summary = %q, want the stored summary kept", got.Summary)
	}
	if len(got.Tags) != 2 {
		t.Fatalf("Tags = %v, want the update applied", got.Tags)
	}
}

// TestPatchMemoryMergesMetadata pins the divergence from POST-with-an-id: PATCH
// merges key-by-key where the upsert replaces wholesale. Getting this backwards
// silently destroys metadata for anyone scripting partial edits.
func TestPatchMemoryMergesMetadata(t *testing.T) {
	h := newServer(t)
	id := seedMemory(t, h, map[string]any{
		"content": "the deploy runbook lives in the ops wiki", "tier": "semantic",
		"metadata": map[string]any{"source": "handbook", "reviewed": "no"},
	})

	rec := patchMemory(t, h, id, map[string]any{"metadata": map[string]any{"reviewed": "yes"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	got := decodeMemory(t, rec.Body.Bytes())
	if got.Metadata["source"] != "handbook" {
		t.Fatalf("Metadata[source] = %v, want preserved by the merge (a wholesale replace would drop it)",
			got.Metadata["source"])
	}
	if got.Metadata["reviewed"] != "yes" {
		t.Fatalf("Metadata[reviewed] = %v, want overwritten", got.Metadata["reviewed"])
	}
}

// TestPatchMemoryNullMetadataValueDeletesKey covers the RFC 7386 delete.
func TestPatchMemoryNullMetadataValueDeletesKey(t *testing.T) {
	h := newServer(t)
	id := seedMemory(t, h, map[string]any{
		"content": "the deploy runbook lives in the ops wiki", "tier": "semantic",
		"metadata": map[string]any{"source": "handbook", "reviewed": "no"},
	})

	rec := patchMemory(t, h, id, map[string]any{"metadata": map[string]any{"reviewed": nil}})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	got := decodeMemory(t, rec.Body.Bytes())
	if _, ok := got.Metadata["reviewed"]; ok {
		t.Fatalf("Metadata[reviewed] = %v, want deleted by an explicit null", got.Metadata["reviewed"])
	}
	if got.Metadata["source"] != "handbook" {
		t.Fatalf("Metadata[source] = %v, want the other keys untouched", got.Metadata["source"])
	}
}

// TestPatchMemoryUnknownID must be a 404, not a 500.
func TestPatchMemoryUnknownID(t *testing.T) {
	h := newServer(t)
	rec := patchMemory(t, h, "does-not-exist", map[string]any{"summary": "irrelevant"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("patch unknown id: want 404, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestPatchMemoryBadTierIsBadRequest is the regression guard on error CLASS.
// service.Update must return ErrInvalidInput for a bad tier: statusFor maps that
// to 400 and anything unrecognized to 500, so a bare fmt.Errorf would report a
// caller's typo as a server fault. Verified by mutation: swapping invalidInputf
// for fmt.Errorf turns this into a 500.
func TestPatchMemoryBadTierIsBadRequest(t *testing.T) {
	h := newServer(t)
	id := seedMemory(t, h, map[string]any{
		"content": "the deploy runbook lives in the ops wiki", "tier": "semantic",
	})

	rec := patchMemory(t, h, id, map[string]any{"tier": "nonsense"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch with a bad tier: want 400, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestPatchMemoryClearsSummary: an explicit empty string is a write, not an
// omission — the behaviour the old ""-sentinel form could not express.
func TestPatchMemoryClearsSummary(t *testing.T) {
	h := newServer(t)
	id := seedMemory(t, h, map[string]any{
		"content": "the deploy runbook lives in the ops wiki", "summary": "runbook location",
		"tier": "semantic",
	})

	rec := patchMemory(t, h, id, map[string]any{"summary": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	if got := decodeMemory(t, rec.Body.Bytes()); got.Summary != "" {
		t.Fatalf("Summary = %q, want cleared by an explicit empty string", got.Summary)
	}
}
