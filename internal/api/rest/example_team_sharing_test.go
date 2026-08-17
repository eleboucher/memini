// Validates docs/examples/team-sharing.md.
//
// Two humans, one server, over REST with named file keys: alice is an
// ordinary read-write key (home alice-home, default namespace
// acme/phoenix/api) and bob-agent is a read_only key bound to the same team
// default. alice's visibility:"personal" write lands in her home namespace
// and is invisible to bob; her visibility:"phoenix" write lands in the team
// ancestor acme/phoenix, which bob sees at the default (full) scope with
// "from" provenance; bob's read_only key is refused with 403 on any write;
// and a namespace link narrows what crosses (a link restricted to
// non-crossing tiers contributes nothing — links narrow, never widen).
package rest_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestExampleTeamSharing(t *testing.T) {
	h := newServerWithFileKeys(t, "", `
keys:
  - name: alice
    secret: "tok-alice"
    home: alice-home
    default_namespace: acme/phoenix/api
  - name: bob-agent
    secret: "tok-bob"
    default_namespace: acme/phoenix/api
    read_only: true
`)

	// Every request below sends no X-Memini-Namespace header unless noted:
	// the key's default_namespace resolves the namespace, so both callers
	// work in acme/phoenix/api by default.
	type memoryJSON struct {
		Namespace string         `json:"namespace"`
		Content   string         `json:"content"`
		Metadata  map[string]any `json:"metadata"`
	}
	type searchJSON struct {
		Results []struct {
			Memory memoryJSON `json:"memory"`
			From   string     `json:"from"`
		} `json:"results"`
	}
	bobSearch := func(query string) searchJSON {
		t.Helper()
		rec := do(t, h, http.MethodPost, "/v1/search", "", "tok-bob", map[string]any{
			"query": query, "limit": 10,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("bob search %q: want 200, got %d (%s)", query, rec.Code, rec.Body)
		}
		var out searchJSON
		mustJSON(t, rec, &out)
		return out
	}

	// Stage 1: alice writes a personal note with visibility "personal". Her
	// key is bound to home alice-home, so the write lands there — not in the
	// team namespace — and attribution stamps her key name.
	rec := do(t, h, http.MethodPost, "/v1/memories", "", "tok-alice", map[string]any{
		"content":    "alice keeps her personal vault notes in ~/vault",
		"tier":       "semantic",
		"visibility": "personal",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("alice personal write: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var personal memoryJSON
	mustJSON(t, rec, &personal)
	if personal.Namespace != "alice-home" {
		t.Fatalf("visibility=personal should land in alice's key-bound home, got %q", personal.Namespace)
	}
	if personal.Metadata["author"] != "alice" {
		t.Fatalf("metadata.author = %v, want alice", personal.Metadata["author"])
	}

	// bob's full-scope read set is acme/phoenix/api + its ancestors; his key
	// has no home binding, so alice-home is unreachable and the personal
	// note is invisible to him.
	if got := bobSearch("personal vault notes"); len(got.Results) != 0 {
		t.Fatalf("bob must not see alice's personal note, got %+v", got.Results)
	}

	// Stage 2: alice shares a team fact by naming the team ancestor —
	// "phoenix" is the unambiguous last segment of acme/phoenix — so the
	// write travels up and lands there.
	rec = do(t, h, http.MethodPost, "/v1/memories", "", "tok-alice", map[string]any{
		"content":    "the phoenix team ships from the release branch every tuesday",
		"tier":       "semantic",
		"visibility": "phoenix",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("alice team write: want 201, got %d (%s)", rec.Code, rec.Body)
	}
	var team memoryJSON
	mustJSON(t, rec, &team)
	if team.Namespace != "acme/phoenix" {
		t.Fatalf("visibility=phoenix should land in acme/phoenix, got %q", team.Namespace)
	}

	// bob sees it at the default (full) scope: acme/phoenix is an ancestor
	// leg of his read set, and the hit carries "from" provenance naming it.
	got := bobSearch("release branch tuesday")
	if len(got.Results) != 1 || got.Results[0].Memory.Namespace != "acme/phoenix" {
		t.Fatalf("bob should see the team fact from the ancestor, got %+v", got.Results)
	}
	if got.Results[0].From != "acme/phoenix" {
		t.Fatalf("team hit from = %q, want acme/phoenix", got.Results[0].From)
	}

	// Stage 3: bob's read_only key is refused on any write — 403 with an
	// error body naming the credential and the reason. The gate runs before
	// the handler, so even a well-formed payload never reaches validation.
	rec = do(t, h, http.MethodPost, "/v1/memories", "", "tok-bob", map[string]any{
		"content": "bob tries to write", "tier": "semantic",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bob write: want 403, got %d (%s)", rec.Code, rec.Body)
	}
	var denial struct {
		Error string `json:"error"`
	}
	mustJSON(t, rec, &denial)
	wantDenial := `read-only credential: API key "bob-agent" has read_only=true and cannot perform mutating requests`
	if denial.Error != wantDenial {
		t.Fatalf("403 error = %q, want %q", denial.Error, wantDenial)
	}

	// Stage 4: a link narrows, never widens. alice seeds ops/runbooks (via
	// an explicit X-Memini-Namespace header) with a semantic fact, a
	// procedural how-to, and an episodic entry, then links the team's
	// working namespace to it.
	seed := func(content, tier string) {
		t.Helper()
		rec := do(t, h, http.MethodPost, "/v1/memories", "ops/runbooks", "tok-alice", map[string]any{
			"content": content, "tier": tier,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed ops/runbooks (%s): want 201, got %d (%s)", tier, rec.Code, rec.Body)
		}
	}
	seed("the incident runbook index lives in the ops wiki", "semantic")
	seed("runbook escalation: page the on-call, then open an incident channel", "procedural")
	seed("ran the quarterly runbook fire drill with the ops team this morning", "episodic")

	// A link restricted to the episodic tier contributes nothing: only
	// durable tiers ever cross a namespace boundary, and a link cannot
	// widen past that rule.
	rec = do(t, h, http.MethodPost, "/v1/links", "", "tok-alice", map[string]any{
		"dst": "ops/runbooks", "tiers": []string{"episodic"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put episodic-only link: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	for _, r := range bobSearch("runbook").Results {
		if r.Memory.Namespace == "ops/runbooks" {
			t.Fatalf("an episodic-only link must contribute nothing (links never widen), got %+v", r)
		}
	}

	// Re-putting the link with tiers ["semantic"] narrows the durable
	// default (semantic + procedural) down to semantic only: bob now sees
	// the semantic fact — annotated "link:ops/runbooks" — but not the
	// procedural how-to, and still never the episodic entry.
	rec = do(t, h, http.MethodPost, "/v1/links", "", "tok-alice", map[string]any{
		"dst": "ops/runbooks", "tiers": []string{"semantic"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put semantic-only link: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var linked []struct {
		Memory memoryJSON `json:"memory"`
		From   string     `json:"from"`
	}
	for _, r := range bobSearch("runbook").Results {
		if r.Memory.Namespace == "ops/runbooks" {
			linked = append(linked, r)
		}
	}
	if len(linked) != 1 {
		t.Fatalf("semantic-only link should surface exactly the semantic fact, got %+v", linked)
	}
	if !strings.Contains(linked[0].Memory.Content, "incident runbook index") {
		t.Fatalf("the linked hit should be the semantic fact, got %q", linked[0].Memory.Content)
	}
	if linked[0].From != "link:ops/runbooks" {
		t.Fatalf("linked hit from = %q, want link:ops/runbooks", linked[0].From)
	}
}
