package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// noLinkStore wraps a store.Store, deliberately hiding any LinkStore methods
// the wrapped concrete type happens to implement — the wrapper's own method
// set has only what store.Store declares, so a type assertion to
// store.LinkStore fails, simulating a backend without link support.
type noLinkStore struct{ store.Store }

func newNoLinkSvc(t *testing.T) *Service {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "nolink.db"), readsetTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(&noLinkStore{Store: st}, embedtest.New(readsetTestDims),
		WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }))
}

func TestLinkNamespacesNilLinkStore(t *testing.T) {
	svc := newNoLinkSvc(t)
	if err := svc.LinkNamespaces(context.Background(), "A", "B", ""); !errors.Is(err, ErrLinksUnsupported) {
		t.Fatalf("LinkNamespaces on a non-LinkStore backend = %v, want ErrLinksUnsupported", err)
	}
	if err := svc.UnlinkNamespaces(context.Background(), "A", "B"); !errors.Is(err, ErrLinksUnsupported) {
		t.Fatalf("UnlinkNamespaces on a non-LinkStore backend = %v, want ErrLinksUnsupported", err)
	}
	if _, err := svc.NamespaceLinks(context.Background(), "A"); !errors.Is(err, ErrLinksUnsupported) {
		t.Fatalf("NamespaceLinks on a non-LinkStore backend = %v, want ErrLinksUnsupported", err)
	}
}

func TestLinkNamespacesValidation(t *testing.T) {
	tests := []struct {
		name    string
		ns      string
		target  string
		tiers   string
		wantErr bool
	}{
		{name: "valid durable", ns: "A", target: "B", tiers: "durable"},
		{name: "valid all", ns: "A", target: "B", tiers: "all"},
		{name: "empty tiers defaults to durable", ns: "A", target: "B", tiers: ""},
		{name: "valid subtree target", ns: "A", target: "team/*", tiers: "durable"},
		{name: "self-link rejected", ns: "A", target: "A", tiers: "durable", wantErr: true},
		{name: "self-subtree link is legal (children, not self)", ns: "A", target: "A/*", tiers: "durable"},
		{name: "invalid tiers", ns: "A", target: "B", tiers: "bogus", wantErr: true},
		{name: "empty namespace", ns: "", target: "B", tiers: "durable", wantErr: true},
		{name: "empty target", ns: "A", target: "", tiers: "durable", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := newReadsetSvc(t)
			err := svc.LinkNamespaces(context.Background(), tt.ns, tt.target, tt.tiers)
			if tt.wantErr {
				if err == nil || !errors.Is(err, ErrInvalidInput) {
					t.Fatalf("LinkNamespaces(%q, %q, %q) = %v, want ErrInvalidInput", tt.ns, tt.target, tt.tiers, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("LinkNamespaces(%q, %q, %q): %v", tt.ns, tt.target, tt.tiers, err)
			}
			links, err := svc.NamespaceLinks(context.Background(), tt.ns)
			if err != nil {
				t.Fatalf("NamespaceLinks: %v", err)
			}
			var found *store.NamespaceLink
			for i := range links {
				if links[i].Target == tt.target {
					found = &links[i]
				}
			}
			if found == nil {
				t.Fatalf("link to %q not found after LinkNamespaces: %+v", tt.target, links)
			}
			wantTiers := tt.tiers
			if wantTiers == "" {
				wantTiers = "durable"
			}
			if found.Tiers != wantTiers {
				t.Errorf("stored tiers = %q, want %q", found.Tiers, wantTiers)
			}
		})
	}
}

func TestLinkNamespacesUpsertOverwritesTiers(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	ctx := context.Background()
	if err := svc.LinkNamespaces(ctx, "A", "B", "durable"); err != nil {
		t.Fatalf("initial link: %v", err)
	}
	if err := svc.LinkNamespaces(ctx, "A", "B", "all"); err != nil {
		t.Fatalf("overwrite link: %v", err)
	}
	links, err := svc.NamespaceLinks(ctx, "A")
	if err != nil {
		t.Fatalf("NamespaceLinks: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("links = %d entries after upsert, want 1: %+v", len(links), links)
	}
	if links[0].Tiers != "all" {
		t.Fatalf("tiers after overwrite = %q, want %q", links[0].Tiers, "all")
	}
}

func TestLinkNamespacesCap(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	ctx := context.Background()
	for i := 0; i < maxNamespaceLinks; i++ {
		target := "target" + string(rune('a'+i))
		if err := svc.LinkNamespaces(ctx, "A", target, "durable"); err != nil {
			t.Fatalf("link %d (%s): %v", i, target, err)
		}
	}
	// The cap is reached; one more distinct target must be rejected.
	if err := svc.LinkNamespaces(ctx, "A", "one-too-many", "durable"); err == nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("link past cap = %v, want ErrInvalidInput", err)
	}
	// But an upsert of an already-linked target does not count against the cap.
	if err := svc.LinkNamespaces(ctx, "A", "targeta", "all"); err != nil {
		t.Fatalf("upsert of existing target at cap: %v", err)
	}
}

func TestUnlinkNamespacesNotFound(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	ctx := context.Background()
	if err := svc.UnlinkNamespaces(ctx, "A", "never-linked"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UnlinkNamespaces on an absent link = %v, want store.ErrNotFound", err)
	}
	if err := svc.LinkNamespaces(ctx, "A", "B", "durable"); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := svc.UnlinkNamespaces(ctx, "A", "B"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if err := svc.UnlinkNamespaces(ctx, "A", "B"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second unlink of the same pair = %v, want store.ErrNotFound", err)
	}
}

func TestUnlinkNamespacesInvalidNamespace(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	if err := svc.UnlinkNamespaces(context.Background(), "", "B"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("UnlinkNamespaces with empty namespace = %v, want ErrInvalidInput", err)
	}
}
