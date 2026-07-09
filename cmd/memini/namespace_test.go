package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runCmd invokes fn (one of the namespace subcommand RunE functions) against
// a fresh output buffer, the way cobra would for a real invocation, and
// returns what it wrote to stdout. A bare &cobra.Command{} has a nil
// Context() until Execute() runs it through the real CLI, so it's set
// explicitly here.
func runCmd(t *testing.T, fn func(*cobra.Command, []string) error) (string, error) {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	err := fn(cmd, nil)
	return buf.String(), err
}

// withNamespaceTestStore points MEMINI_SQLITE_PATH at a fresh temp-dir
// database for the duration of the test, so namespace subcommands (which go
// through withLocalStore -> config.Load -> buildStore) operate on an isolated
// store instead of the real cwd-relative default.
func withNamespaceTestStore(t *testing.T) {
	t.Helper()
	t.Setenv("MEMINI_BACKEND", "sqlite")
	t.Setenv("MEMINI_SQLITE_PATH", filepath.Join(t.TempDir(), "ns-cli.db"))
}

// resetLinkFlags clears the package-level flag vars the link/unlink/links
// RunE functions read, so tests don't leak values across t.Run subtests the
// way cobra's own flag parsing would reset them between real invocations.
func resetLinkFlags() {
	nsFrom, nsTo, nsLinkTiers, nsNamespace = "", "", "", ""
}

func TestNamespaceLinkCLI(t *testing.T) {
	withNamespaceTestStore(t)
	t.Cleanup(resetLinkFlags)

	// Create with the default tiers.
	nsFrom, nsTo, nsLinkTiers = "A", "B", "durable"
	out, err := runCmd(t, runNamespaceLink)
	if err != nil {
		t.Fatalf("namespace link: %v", err)
	}
	if !strings.Contains(out, "A") || !strings.Contains(out, "B") {
		t.Errorf("link output = %q, want it to mention A and B", out)
	}

	// `namespace links` with no filter shows the new link.
	nsNamespace = ""
	out, err = runCmd(t, runNamespaceLinks)
	if err != nil {
		t.Fatalf("namespace links: %v", err)
	}
	if !strings.Contains(out, "A") || !strings.Contains(out, "B") || !strings.Contains(out, "durable") {
		t.Fatalf("namespace links output = %q, want a line for A -> B (durable)", out)
	}

	// `namespace links --namespace A` shows only A's outgoing links.
	nsNamespace = "A"
	out, err = runCmd(t, runNamespaceLinks)
	if err != nil {
		t.Fatalf("namespace links --namespace A: %v", err)
	}
	if !strings.Contains(out, "B") {
		t.Fatalf("namespace links --namespace A output = %q, want it to list B", out)
	}

	// Idempotent overwrite: linking again with tiers=all replaces, not duplicates.
	nsFrom, nsTo, nsLinkTiers = "A", "B", "all"
	if _, err := runCmd(t, runNamespaceLink); err != nil {
		t.Fatalf("namespace link (overwrite): %v", err)
	}
	nsNamespace = "A"
	out, err = runCmd(t, runNamespaceLinks)
	if err != nil {
		t.Fatalf("namespace links --namespace A (after overwrite): %v", err)
	}
	if strings.Count(out, "B") != 1 && !strings.Contains(out, "all") {
		t.Fatalf("namespace links after overwrite = %q, want a single line with tiers=all", out)
	}

	// Unlink removes it; a repeat unlink errors (ErrNotFound).
	nsFrom, nsTo = "A", "B"
	if _, err := runCmd(t, runNamespaceUnlink); err != nil {
		t.Fatalf("namespace unlink: %v", err)
	}
	if _, err := runCmd(t, runNamespaceUnlink); err == nil {
		t.Fatal("namespace unlink on an absent link should error, got nil")
	}

	nsNamespace = ""
	out, err = runCmd(t, runNamespaceLinks)
	if err != nil {
		t.Fatalf("namespace links (after unlink): %v", err)
	}
	if !strings.Contains(out, "no namespace links") {
		t.Fatalf("namespace links after unlink = %q, want an empty-state message", out)
	}
}

func TestNamespaceLinkCLIValidation(t *testing.T) {
	withNamespaceTestStore(t)
	t.Cleanup(resetLinkFlags)

	nsFrom, nsTo, nsLinkTiers = "A", "A", "durable"
	if _, err := runCmd(t, runNamespaceLink); err == nil {
		t.Fatal("self-link should be rejected by the CLI (via the shared service validation)")
	}

	nsFrom, nsTo, nsLinkTiers = "A", "B", "bogus"
	if _, err := runCmd(t, runNamespaceLink); err == nil {
		t.Fatal("invalid tiers should be rejected by the CLI")
	}
}
