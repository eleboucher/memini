package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestAddAPIKeyReadOnlyFlag: `memini key add <name> --read-only` mints a
// credential that cannot write. This is how a CI credential is provisioned when
// the server's api_keys table is the source of truth (the declarative
// MEMINI_API_KEYS_FILE path is covered in internal/apiauth).
func TestAddAPIKeyReadOnlyFlag(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	_, key, err := addAPIKey(context.Background(), ks, "ci-frank", keyAddOpts{ReadOnly: new(true)})
	if err != nil {
		t.Fatalf("addAPIKey: %v", err)
	}
	if !key.ReadOnly {
		t.Fatal("expected key to be created with ReadOnly=true")
	}
}

func TestAddAPIKeyReadOnlyDefaultsFalse(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	_, key, err := addAPIKey(context.Background(), ks, "plain-frank-ro", keyAddOpts{})
	if err != nil {
		t.Fatalf("addAPIKey: %v", err)
	}
	if key.ReadOnly {
		t.Fatal("expected key to default to ReadOnly=false when --read-only is not passed")
	}
}

// TestAddAPIKeyRotationPreservesReadOnly pins the contract that matters most
// operationally: rotating a CI credential's secret must not quietly hand it
// write access back.
func TestAddAPIKeyRotationPreservesReadOnly(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	ctx := context.Background()

	if _, _, err := addAPIKey(ctx, ks, "ci-ivy", keyAddOpts{ReadOnly: new(true)}); err != nil {
		t.Fatalf("addAPIKey: %v", err)
	}
	_, rotated, err := addAPIKey(ctx, ks, "ci-ivy", keyAddOpts{})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !rotated.ReadOnly {
		t.Error("rotation must preserve ReadOnly, got false")
	}
}

// TestAddAPIKeyRotationExplicitClearsReadOnly: stated operator intent wins,
// mirroring --admin=false and --disabled=false.
func TestAddAPIKeyRotationExplicitClearsReadOnly(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	ctx := context.Background()

	if _, _, err := addAPIKey(ctx, ks, "ci-kim", keyAddOpts{ReadOnly: new(true)}); err != nil {
		t.Fatalf("addAPIKey: %v", err)
	}
	_, rotated, err := addAPIKey(ctx, ks, "ci-kim", keyAddOpts{ReadOnly: new(false)})
	if err != nil {
		t.Fatalf("rotate with explicit --read-only=false: %v", err)
	}
	if rotated.ReadOnly {
		t.Error("explicit --read-only=false must lift the restriction")
	}
}

// TestAddAPIKeyReadOnlyIndependentOfAdmin: the two capabilities compose, so a
// CLI-minted admin+read-only auditor is expressible.
func TestAddAPIKeyReadOnlyIndependentOfAdmin(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	_, key, err := addAPIKey(context.Background(), ks, "auditor",
		keyAddOpts{Admin: new(true), ReadOnly: new(true)})
	if err != nil {
		t.Fatalf("addAPIKey: %v", err)
	}
	if !key.Admin || !key.ReadOnly {
		t.Fatalf("want Admin=true ReadOnly=true, got Admin=%v ReadOnly=%v", key.Admin, key.ReadOnly)
	}
}

// TestPrintAPIKeysTableShowsReadOnlyColumn: `memini key ls` must show which
// keys are read-only, or an operator cannot audit them from the CLI.
func TestPrintAPIKeysTableShowsReadOnlyColumn(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	ctx := context.Background()
	if _, _, err := addAPIKey(ctx, ks, "ci-list", keyAddOpts{ReadOnly: new(true)}); err != nil {
		t.Fatalf("addAPIKey: %v", err)
	}
	if _, _, err := addAPIKey(ctx, ks, "rw-list", keyAddOpts{}); err != nil {
		t.Fatalf("addAPIKey: %v", err)
	}
	keys, err := ks.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	var buf bytes.Buffer
	printAPIKeys(&buf, keys)
	out := buf.String()
	if !strings.Contains(out, "READ ONLY") {
		t.Fatalf("key ls header must carry a READ ONLY column, got:\n%s", out)
	}
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "ci-list":
			if !strings.HasSuffix(strings.TrimSpace(line), "true") {
				t.Errorf("ci-list row must end with read-only true, got %q", line)
			}
		case "rw-list":
			if !strings.HasSuffix(strings.TrimSpace(line), "false") {
				t.Errorf("rw-list row must end with read-only false, got %q", line)
			}
		}
	}
}
