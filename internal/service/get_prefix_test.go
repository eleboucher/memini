package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// prefixNS is the single namespace the prefix tests address — Get's prefix
// resolution is namespace-scoped, and one namespace exercises it fully.
const prefixNS = "alice"

// putWithID stores a memory under an explicit id so the prefix fixtures
// control the leading bytes exactly.
func putWithID(t *testing.T, svc *service.Service, id, content string) {
	t.Helper()
	if _, err := svc.Remember(context.Background(), service.RememberInput{
		Namespace: prefixNS, Content: content, Tier: memory.TierSemantic, ID: id,
	}); err != nil {
		t.Fatalf("remember %s: %v", id, err)
	}
}

// TestGetIDPrefixResolvesUnique pins the short-id convenience: a >=8-hex-char
// prefix that matches exactly one memory in the namespace resolves to it.
func TestGetIDPrefixResolvesUnique(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	const ns = prefixNS
	putWithID(t, svc, "cafef00d-aaaa-4000-8000-000000000001", "the unique one")
	putWithID(t, svc, "deadbeef-bbbb-4000-8000-000000000002", "a decoy")

	for _, prefix := range []string{"cafef00d", "cafef00d-aaaa", "cafef00d-aaaa-4000-8000-000000000001"} {
		m, err := svc.Get(ctx, ns, prefix)
		if err != nil {
			t.Fatalf("Get(%q): %v", prefix, err)
		}
		if m.ID != "cafef00d-aaaa-4000-8000-000000000001" {
			t.Fatalf("Get(%q) resolved %q", prefix, m.ID)
		}
	}
}

// TestGetIDPrefixAmbiguous pins the collision contract: a prefix matching
// more than one memory errors as a conflict (409 over REST) and the error
// lists the colliding full ids so the caller can retry with a longer prefix.
func TestGetIDPrefixAmbiguous(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	const ns = prefixNS
	putWithID(t, svc, "deadbeef-1111-4000-8000-000000000001", "first twin")
	putWithID(t, svc, "deadbeef-2222-4000-8000-000000000002", "second twin")

	_, err := svc.Get(ctx, ns, "deadbeef")
	if err == nil {
		t.Fatal("ambiguous prefix must error")
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("ambiguous prefix should be a conflict, got %v", err)
	}
	var amb *service.AmbiguousIDError
	if !errors.As(err, &amb) {
		t.Fatalf("want *service.AmbiguousIDError, got %T (%v)", err, err)
	}
	if amb.Prefix != "deadbeef" || len(amb.IDs) != 2 {
		t.Fatalf("AmbiguousIDError = %+v, want prefix deadbeef and 2 candidates", amb)
	}
	for _, id := range []string{
		"deadbeef-1111-4000-8000-000000000001",
		"deadbeef-2222-4000-8000-000000000002",
	} {
		if !strings.Contains(err.Error(), id) {
			t.Fatalf("error must list colliding id %s, got: %v", id, err)
		}
	}

	// A longer prefix that disambiguates succeeds.
	m, err := svc.Get(ctx, ns, "deadbeef-2222")
	if err != nil || m.ID != "deadbeef-2222-4000-8000-000000000002" {
		t.Fatalf("longer prefix: got (%v, %v)", m, err)
	}
}

// TestGetIDPrefixExactWins pins that a stored id equal to the given string is
// returned verbatim even when it is also a prefix of other ids — prefix
// resolution only runs after an exact miss.
func TestGetIDPrefixExactWins(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	const ns = prefixNS
	putWithID(t, svc, "abcd1234", "the exact short id")
	putWithID(t, svc, "abcd1234-5678-4000-8000-000000000001", "the longer sibling")

	m, err := svc.Get(ctx, ns, "abcd1234")
	if err != nil {
		t.Fatalf("exact get: %v", err)
	}
	if m.Content != "the exact short id" {
		t.Fatalf("exact id must win over prefix resolution, got %q (%s)", m.Content, m.ID)
	}
}

// TestGetIDPrefixIneligible pins the eligibility gate: prefixes shorter than
// 8 hex chars and non-hex ids never trigger the scan — they miss with the
// plain not-found error exactly as before.
func TestGetIDPrefixIneligible(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	const ns = prefixNS
	putWithID(t, svc, "cafef00d-aaaa-4000-8000-000000000001", "the unique one")
	putWithID(t, svc, "openclaw:main:0001", "an imported custom id")

	for _, id := range []string{
		"cafef00d",         // sanity: this one IS eligible
		"openclaw:main:00", // non-hex custom-id prefix: never scanned
		"cafef00",          // 7 hex chars: below the minimum
		"cafe",             // far below the minimum
	} {
		_, err := svc.Get(ctx, ns, id)
		switch id {
		case "cafef00d":
			if err != nil {
				t.Fatalf("eligible sanity check failed: %v", err)
			}
		default:
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("Get(%q) = %v, want ErrNotFound (no prefix resolution)", id, err)
			}
		}
	}
}
