package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// --- resolveVisibility (pure function) ------------------------------------

func TestResolveVisibility(t *testing.T) {
	tests := []struct {
		name       string
		visibility string
		namespace  string
		home       string
		tier       memory.Tier
		want       string
		wantErr    bool
		errSubstr  string
	}{
		{
			name: "empty visibility stays primary", visibility: "", namespace: "acme/phoenix",
			tier: memory.TierSemantic, want: "acme/phoenix",
		},
		{
			name: "project stays primary", visibility: "project", namespace: "acme/phoenix",
			tier: memory.TierSemantic, want: "acme/phoenix",
		},
		{
			name: "personal with home resolves to home", visibility: "personal", namespace: "acme/phoenix",
			home: "personal/kit", tier: memory.TierSemantic, want: "personal/kit",
		},
		{
			name: "personal without home errors naming MEMINI_HOME", visibility: "personal",
			namespace: "acme/phoenix", tier: memory.TierSemantic,
			wantErr: true, errSubstr: "MEMINI_HOME",
		},
		{
			name: "personal with whitespace-only home errors the same as empty", visibility: "personal",
			namespace: "acme/phoenix", home: "   ", tier: memory.TierSemantic,
			wantErr: true, errSubstr: "MEMINI_HOME",
		},
		{
			name: "ancestor by exact full path", visibility: "acme/phoenix",
			namespace: "acme/phoenix/api", tier: memory.TierSemantic, want: "acme/phoenix",
		},
		{
			name: "ancestor by unambiguous last segment", visibility: "acme",
			namespace: "acme/phoenix/api", tier: memory.TierSemantic, want: "acme",
		},
		{
			// "mid" is not the root segment, so no ancestor's full path equals
			// "mid" outright (which would win over ambiguity per spec) — it
			// repeats only as the last segment of two distinct ancestors.
			name: "ambiguous last segment errors listing the chain", visibility: "mid",
			namespace: "acme/mid/mid/west", tier: memory.TierSemantic,
			wantErr: true, errSubstr: "acme/mid/mid, acme/mid, acme",
		},
		{
			name: "unknown visibility errors listing the chain", visibility: "bogus",
			namespace: "acme/phoenix/api", tier: memory.TierSemantic,
			wantErr: true, errSubstr: "valid: project, personal, acme/phoenix, acme",
		},
		{
			name: "unknown visibility on a flat namespace lists no chain", visibility: "bogus",
			namespace: "acme", tier: memory.TierSemantic,
			wantErr: true, errSubstr: "valid: project, personal",
		},
		{
			name:       "tier clamp: episodic stays primary despite ancestor visibility",
			visibility: "acme", namespace: "acme/phoenix/api", tier: memory.TierEpisodic,
			want: "acme/phoenix/api",
		},
		{
			name:       "tier clamp: working stays primary despite personal visibility, no home needed",
			visibility: "personal", namespace: "acme/phoenix", tier: memory.TierWorking,
			home: "", want: "acme/phoenix",
		},
		{
			name:       "tier clamp: episodic stays primary despite unknown visibility (clamp precedes validation)",
			visibility: "bogus", namespace: "acme/phoenix", tier: memory.TierEpisodic,
			want: "acme/phoenix",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := RememberInput{Namespace: tt.namespace, Home: tt.home, Visibility: tt.visibility}
			got, err := resolveVisibility(in, tt.tier)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveVisibility() error = nil, want error")
				}
				if !errors.Is(err, ErrInvalidInput) {
					t.Errorf("resolveVisibility() error = %v, want ErrInvalidInput", err)
				}
				if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("resolveVisibility() error = %q, want substring %q", err.Error(), tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveVisibility() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveVisibility() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- lastSegment -----------------------------------------------------------

func TestLastSegment(t *testing.T) {
	tests := []struct{ in, want string }{
		{"acme", "acme"},
		{"acme/phoenix", "phoenix"},
		{"acme/phoenix/api", "api"},
	}
	for _, tt := range tests {
		if got := lastSegment(tt.in); got != tt.want {
			t.Errorf("lastSegment(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// --- Remember integration: visibility end-to-end ---------------------------

const visibilityTestDims = 64

// newVisibilitySvc builds a Service over a real sqlite-vec store for
// visibility integration tests, with fingerprint dedup left at its default
// (on) and no fuzzy write-dedup unless the caller adds it.
func newVisibilitySvc(t *testing.T, opts ...Option) (*Service, *sqlitevec.Store) {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "visibility.db"), visibilityTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	all := append([]Option{
		WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
	}, opts...)
	svc := New(st, embedtest.New(visibilityTestDims), all...)
	// Registered AFTER the store's Close cleanup so it runs BEFORE it (t.Cleanup
	// is LIFO): detached best-effort work — write-time reinforcement,
	// corroboration, contradiction routing — must finish before the store closes
	// or it writes into a closed database and leaves WAL files behind that
	// TempDir cleanup then trips over. This is WaitBackground's documented
	// contract, and it is the same order cmd/memini uses at shutdown.
	t.Cleanup(svc.WaitBackground)
	return svc, st
}

func TestRememberVisibilityRoutesTargetNamespace(t *testing.T) {
	ctx := context.Background()
	svc, st := newVisibilitySvc(t)

	m, err := svc.Remember(ctx, RememberInput{
		Namespace:  "acme/phoenix",
		Visibility: "acme",
		Content:    "the deploy window is 3am UTC",
		Tier:       memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if m.Namespace != "acme" {
		t.Fatalf("m.Namespace = %q, want %q", m.Namespace, "acme")
	}

	got, err := st.List(ctx, "acme/phoenix", store.Filter{}, 0)
	if err != nil {
		t.Fatalf("list acme/phoenix: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("acme/phoenix has %d memories, want 0 (write should have landed in acme)", len(got))
	}
	got, err = st.List(ctx, "acme", store.Filter{}, 0)
	if err != nil {
		t.Fatalf("list acme: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("acme has %d memories, want 1", len(got))
	}
}

// TestRememberVisibilityFollowsFingerprintDedup pins gap G4: the
// exact-restatement fingerprint gate (fingerprintDedup, on by default) checks
// GetByFingerprint against the RESOLVED target namespace, not the request
// primary. A duplicate write routed by visibility to an ancestor that already
// holds the same content must coalesce into the ancestor's existing memory,
// not create a second copy under the primary namespace.
func TestRememberVisibilityFollowsFingerprintDedup(t *testing.T) {
	ctx := context.Background()
	svc, st := newVisibilitySvc(t)
	const content = "the deploy window is 3am UTC"

	first, err := svc.Remember(ctx, RememberInput{
		Namespace: "acme",
		Content:   content,
		Tier:      memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember (seed): %v", err)
	}

	second, err := svc.Remember(ctx, RememberInput{
		Namespace:  "acme/phoenix",
		Visibility: "acme",
		Content:    content,
		Tier:       memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember (duplicate via visibility): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second write got a new memory %q, want fingerprint hit on %q", second.ID, first.ID)
	}

	got, err := st.List(ctx, "acme/phoenix", store.Filter{}, 0)
	if err != nil {
		t.Fatalf("list acme/phoenix: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("acme/phoenix has %d memories, want 0 (dedup should have hit in acme)", len(got))
	}
	got, err = st.List(ctx, "acme", store.Filter{}, 0)
	if err != nil {
		t.Fatalf("list acme: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("acme has %d memories, want 1 (no duplicate)", len(got))
	}
}

// TestRememberVisibilityFollowsFuzzyDedup mirrors the fingerprint test above
// for the fuzzy write-time dedup gate (WriteDedupCoalesce): the write-time
// vector search that decides coalesce/supersede/hint must also run against
// the resolved target namespace.
func TestRememberVisibilityFollowsFuzzyDedup(t *testing.T) {
	ctx := context.Background()
	svc, st := newVisibilitySvc(t, WithWriteDedup(0.9, WriteDedupCoalesce), WithSyncReinforce())

	first, err := svc.Remember(ctx, RememberInput{
		Namespace: "acme",
		Content:   "the deploy window is 3am UTC",
		Tier:      memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember (seed): %v", err)
	}

	second, err := svc.Remember(ctx, RememberInput{
		Namespace:  "acme/phoenix",
		Visibility: "acme",
		Content:    "the deploy window is 3am UTC", // identical text embeds identically under embedtest
		Tier:       memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember (near-duplicate via visibility): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second write got a new memory %q, want coalesce onto %q", second.ID, first.ID)
	}

	got, err := st.List(ctx, "acme/phoenix", store.Filter{}, 0)
	if err != nil {
		t.Fatalf("list acme/phoenix: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("acme/phoenix has %d memories, want 0 (fuzzy dedup should have coalesced in acme)", len(got))
	}
}

// TestRememberVisibilityClampAppliesToAutoClassifiedTier pins requirement 5:
// the tier clamp must see the FINAL tier — after auto-classification — not
// the raw (possibly empty) input tier. Content with no decision/preference/
// problem marker leaves the auto-classified tier at the working default
// (non-durable): visibility must clamp to primary even though it names an
// in-scope ancestor. Content that DOES match a marker gets promoted to a
// durable tier by the classifier: visibility must NOT clamp, proving
// resolveVisibility runs after classification rather than on the
// pre-classification placeholder.
func TestRememberVisibilityClampAppliesToAutoClassifiedTier(t *testing.T) {
	ctx := context.Background()

	t.Run("unclassified capture clamps to primary", func(t *testing.T) {
		svc, st := newVisibilitySvc(t)
		m, err := svc.Remember(ctx, RememberInput{
			Namespace:  "acme/phoenix",
			Visibility: "acme",
			Content:    "ok sounds good, moving on",
		})
		if err != nil {
			t.Fatalf("remember: %v", err)
		}
		if m.Tier != memory.TierWorking {
			t.Fatalf("tier = %q, want %q (test assumes no marker matches)", m.Tier, memory.TierWorking)
		}
		if m.Namespace != "acme/phoenix" {
			t.Fatalf("m.Namespace = %q, want clamp to primary %q", m.Namespace, "acme/phoenix")
		}
		got, err := st.List(ctx, "acme", store.Filter{}, 0)
		if err != nil {
			t.Fatalf("list acme: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("acme has %d memories, want 0 (clamped write must not travel)", len(got))
		}
	})

	t.Run("classifier-promoted durable capture honors visibility", func(t *testing.T) {
		svc, _ := newVisibilitySvc(t)
		// "we decided" + "instead of" match distinct decision markers (see
		// TestLevelTagging), promoting the auto-classified tier to semantic.
		m, err := svc.Remember(ctx, RememberInput{
			Namespace:  "acme/phoenix",
			Visibility: "acme",
			Content:    "We decided to switch to Postgres instead of SQLite",
		})
		if err != nil {
			t.Fatalf("remember: %v", err)
		}
		if m.Tier != memory.TierSemantic {
			t.Fatalf("tier = %q, want %q (test assumes markers classify this durable)", m.Tier, memory.TierSemantic)
		}
		if m.Namespace != "acme" {
			t.Fatalf("m.Namespace = %q, want resolved ancestor %q", m.Namespace, "acme")
		}
	})
}
