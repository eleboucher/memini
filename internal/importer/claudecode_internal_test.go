package importer

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/eleboucher/memini/internal/config"
)

// TestNSResolverMemoizes: the resolver runs the (expensive, git-shelling) fn at
// most once per distinct cwd, and returns "" for an empty cwd without calling fn.
func TestNSResolverMemoizes(t *testing.T) {
	var calls atomic.Int32
	r := &nsResolver{
		cache:   map[string]string{},
		demoted: map[string]string{},
		fn: func(cwd string) (string, config.NamespaceSource) {
			calls.Add(1)
			return "ns-for-" + cwd, config.NamespaceFromGitRemote
		},
	}

	if got := r.resolve(""); got != "" {
		t.Fatalf("empty cwd should resolve to %q, got %q", "", got)
	}
	for range 5 {
		if got := r.resolve("/proj/a"); got != "ns-for-/proj/a" {
			t.Fatalf("resolve = %q", got)
		}
	}
	r.resolve("/proj/b")
	if calls.Load() != 2 {
		t.Fatalf("fn called %d times, want 2 (once per distinct cwd, never for empty)", calls.Load())
	}
}

// TestNSResolverRecordsDemotion: a cwd resolved without the git-remote step is
// recorded so the backfill can warn that history may have landed off-namespace.
func TestNSResolverRecordsDemotion(t *testing.T) {
	r := &nsResolver{
		cache:   map[string]string{},
		demoted: map[string]string{},
		fn: func(cwd string) (string, config.NamespaceSource) {
			return "dirbase", config.NamespaceFromCWD // git failed → basename
		},
	}
	r.resolve("/gone/project")
	w := r.warnings()
	if len(w) != 1 {
		t.Fatalf("want one demotion warning, got %v", w)
	}
}

// TestNSResolverConcurrent exercises the lock under the parallel directory walk:
// many goroutines hammering shared cwds must still call fn once per distinct cwd.
// Run with -race.
func TestNSResolverConcurrent(t *testing.T) {
	var calls atomic.Int32
	r := &nsResolver{
		cache:   map[string]string{},
		demoted: map[string]string{},
		fn: func(cwd string) (string, config.NamespaceSource) {
			calls.Add(1)
			return "ns-" + cwd, config.NamespaceFromGitRemote
		},
	}
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.resolve("/shared/proj")
			r.resolve("/other/proj")
		}()
	}
	wg.Wait()
	if calls.Load() != 2 {
		t.Fatalf("fn called %d times across 50 goroutines, want 2 distinct cwds", calls.Load())
	}
}

// TestParseClaudeCodeDropsErrorOnlyResponses: an exchange whose assistant turn
// is just a transient API error carries no knowledge and must not become a
// memory, while a normal exchange in the same transcript is kept.
func TestParseClaudeCodeDropsErrorOnlyResponses(t *testing.T) {
	jsonl := `{"type":"user","sessionId":"s1","message":{"content":"continue"}}
{"type":"assistant","sessionId":"s1","message":{"content":[{"type":"text","text":"API Error: 500 Internal server error. This is a server-side issue, usually transient."}]}}
{"type":"user","sessionId":"s1","message":{"content":"explain the cache layer"}}
{"type":"assistant","sessionId":"s1","message":{"content":[{"type":"text","text":"The cache is a write-through LRU in front of Postgres."}]}}
`
	recs, err := parseClaudeCode([]byte(jsonl))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1 (error-only exchange dropped)", len(recs))
	}
	if !strings.Contains(recs[0].Content, "cache is a write-through LRU") {
		t.Fatalf("kept the wrong exchange: %q", recs[0].Content)
	}
}

func TestIsCommandNoise(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"<task-notification> <task-id>abc</task-id>", true},
		{"<local-command-stdout></local-command-stdout>", true},
		{"<command-name>/clear</command-name>", true},
		{"explain the cache layer", false},
	} {
		if got := isCommandNoise(tc.text); got != tc.want {
			t.Errorf("isCommandNoise(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestIsErrorOnlyResponse(t *testing.T) {
	for _, tc := range []struct {
		asst string
		want bool
	}{
		{"API Error: 500 Internal server error", true},
		{"  API Error: overloaded", true},
		{"The API Error you saw was caused by a missing token; here's the fix.", false},
		{"Done — applied the fix.", false},
	} {
		if got := isErrorOnlyResponse(tc.asst); got != tc.want {
			t.Errorf("isErrorOnlyResponse(%q) = %v, want %v", tc.asst, got, tc.want)
		}
	}
}

func TestStripNoise(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"system-reminder block removed, prose kept",
			"fix the auth bug\n<system-reminder>\nbackground context\n</system-reminder>",
			"fix the auth bug"},
		{"memini-context briefing removed",
			"<memini-context project=\"x\">\n- Pinned: foo\n</memini-context>\nwhat changed?",
			"what changed?"},
		{"inline mention preserved (not line-anchored tag)",
			"explain what a <system-reminder> tag is used for",
			"explain what a <system-reminder> tag is used for"},
		{"hook-run chrome removed",
			"done\nRan 2 Stop hooks\n",
			"done"},
		{"pure noise becomes empty",
			"<system-reminder>only noise</system-reminder>",
			""},
		{"clean prose untouched",
			"just a normal message",
			"just a normal message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripNoise(tc.in); got != tc.want {
				t.Errorf("stripNoise(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseClaudeCodeStripsEmbeddedNoise: a real exchange whose user turn has an
// appended system-reminder is kept, with the reminder stripped from storage.
func TestParseClaudeCodeStripsEmbeddedNoise(t *testing.T) {
	jsonl := `{"type":"user","sessionId":"s1","message":{"content":"refactor the parser\n<system-reminder>injected ctx</system-reminder>"}}
{"type":"assistant","sessionId":"s1","message":{"content":[{"type":"text","text":"Done — split it into two functions."}]}}
`
	recs, err := parseClaudeCode([]byte(jsonl))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if strings.Contains(recs[0].Content, "system-reminder") || strings.Contains(recs[0].Content, "injected ctx") {
		t.Errorf("reminder not stripped: %q", recs[0].Content)
	}
	if !strings.Contains(recs[0].Content, "refactor the parser") {
		t.Errorf("real prompt lost: %q", recs[0].Content)
	}
}
