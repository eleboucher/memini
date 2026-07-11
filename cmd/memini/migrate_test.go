package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/eleboucher/memini/internal/maintenance"
)

// TestPrintScopesReportGlobalNamespaceInstructions pins T11's requirement
// that a set MEMINI_GLOBAL_NAMESPACE prints adoption instructions (both the
// single-operator MEMINI_HOME path and the team-wide `memini link add`
// path) rather than silently rewriting anything.
func TestPrintScopesReportGlobalNamespaceInstructions(t *testing.T) {
	var buf bytes.Buffer
	printScopesReport(&buf, maintenance.ScopesReport{GlobalNamespaceEnv: "shared/golang"})
	out := buf.String()
	for _, want := range []string{
		"MEMINI_GLOBAL_NAMESPACE", "shared/golang",
		"MEMINI_HOME=", "memini link add",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q; got:\n%s", want, out)
		}
	}
}

// TestPrintScopesReportNothingToDo covers the idempotent no-op case: no
// merges and no bare _shared namespaces prints a clean "nothing to do".
func TestPrintScopesReportNothingToDo(t *testing.T) {
	var buf bytes.Buffer
	printScopesReport(&buf, maintenance.ScopesReport{})
	if !strings.Contains(buf.String(), "nothing to do") {
		t.Errorf("want a nothing-to-do report, got:\n%s", buf.String())
	}
}

// TestPrintScopesReportMerges asserts the merge table names both sides of
// the merge plus the moved/dedup counts, and flags dry-run in the header.
func TestPrintScopesReportMerges(t *testing.T) {
	var buf bytes.Buffer
	printScopesReport(&buf, maintenance.ScopesReport{
		DryRun: true,
		Merges: []maintenance.ScopeMerge{
			{From: "acme/_shared", To: "acme", Moved: 3, DedupClusters: 1, DedupTombstoned: 1},
		},
		BareShared: []string{"_shared"},
	})
	out := buf.String()
	for _, want := range []string{"dry-run", "acme/_shared", "acme", "3", "_shared", "no parent to merge into"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q; got:\n%s", want, out)
		}
	}
}

// TestRunMigrateScopesRequiresEmbedEndpointToApply pins the mandatory
// post-merge dedup pass (gap G14): applying (--yes) without an embeddings
// endpoint configured must fail fast, before any data moves, rather than
// moving data and then erroring mid-dedup.
func TestRunMigrateScopesRequiresEmbedEndpointToApply(t *testing.T) {
	t.Setenv("MEMINI_EMBED_BASE_URL", "")
	t.Setenv("MEMINI_BACKEND", "sqlite")
	t.Setenv("MEMINI_SQLITE_PATH", t.TempDir()+"/m.db")
	t.Setenv("MEMINI_EMBED_DIMS", "4")

	migrateScopesYes = true
	t.Cleanup(func() { migrateScopesYes = false })

	err := runMigrateScopes(migrateScopesCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "MEMINI_EMBED_BASE_URL") {
		t.Fatalf("want an embeddings-endpoint error, got %v", err)
	}
}
