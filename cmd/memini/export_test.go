package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/importer"
	"github.com/eleboucher/memini/internal/memory"
)

// TestExportRoundTrip checks that a memory exported via toExportRecord parses
// back through `import --source memini` without losing fields, so an export is
// genuinely re-importable for backup/migration.
func TestExportRoundTrip(t *testing.T) {
	created := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 6, 2, 12, 30, 0, 0, time.UTC)
	expires := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	m := &memory.Memory{
		ID:         "m1",
		Namespace:  "demo",
		Tier:       memory.TierSemantic,
		Content:    "auth uses JWT tokens",
		Summary:    "auth design",
		Tags:       []string{"auth", "bug_fixes"},
		Metadata:   map[string]any{"category": "architecture_decisions"},
		Importance: 0.6,
		CreatedAt:  created,
		UpdatedAt:  updated,
		ExpiresAt:  &expires,
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(exportFile{Memories: []exportRecord{toExportRecord(m)}}); err != nil {
		t.Fatalf("encode: %v", err)
	}

	recs, err := importer.Parse(importer.SourceMemini, buf.Bytes())
	if err != nil {
		t.Fatalf("parse export: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	got := recs[0]
	if got.ID != m.ID || got.Namespace != m.Namespace || got.Tier != m.Tier {
		t.Errorf("identity mismatch: %+v", got)
	}
	if got.Content != m.Content || got.Summary != m.Summary || got.Importance != m.Importance {
		t.Errorf("body mismatch: %+v", got)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "auth" || got.Tags[1] != "bug_fixes" {
		t.Errorf("tags mismatch: %v", got.Tags)
	}
	if got.Metadata["category"] != "architecture_decisions" {
		t.Errorf("metadata mismatch: %v", got.Metadata)
	}
	if !got.CreatedAt.Equal(created) || !got.UpdatedAt.Equal(updated) {
		t.Errorf("timestamps mismatch: created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Errorf("expires mismatch: %v", got.ExpiresAt)
	}
}

func TestParseMetaPairs(t *testing.T) {
	got, err := parseMetaPairs([]string{"category=bug_fixes", "k=a=b"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["category"] != "bug_fixes" || got["k"] != "a=b" {
		t.Fatalf("unexpected map: %v", got)
	}
	if _, err := parseMetaPairs([]string{"noequals"}); err == nil {
		t.Fatal("expected error for missing '='")
	}
	if got := mustNil(t); got != nil {
		t.Fatalf("empty input should yield nil, got %v", got)
	}
}

func mustNil(t *testing.T) map[string]string {
	t.Helper()
	m, err := parseMetaPairs(nil)
	if err != nil {
		t.Fatalf("parse nil: %v", err)
	}
	return m
}
