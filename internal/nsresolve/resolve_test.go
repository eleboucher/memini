package nsresolve_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eleboucher/memini/internal/nsresolve"
	"github.com/eleboucher/memini/internal/store"
)

// pinFor builds a PinLookup that returns namespace ns for the first of its
// argument keys present in pinned (preference order preserved by Resolve), and
// records which key matched. A nil/empty pinned map is "no pins".
func pinFor(pinned map[string]string) nsresolve.PinLookup {
	return func(_ context.Context, keys []string) (string, string, bool, error) {
		for _, k := range keys {
			if ns, ok := pinned[k]; ok {
				return ns, k, true, nil
			}
		}
		return "", "", false, nil
	}
}

// TestResolvePrecedenceMatrix pins the full precedence ordering
// pin > env > declared > derive > key-default > server-default by starting from
// a fact set that would resolve every way and peeling off the winners one at a
// time — each removal must expose exactly the next rule down.
func TestResolvePrecedenceMatrix(t *testing.T) {
	// A project that could resolve by pin, env, declared, or derivation.
	base := nsresolve.Facts{
		RemoteURL:         "https://github.com/acme/phoenix.git",
		ToplevelPath:      "/home/kit/src/phoenix",
		ToplevelBasename:  "phoenix",
		CwdBasename:       "phoenix",
		EnvNamespace:      "env-ns",
		DeclaredNamespace: "declared-ns",
	}
	pins := pinFor(map[string]string{"remote:github.com/acme/phoenix": "pinned-ns"})

	cases := []struct {
		name          string
		facts         nsresolve.Facts
		pins          nsresolve.PinLookup
		keyDefault    string
		serverDefault string
		wantNS        string
		wantSource    string
		wantPinKey    string
	}{
		{
			name:  "pin wins over env, declared, and derivation",
			facts: base, pins: pins, keyDefault: "kd", serverDefault: "sd",
			wantNS: "pinned-ns", wantSource: nsresolve.SourcePin,
			wantPinKey: "remote:github.com/acme/phoenix",
		},
		{
			name:  "env wins once there is no pin",
			facts: base, pins: nil, keyDefault: "kd", serverDefault: "sd",
			wantNS: "env-ns", wantSource: nsresolve.SourceEnv,
		},
		{
			name:  "declared wins once pin and env are gone",
			facts: withoutEnv(base), pins: nil, keyDefault: "kd", serverDefault: "sd",
			wantNS: "declared-ns", wantSource: nsresolve.SourceDeclared,
		},
		{
			name:  "derivation (remote) wins once pin/env/declared are gone",
			facts: onlyProject(base), pins: nil, keyDefault: "kd", serverDefault: "sd",
			wantNS: "phoenix", wantSource: nsresolve.SourceRemote,
		},
		{
			name:  "key-default wins when there is nothing to derive from",
			facts: nsresolve.Facts{}, pins: nil, keyDefault: "kd", serverDefault: "sd",
			wantNS: "kd", wantSource: nsresolve.SourceKeyDefault,
		},
		{
			name:  "server-default is the last resort",
			facts: nsresolve.Facts{}, pins: nil, keyDefault: "", serverDefault: "sd",
			wantNS: "sd", wantSource: nsresolve.SourceServerDefault,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := nsresolve.Resolve(context.Background(), c.facts, c.pins, store.ClientSettings{}, c.keyDefault, c.serverDefault)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Namespace != c.wantNS {
				t.Errorf("namespace = %q, want %q", got.Namespace, c.wantNS)
			}
			if got.Source != c.wantSource {
				t.Errorf("source = %q, want %q", got.Source, c.wantSource)
			}
			if got.PinKey != c.wantPinKey {
				t.Errorf("pin key = %q, want %q", got.PinKey, c.wantPinKey)
			}
		})
	}
}

