package rest_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

// wantHash is the wire content_hash recipe: first 16 hex chars of sha256 over
// content, falling back to summary only when content is empty — mirroring the
// plugin client's injectedIdentity so the test pins cross-stack agreement,
// not just server self-consistency.
func wantHash(content, summary string) string {
	text := content
	if text == "" {
		text = summary
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:16]
}

// searchResult is the slice of the /v1/search wire shape these tests care
// about.
type searchResult struct {
	Results []struct {
		Memory struct {
			Id               string  `json:"id"`
			Content          string  `json:"content"`
			ContentHash      *string `json:"content_hash"`
			ContentTruncated *bool   `json:"content_truncated"`
			Summary          *string `json:"summary"`
		} `json:"memory"`
	} `json:"results"`
}

// TestSearchResponseFormat pins the /v1/search progressive-disclosure
// contract: detailed (and absent) format returns full content; concise
// replaces content with the summary or a ≤240-rune boundary cut marked
// content_truncated; content_hash is present in BOTH formats, equal across
// them, and always computed over the full stored content.
func TestSearchResponseFormat(t *testing.T) {
	h := newServer(t)
	const ns = "alice"

	longContent := strings.TrimSpace(strings.Repeat("the deployment pipeline runs its checks across many cores ", 10))
	summaryContent := strings.TrimSpace(strings.Repeat("every retry backs off exponentially before the breaker opens ", 10))
	const shortContent = "the deploy key lives in vault"
	const theSummary = "retries back off exponentially"

	remember := func(id, content, summary string) {
		body := map[string]any{"content": content, "tier": "semantic", "id": id}
		if summary != "" {
			body["summary"] = summary
		}
		if rec := do(t, h, http.MethodPost, "/v1/memories", ns, apiKey, body); rec.Code != http.StatusCreated {
			t.Fatalf("remember %s: want 201, got %d (%s)", id, rec.Code, rec.Body)
		}
	}
	remember("long-1", longContent, "")
	remember("summary-1", summaryContent, theSummary)
	remember("short-1", shortContent, "")

	search := func(format string) searchResult {
		body := map[string]any{"query": "deployment pipeline retry breaker deploy key vault", "limit": 10}
		if format != "" {
			body["response_format"] = format
		}
		rec := do(t, h, http.MethodPost, "/v1/search", ns, apiKey, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("search format=%q: want 200, got %d (%s)", format, rec.Code, rec.Body)
		}
		var out searchResult
		mustJSON(t, rec, &out)
		return out
	}
	byID := func(out searchResult) map[string]struct {
		Content   string
		Hash      *string
		Truncated *bool
	} {
		m := make(map[string]struct {
			Content   string
			Hash      *string
			Truncated *bool
		})
		for _, r := range out.Results {
			m[r.Memory.Id] = struct {
				Content   string
				Hash      *string
				Truncated *bool
			}{r.Memory.Content, r.Memory.ContentHash, r.Memory.ContentTruncated}
		}
		return m
	}

	for _, format := range []string{"", "detailed"} {
		got := byID(search(format))
		if len(got) < 3 {
			t.Fatalf("format=%q: want all 3 memories, got %v", format, got)
		}
		if got["long-1"].Content != longContent {
			t.Fatalf("format=%q must return full content, got %d chars", format, len(got["long-1"].Content))
		}
		for id := range got {
			if got[id].Truncated != nil {
				t.Fatalf("format=%q: content_truncated must be absent, set on %s", format, id)
			}
			if got[id].Hash == nil {
				t.Fatalf("format=%q: content_hash missing on %s", format, id)
			}
		}
		if *got["long-1"].Hash != wantHash(longContent, "") {
			t.Fatalf("content_hash recipe mismatch: got %q, want %q", *got["long-1"].Hash, wantHash(longContent, ""))
		}
		// Content wins over summary in the hash input.
		if *got["summary-1"].Hash != wantHash(summaryContent, theSummary) {
			t.Fatalf("summary memory hash = %q, want over-content %q", *got["summary-1"].Hash, wantHash(summaryContent, theSummary))
		}
	}

	got := byID(search("concise"))
	// Long content: boundary cut ≤240 runes + ellipsis, marked truncated.
	long := got["long-1"]
	if !strings.HasSuffix(long.Content, "…") || len([]rune(long.Content)) > 241 {
		t.Fatalf("concise long content = %d runes (%q…), want ≤241 ending with …", len([]rune(long.Content)), long.Content[:40])
	}
	if strings.HasSuffix(strings.TrimSuffix(long.Content, "…"), "cor") {
		t.Fatalf("concise cut landed mid-word: %q", long.Content)
	}
	if long.Truncated == nil || !*long.Truncated {
		t.Fatal("concise long content must set content_truncated=true")
	}
	// Summary memory: summary verbatim, NOT marked truncated.
	if got["summary-1"].Content != theSummary || got["summary-1"].Truncated != nil {
		t.Fatalf("concise summary item = (%q, %v), want summary verbatim and no content_truncated",
			got["summary-1"].Content, got["summary-1"].Truncated)
	}
	// Short content: verbatim, not marked.
	if got["short-1"].Content != shortContent || got["short-1"].Truncated != nil {
		t.Fatalf("concise short item = (%q, %v), want verbatim", got["short-1"].Content, got["short-1"].Truncated)
	}
	// content_hash is identical across formats: computed over the FULL stored
	// content, never the concise text.
	if *got["long-1"].Hash != wantHash(longContent, "") {
		t.Fatalf("concise content_hash = %q, want full-content hash %q", *got["long-1"].Hash, wantHash(longContent, ""))
	}
	if *got["summary-1"].Hash != wantHash(summaryContent, theSummary) {
		t.Fatal("concise summary item hash must still cover the full content")
	}

	// An unknown response_format is rejected, mirroring scope validation.
	rec := do(t, h, http.MethodPost, "/v1/search", ns, apiKey, map[string]any{
		"query": "x", "response_format": "compact",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid response_format: want 400, got %d (%s)", rec.Code, rec.Body)
	}
}

// briefingWire is the slice of the /v1/namespaces/briefing wire shape these
// tests care about.
type briefingWire struct {
	Facts *[]struct {
		Memory struct {
			Id               string  `json:"id"`
			Content          string  `json:"content"`
			ContentHash      *string `json:"content_hash"`
			ContentTruncated *bool   `json:"content_truncated"`
		} `json:"memory"`
	} `json:"facts"`
	Children *[]struct {
		Namespace string `json:"namespace"`
		Total     int    `json:"total"`
		Pinned    *[]struct {
			Id               string  `json:"id"`
			Content          string  `json:"content"`
			ContentHash      *string `json:"content_hash"`
			ContentTruncated *bool   `json:"content_truncated"`
		} `json:"pinned"`
		Recent       *[]map[string]any `json:"recent"`
		PinnedTitles *[]string         `json:"pinned_titles"`
		RecentTitles *[]string         `json:"recent_titles"`
	} `json:"children"`
}

// TestBriefingFormat pins the briefing's format param: detailed (default)
// carries full content, concise caps items at 280 runes with the boundary
// cut and content_truncated marker, and content_hash rides on items in both
// formats.
func TestBriefingFormat(t *testing.T) {
	h := newServer(t)
	const ns = "alice"

	longContent := strings.TrimSpace(strings.Repeat("the ingestion worker drains the queue before rotating its lease ", 10))
	if rec := do(t, h, http.MethodPost, "/v1/memories", ns, apiKey, map[string]any{
		"content": longContent, "tier": "semantic", "id": "brief-long-1",
	}); rec.Code != http.StatusCreated {
		t.Fatalf("remember: %d (%s)", rec.Code, rec.Body)
	}

	brief := func(query string) briefingWire {
		rec := do(t, h, http.MethodGet, "/v1/namespaces/briefing"+query, ns, apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("briefing %q: want 200, got %d (%s)", query, rec.Code, rec.Body)
		}
		var out briefingWire
		mustJSON(t, rec, &out)
		return out
	}

	// Default (and explicit detailed): full content, hash present, no marker.
	for _, q := range []string{"", "?format=detailed"} {
		b := brief(q)
		if b.Facts == nil || len(*b.Facts) == 0 {
			t.Fatalf("briefing %q returned no facts", q)
		}
		item := (*b.Facts)[0]
		if item.Memory.Content != longContent {
			t.Fatalf("briefing %q must carry full content, got %d chars", q, len(item.Memory.Content))
		}
		if item.Memory.ContentTruncated != nil {
			t.Fatalf("briefing %q: content_truncated must be absent", q)
		}
		if item.Memory.ContentHash == nil || *item.Memory.ContentHash != wantHash(longContent, "") {
			t.Fatalf("briefing %q: content_hash = %v, want %q", q, item.Memory.ContentHash, wantHash(longContent, ""))
		}
	}

	// Concise: 280-rune boundary cut, marked, hash unchanged.
	b := brief("?format=concise")
	item := (*b.Facts)[0]
	if !strings.HasSuffix(item.Memory.Content, "…") || len([]rune(item.Memory.Content)) > 281 {
		t.Fatalf("concise briefing item = %d runes, want ≤281 ending with …", len([]rune(item.Memory.Content)))
	}
	if item.Memory.ContentTruncated == nil || !*item.Memory.ContentTruncated {
		t.Fatal("concise briefing cut must set content_truncated")
	}
	if item.Memory.ContentHash == nil || *item.Memory.ContentHash != wantHash(longContent, "") {
		t.Fatal("concise briefing item hash must cover the full stored content")
	}

	// An unknown format is rejected.
	if rec := do(t, h, http.MethodGet, "/v1/namespaces/briefing?format=compact", ns, apiKey, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid format: want 400, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestBriefingChildrenModes pins the children query param: full (default)
// ships complete memory objects, summary ships {namespace, total,
// pinned_titles, recent_titles} only, and none omits the rollup.
func TestBriefingChildrenModes(t *testing.T) {
	h := newServer(t)
	const parent = "acme"
	const child = "acme/app"

	longChildContent := strings.TrimSpace(strings.Repeat("the app child namespace holds its own facts ", 5))
	for id, body := range map[string]map[string]any{
		"child-pin-1": {"content": longChildContent, "tier": "semantic", "tags": []string{"pinned"}},
		"child-mem-1": {"content": "a plain durable fact in the child", "tier": "semantic"},
	} {
		body["id"] = id
		if rec := do(t, h, http.MethodPost, "/v1/memories", child, apiKey, body); rec.Code != http.StatusCreated {
			t.Fatalf("remember %s: %d (%s)", id, rec.Code, rec.Body)
		}
	}

	brief := func(query string) briefingWire {
		rec := do(t, h, http.MethodGet, "/v1/namespaces/briefing"+query, parent, apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("briefing %q: want 200, got %d (%s)", query, rec.Code, rec.Body)
		}
		var out briefingWire
		mustJSON(t, rec, &out)
		return out
	}

	// Default (and explicit full): complete memory objects with content_hash.
	for _, q := range []string{"", "?children=full"} {
		b := brief(q)
		if b.Children == nil || len(*b.Children) != 1 {
			t.Fatalf("children %q: want the one child rollup, got %+v", q, b.Children)
		}
		c := (*b.Children)[0]
		if c.Namespace != child || c.Total != 2 {
			t.Fatalf("children %q: rollup = %s/%d, want %s/2", q, c.Namespace, c.Total, child)
		}
		if c.Pinned == nil || len(*c.Pinned) != 1 || (*c.Pinned)[0].Content != longChildContent {
			t.Fatalf("children %q: want the full pinned memory object, got %+v", q, c.Pinned)
		}
		if (*c.Pinned)[0].ContentHash == nil {
			t.Fatalf("children %q: child memory missing content_hash", q)
		}
		if c.PinnedTitles != nil || c.RecentTitles != nil {
			t.Fatalf("children %q: title fields must be absent in full mode", q)
		}
	}

	// summary: titles/counts only — no memory objects at all.
	b := brief("?children=summary")
	if b.Children == nil || len(*b.Children) != 1 {
		t.Fatalf("children=summary: want the one child rollup, got %+v", b.Children)
	}
	c := (*b.Children)[0]
	if c.Namespace != child || c.Total != 2 {
		t.Fatalf("children=summary rollup = %s/%d, want %s/2", c.Namespace, c.Total, child)
	}
	if c.Pinned != nil || c.Recent != nil {
		t.Fatal("children=summary must not ship memory objects")
	}
	if c.PinnedTitles == nil || len(*c.PinnedTitles) != 1 {
		t.Fatalf("children=summary: want 1 pinned title, got %v", c.PinnedTitles)
	}
	title := (*c.PinnedTitles)[0]
	if !strings.HasSuffix(title, "…") || len([]rune(title)) > 61 {
		t.Fatalf("pinned title = %q (%d runes), want a ≤61-rune cut ending with …", title, len([]rune(title)))
	}

	// none: the rollup is omitted entirely.
	if b = brief("?children=none"); b.Children != nil {
		t.Fatalf("children=none must omit the rollup, got %+v", b.Children)
	}

	// An unknown mode is rejected.
	if rec := do(t, h, http.MethodGet, "/v1/namespaces/briefing?children=collapsed", parent, apiKey, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid children: want 400, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestGetMemoryIDPrefix pins the REST short-id contract on GET
// /v1/memories/{id}: a unique ≥8-hex-char prefix resolves, an ambiguous one
// is a 409 listing the colliding full ids, a short prefix is a plain 404,
// and mutations never resolve prefixes.
func TestGetMemoryIDPrefix(t *testing.T) {
	h := newServer(t)
	const ns = "alice"
	ids := []string{
		"deadbeef-1111-4000-8000-000000000001",
		"deadbeef-2222-4000-8000-000000000002",
		"cafef00d-aaaa-4000-8000-000000000003",
	}
	for i, id := range ids {
		if rec := do(t, h, http.MethodPost, "/v1/memories", ns, apiKey, map[string]any{
			"content": "prefix fixture number " + string(rune('a'+i)), "tier": "semantic", "id": id,
		}); rec.Code != http.StatusCreated {
			t.Fatalf("remember %s: %d (%s)", id, rec.Code, rec.Body)
		}
	}

	// Unique prefix resolves; the full id still works.
	for _, path := range []string{"/v1/memories/cafef00d", "/v1/memories/" + ids[2]} {
		rec := do(t, h, http.MethodGet, path, ns, apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: want 200, got %d (%s)", path, rec.Code, rec.Body)
		}
		var m struct {
			Id string `json:"id"`
		}
		mustJSON(t, rec, &m)
		if m.Id != ids[2] {
			t.Fatalf("GET %s resolved %q, want %q", path, m.Id, ids[2])
		}
	}

	// Ambiguous prefix: 409 listing both colliding full ids.
	rec := do(t, h, http.MethodGet, "/v1/memories/deadbeef", ns, apiKey, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("ambiguous prefix: want 409, got %d (%s)", rec.Code, rec.Body)
	}
	var errBody struct {
		Error string `json:"error"`
	}
	mustJSON(t, rec, &errBody)
	for _, id := range ids[:2] {
		if !strings.Contains(errBody.Error, id) {
			t.Fatalf("409 body must list %s, got %q", id, errBody.Error)
		}
	}

	// A prefix under 8 hex chars is never resolved: plain 404.
	if rec := do(t, h, http.MethodGet, "/v1/memories/cafef00", ns, apiKey, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("short prefix: want 404, got %d (%s)", rec.Code, rec.Body)
	}

	// Mutations stay verbatim-full-id-only: the same unique prefix 404s.
	if rec := do(t, h, http.MethodDelete, "/v1/memories/cafef00d", ns, apiKey, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE by prefix: want 404, got %d (%s)", rec.Code, rec.Body)
	}
	if rec := do(t, h, http.MethodPatch, "/v1/memories/cafef00d", ns, apiKey, map[string]any{
		"content": "must not apply",
	}); rec.Code != http.StatusNotFound {
		t.Fatalf("PATCH by prefix: want 404, got %d (%s)", rec.Code, rec.Body)
	}
}
