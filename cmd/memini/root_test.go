package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestRunServerRefusesDeletedGlobalNamespace and
// TestRunServerRefusesDeletedTenantShared pin the T12 boot guard at the
// actual call site (not just the config.FatalDeprecatedVars helper):
// runServer must refuse before doing any work when either deleted knob is
// set, and the returned error must reach the operator with the raw
// guidance (see config.FatalDeprecatedVars for the message contract) rather
// than being wrapped into generic noise.
func TestRunServerRefusesDeletedGlobalNamespace(t *testing.T) {
	t.Setenv("MEMINI_GLOBAL_NAMESPACE", "shared/golang")

	err := runServer(&cobra.Command{}, nil)
	if err == nil {
		t.Fatal("runServer: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "MEMINI_GLOBAL_NAMESPACE") {
		t.Errorf("runServer error = %q, want it to name MEMINI_GLOBAL_NAMESPACE", err.Error())
	}
}

func TestRunServerRefusesDeletedTenantShared(t *testing.T) {
	t.Setenv("MEMINI_TENANT_SHARED", "true")

	err := runServer(&cobra.Command{}, nil)
	if err == nil {
		t.Fatal("runServer: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "MEMINI_TENANT_SHARED") {
		t.Errorf("runServer error = %q, want it to name MEMINI_TENANT_SHARED", err.Error())
	}
}

// TestRunMCPRefusesDeletedGlobalNamespace and
// TestRunMCPRefusesDeletedTenantShared pin the same T12 boot guard on the
// stdio MCP entrypoint (review finding): `memini mcp` builds the identical
// service stack via buildServiceStack and runs as a persistent server —
// the standard plugin deployment mode — so it must refuse a stale deleted
// knob exactly like runServer, not boot silently past it.
func TestRunMCPRefusesDeletedGlobalNamespace(t *testing.T) {
	t.Setenv("MEMINI_GLOBAL_NAMESPACE", "shared/golang")
	t.Setenv("MEMINI_SQLITE_PATH", t.TempDir()+"/memini.db") // never reached; keeps a regressed run from touching a real db

	err := runMCP(&cobra.Command{}, nil)
	if err == nil {
		t.Fatal("runMCP: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "MEMINI_GLOBAL_NAMESPACE") {
		t.Errorf("runMCP error = %q, want it to name MEMINI_GLOBAL_NAMESPACE", err.Error())
	}
}

func TestRunMCPRefusesDeletedTenantShared(t *testing.T) {
	t.Setenv("MEMINI_TENANT_SHARED", "true")
	t.Setenv("MEMINI_SQLITE_PATH", t.TempDir()+"/memini.db")

	err := runMCP(&cobra.Command{}, nil)
	if err == nil {
		t.Fatal("runMCP: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "MEMINI_TENANT_SHARED") {
		t.Errorf("runMCP error = %q, want it to name MEMINI_TENANT_SHARED", err.Error())
	}
}

// TestRunMigrateScopesNotBlockedByGlobalNamespace pins the deadlock-avoidance
// case (brief's "IMPORTANT subtlety"): `memini migrate scopes` must keep
// running even with MEMINI_GLOBAL_NAMESPACE set, since it's the very command
// that reads that var to print adoption instructions. If the boot guard were
// enforced inside config.Load() (which runMigrateScopes also calls), this
// would deadlock the operator. Uses --yes=false (dry-run default) against an
// empty sqlite store so the command completes without needing an embedder.
// The output assertions prove the exempted path actually ran through to the
// report printer (T11's adoption instructions), not merely returned nil.
func TestRunMigrateScopesNotBlockedByGlobalNamespace(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEMINI_BACKEND", "sqlite")
	t.Setenv("MEMINI_SQLITE_PATH", dir+"/memini.db")
	t.Setenv("MEMINI_GLOBAL_NAMESPACE", "shared/golang")
	t.Setenv("MEMINI_EMBED_DIMS", "8")

	migrateScopesYes = false
	migrateScopesCmd.SetContext(context.Background())
	var buf bytes.Buffer
	migrateScopesCmd.SetOut(&buf)
	t.Cleanup(func() { migrateScopesCmd.SetOut(nil) })

	if err := runMigrateScopes(migrateScopesCmd, nil); err != nil {
		t.Fatalf("runMigrateScopes: unexpected error with MEMINI_GLOBAL_NAMESPACE set: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"MEMINI_GLOBAL_NAMESPACE",     // names the dead knob
		`MEMINI_HOME="shared/golang"`, // single-operator adoption
		"memini link add",             // team-wide adoption
	} {
		if !strings.Contains(out, want) {
			t.Errorf("adoption instructions missing %q; got:\n%s", want, out)
		}
	}
}