func withoutEnv(f nsresolve.Facts) nsresolve.Facts { f.EnvNamespace = ""; return f }
func onlyProject(f nsresolve.Facts) nsresolve.Facts {
	f.EnvNamespace = ""
	f.DeclaredNamespace = ""
	return f
}

// TestResolvePinBeatsEnvAndDeclared is the headline reason env_namespace is sent
// to the server at all: the client cannot know a pin exists, so only the server
// can let a pin beat MEMINI_NAMESPACE (and a declared namespace).
func TestResolvePinBeatsEnvAndDeclared(t *testing.T) {
	f := nsresolve.Facts{
		ToplevelPath:      "/srv/app",
		EnvNamespace:      "from-env",
		DeclaredNamespace: "from-declared",
	}
	pins := pinFor(map[string]string{"path:/srv/app": "from-pin"})
	got, err := nsresolve.Resolve(context.Background(), f, pins, store.ClientSettings{}, "", "sd")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Namespace != "from-pin" || got.Source != nsresolve.SourcePin {
		t.Fatalf("pin must beat env and declared, got %q/%q", got.Namespace, got.Source)
	}
	if got.PinKey != "path:/srv/app" {
		t.Errorf("pin key = %q, want path:/srv/app", got.PinKey)
	}
}

// TestResolveDeclaredBeatsDerivationLosesToEnv pins declared_namespace's exact
// rung: it wins over derivation (verbatim, no agent suffix) but loses to
// env_namespace.
func TestResolveDeclaredBeatsDerivationLosesToEnv(t *testing.T) {
	withEnv := nsresolve.Facts{
		RemoteURL:         "https://github.com/acme/phoenix.git",
		Agent:             "reviewer",
		EnvNamespace:      "env-ns",
		DeclaredNamespace: "declared-ns",
	}
	got, err := nsresolve.Resolve(context.Background(), withEnv, nil, store.ClientSettings{}, "", "sd")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Source != nsresolve.SourceEnv {
		t.Fatalf("env must beat declared, got source %q", got.Source)
	}

	noEnv := withEnv
	noEnv.EnvNamespace = ""
	got, err = nsresolve.Resolve(context.Background(), noEnv, nil, store.ClientSettings{}, "", "sd")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Verbatim: declared beats the remote derivation AND carries no agent suffix.
	if got.Namespace != "declared-ns" || got.Source != nsresolve.SourceDeclared {
		t.Fatalf("declared must beat derivation verbatim, got %q/%q", got.Namespace, got.Source)
	}
}

// TestResolvePrefixAndAgentInteraction pins how namespace_prefix and the agent
// suffix compose on a derived namespace: prefix is prepended, the agent suffix
// appended, so the derived middle is wrapped on both sides — and neither shapes
// a pin/env/declared/default value.
func TestResolvePrefixAndAgentInteraction(t *testing.T) {
	prefix := "team"
	scope := "owner_repo"
	s := store.ClientSettings{NamespacePrefix: &prefix, NamespaceScope: &scope}

	t.Run("prefix and agent wrap a derived owner-repo slug", func(t *testing.T) {
		f := nsresolve.Facts{RemoteURL: "https://github.com/acme/phoenix.git", Agent: "reviewer"}
		got, err := nsresolve.Resolve(context.Background(), f, nil, s, "", "sd")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Namespace != "team/acme-phoenix/reviewer" || got.Source != nsresolve.SourceRemote {
			t.Fatalf("got %q/%q, want team/acme-phoenix/reviewer/remote", got.Namespace, got.Source)
		}
	})

	t.Run("prefix does not touch a declared namespace", func(t *testing.T) {
		f := nsresolve.Facts{DeclaredNamespace: "gateway/hook", Agent: "reviewer"}
		got, err := nsresolve.Resolve(context.Background(), f, nil, s, "", "sd")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Namespace != "gateway/hook" || got.Source != nsresolve.SourceDeclared {
			t.Fatalf("declared must stay verbatim (no prefix/agent), got %q/%q", got.Namespace, got.Source)
		}
	})

	t.Run("agent that sanitizes to empty adds no suffix", func(t *testing.T) {
		f := nsresolve.Facts{RemoteURL: "https://github.com/acme/phoenix.git", Agent: "!!!"}
		got, err := nsresolve.Resolve(context.Background(), f, nil, store.ClientSettings{}, "", "sd")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Namespace != "phoenix" {
			t.Fatalf("got %q, want phoenix (no agent suffix)", got.Namespace)
		}
	})
}

