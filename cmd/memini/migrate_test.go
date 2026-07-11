package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

const migrateTestDims = 64

// seedShared upserts one memory into ns on st, embedding content with emb.
func seedShared(t *testing.T, st store.Store, emb *embedtest.Fake, ns, id, content string) {
	t.Helper()
	ctx := context.Background()
	vec, err := emb.Embed(ctx, []string{content})
	if err != nil {
		t.Fatalf("embed %s: %v", id, err)
	}
	ts := time.Now().UTC()
	m := &memory.Memory{
		ID: id, Namespace: ns, Tier: memory.TierSemantic, Content: content,
		CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts, Embedding: vec[0],
	}
	if err := st.Upsert(ctx, m); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

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
	for _, want := range []string{"(dry-run)", "acme/_shared", "acme", "3", "_shared", "no parent to merge into"} {
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

// failSecondReassignStore wraps a real store but fails Reassign into the
// given target namespace, simulating a mid-migration failure once earlier
// merges have already committed.
type failSecondReassignStore struct {
	store.Store
	failTo string
}

func (f *failSecondReassignStore) Reassign(ctx context.Context, fromNS string, ids []string, toNS string) (int64, error) {
	if toNS == f.failTo {
		return 0, errors.New("simulated reassign failure")
	}
	return f.Store.Reassign(ctx, fromNS, ids, toNS)
}

// TestMigrateScopesOnPrintsPartialReportOnError pins the CLI's partial-report
// contract: when the 2nd of 2 merges fails mid-migration, the operator must
// still see the 1st (already committed) merge in the output — plus a clear
// stopped-early line — and the error must still propagate for the non-zero
// exit.
func TestMigrateScopesOnPrintsPartialReportOnError(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), migrateTestDims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	emb := embedtest.New(migrateTestDims)

	// Sorted namespace order processes alpha/_shared before beta/_shared, so
	// failing the reassign into "beta" fails the 2nd merge after the 1st
	// committed.
	seedShared(t, st, emb, "alpha/_shared", "a1", "alpha shared fact")
	seedShared(t, st, emb, "beta/_shared", "b1", "beta shared fact")

	var buf bytes.Buffer
	frs := &failSecondReassignStore{Store: st, failTo: "beta"}
	err = migrateScopesOn(ctx, &buf, frs, maintenance.ScopesOptions{Embedder: emb})
	if err == nil || !strings.Contains(err.Error(), "simulated reassign failure") {
		t.Fatalf("want the mid-migration error to propagate, got %v", err)
	}
	out := buf.String()
	for _, want := range []string{"alpha/_shared", "alpha", "stopped early"} {
		if !strings.Contains(out, want) {
			t.Errorf("partial report missing %q; got:\n%s", want, out)
		}
	}
	// The 1st merge really committed; the 2nd did not.
	mems, err := st.List(ctx, "alpha", store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
	if err != nil {
		t.Fatalf("list alpha: %v", err)
	}
	if len(mems) != 1 {
		t.Errorf("alpha has %d memories, want 1 (the committed merge)", len(mems))
	}
	mems, err = st.List(ctx, "beta/_shared", store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
	if err != nil {
		t.Fatalf("list beta/_shared: %v", err)
	}
	if len(mems) != 1 {
		t.Errorf("beta/_shared has %d memories, want unchanged 1 (the failed merge)", len(mems))
	}
}

// TestRunMigrateScopesDryRunEndToEnd exercises the full CLI->library wiring
// (config from env, withLocalStore, MigrateScopes, report printer) against a
// real sqlite store in dry-run mode: the report names the would-be merge and
// the store is left untouched.
func TestRunMigrateScopesDryRunEndToEnd(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "m.db")
	emb := embedtest.New(4)

	// Seed with a separate handle, closed before the CLI opens its own.
	seed, err := sqlitevec.Open(ctx, dbPath, 4)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	seedShared(t, seed, emb, "acme/_shared", "s1", "shared fact")
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	t.Setenv("MEMINI_BACKEND", "sqlite")
	t.Setenv("MEMINI_SQLITE_PATH", dbPath)
	t.Setenv("MEMINI_EMBED_DIMS", "4")
	t.Setenv("MEMINI_EMBED_BASE_URL", "")
	t.Setenv("MEMINI_GLOBAL_NAMESPACE", "")

	migrateScopesYes = false
	var buf bytes.Buffer
	migrateScopesCmd.SetOut(&buf)
	t.Cleanup(func() { migrateScopesCmd.SetOut(nil) })
	migrateScopesCmd.SetContext(ctx)

	if err := runMigrateScopes(migrateScopesCmd, nil); err != nil {
		t.Fatalf("runMigrateScopes dry-run: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"(dry-run)", "acme/_shared", "acme"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q; got:\n%s", want, out)
		}
	}

	// The store is unchanged: the memory still lives in acme/_shared.
	check, err := sqlitevec.Open(ctx, dbPath, 4)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = check.Close() })
	mems, err := check.List(ctx, "acme/_shared", store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
	if err != nil {
		t.Fatalf("list acme/_shared: %v", err)
	}
	if len(mems) != 1 {
		t.Errorf("acme/_shared has %d memories after dry-run, want unchanged 1", len(mems))
	}
}