// TestResolveInvalidNamespaceNamesFact pins the error contract: an invalid
// resolved namespace is nsresolve.ErrInvalidInput and the message names the
// offending fact, so the handler's 400 tells the operator which input was bad.
func TestResolveInvalidNamespaceNamesFact(t *testing.T) {
	huge := strings.Repeat("x", 300) // exceeds ValidateNamespace's 256-byte cap
	cases := []struct {
		name  string
		facts nsresolve.Facts
		want  string // substring the error must name
	}{
		{"declared", nsresolve.Facts{DeclaredNamespace: huge}, "declared_namespace"},
		{"env", nsresolve.Facts{EnvNamespace: huge}, "env_namespace"},
		{"derived from cwd", nsresolve.Facts{CwdBasename: huge}, "cwd_basename"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := nsresolve.Resolve(context.Background(), c.facts, nil, store.ClientSettings{}, "", "sd")
			if !errors.Is(err, nsresolve.ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name the offending fact %q", err, c.want)
			}
		})
	}
}

// TestResolveDeterministic pins the handshake's core guarantee: identical inputs
// (including identical PinLookup results) yield an identical Result, every time.
func TestResolveDeterministic(t *testing.T) {
	f := nsresolve.Facts{RemoteURL: "https://github.com/acme/phoenix.git", Agent: "reviewer"}
	first, err := nsresolve.Resolve(context.Background(), f, nil, store.ClientSettings{}, "", "sd")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for range 5 {
		got, err := nsresolve.Resolve(context.Background(), f, nil, store.ClientSettings{}, "", "sd")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got != first {
			t.Fatalf("non-deterministic: %+v != %+v", got, first)
		}
	}
}

// TestPinKeysOrderAndPresence pins PinKeys' preference order (remote before
// path) and its handling of absent facts.
func TestPinKeysOrderAndPresence(t *testing.T) {
	both := nsresolve.PinKeys(nsresolve.Facts{
		RemoteURL:    "git@github.com:acme/phoenix.git",
		ToplevelPath: "/srv/app",
	})
	want := []string{"remote:github.com/acme/phoenix", "path:/srv/app"}
	if len(both) != 2 || both[0] != want[0] || both[1] != want[1] {
		t.Fatalf("PinKeys = %v, want %v (remote first)", both, want)
	}
	if got := nsresolve.PinKeys(nsresolve.Facts{ToplevelPath: "/srv/app"}); len(got) != 1 || got[0] != "path:/srv/app" {
		t.Fatalf("path-only PinKeys = %v", got)
	}
	if got := nsresolve.PinKeys(nsresolve.Facts{CwdBasename: "bare"}); len(got) != 0 {
		t.Fatalf("bare-dir PinKeys = %v, want empty", got)
	}
}

// TestResolveSkipsPinsWhenCapabilityAbsent pins the graceful degrade: a nil
// PinLookup (a backend with no pin capability) resolves derived-only
// rather than erroring.
func TestResolveSkipsPinsWhenCapabilityAbsent(t *testing.T) {
	f := nsresolve.Facts{RemoteURL: "https://github.com/acme/phoenix.git", ToplevelPath: "/srv/app"}
	got, err := nsresolve.Resolve(context.Background(), f, nil, store.ClientSettings{}, "", "sd")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Source != nsresolve.SourceRemote || got.Namespace != "phoenix" {
		t.Fatalf("nil PinLookup should derive, got %q/%q", got.Namespace, got.Source)
	}
}
