package nsresolve

import (
	"context"
	"errors"
	"fmt"

	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/store"
)

// ErrInvalidInput is returned by Resolve when the namespace a rule resolves to
// fails httputil.ValidateNamespace. The wrapped message names the offending
// fact (e.g. "declared_namespace", "remote_url") so the caller can turn it into
// a 400 that tells the operator exactly which input was bad. Callers map it
// with errors.Is.
var ErrInvalidInput = errors.New("nsresolve: invalid input")

// Namespace-source labels, echoed back as HandshakeResponse.namespace_source.
// These are the spec's enum values (api/openapi.yaml) — snake_case throughout,
// no dashes on the wire.
const (
	SourcePin           = "pin"
	SourceEnv           = "env"
	SourceDeclared      = "declared"
	SourceRemote        = "remote"
	SourceToplevel      = "toplevel"
	SourceCwd           = "cwd"
	SourceKeyDefault    = "key_default"
	SourceServerDefault = "server_default"
)

// scopeOwnerRepo is the namespace_scope value that derives an "owner-repo" slug
// instead of the bare repo name. Matches store.ClientSettings' namespace_scope
// enum (api/openapi.yaml).
const scopeOwnerRepo = "owner_repo"

// Facts is what a client knows about its project and itself at handshake time.
// Every field is optional except that a usable resolution needs at least a
// cwd_basename (the last-resort derivation fallback). Raw, unnormalized values
// exactly as the client observed them — Resolve does the normalizing.
type Facts struct {
	// RemoteURL is the raw `git remote get-url origin`, unnormalized.
	RemoteURL string
	// ToplevelPath is the absolute git toplevel directory (the path pin key).
	ToplevelPath string
	// ToplevelBasename is basename(ToplevelPath), the toplevel derivation fallback.
	ToplevelBasename string
	// CwdBasename is basename(cwd), the last-resort derivation fallback.
	CwdBasename string
	// Agent is an optional per-agent suffix (sanitized, appended as a segment).
	Agent string
	// EnvNamespace is the client's MEMINI_NAMESPACE, sent so a pin can still
	// beat it server-side.
	EnvNamespace string
	// DeclaredNamespace is a namespace a gateway/CI caller declares directly.
	DeclaredNamespace string
}

// Result is a resolved namespace plus why it was chosen. PinKey is set (to the
// project_map key that matched) only when Source == SourcePin.
type Result struct {
	Namespace string
	Source    string
	PinKey    string
}

// PinLookup resolves the first of keys (given in preference order) that has a
// project_map pin, returning its namespace and the key that matched. ok is
// false when none of the keys is pinned. A nil PinLookup, or one that reports a
// backend without the pin capability, means "no pins" — Resolve then falls
// through to derivation, exactly as it does for an unpinned project.
type PinLookup func(ctx context.Context, keys []string) (namespace string, key string, ok bool, err error)

// Resolve turns project facts into the one namespace a caller should use,
// applying the precedence (highest first):
//
//  1. pin           — an operator-created project_map entry for this project.
//  2. env           — the client's MEMINI_NAMESPACE.
//  3. declared      — a namespace a gateway/CI caller stated outright.
//  4. derive        — from the git remote (repo name, or owner-repo slug under
//     namespace_scope=owner_repo), else the toplevel basename, else the cwd
//     basename; then namespace_prefix is prepended and the agent suffix
//     appended.
//  5. key-default   — the caller's per-key default namespace.
//  6. server-default— the server-wide default namespace.
//
// pin/env/declared/key-default/server-default are returned VERBATIM: normalized
// and validated, but with NO prefix and NO agent suffix — those shape only the
// derived name. Every path ends by normalizing (httputil.NormalizeNamespace)
// and validating (httputil.ValidateNamespace) the chosen value; a failure is
// ErrInvalidInput naming the offending fact.
//
// It performs no writes and, given the same inputs and the same PinLookup
// results, always returns the same Result — the property that lets the
// handshake be deterministic and side-effect-free.
func Resolve(ctx context.Context, f Facts, pins PinLookup, s store.ClientSettings, keyDefault, serverDefault string) (Result, error) {
	// 1. pin — highest precedence, beats even MEMINI_NAMESPACE.
	if pins != nil {
		if keys := PinKeys(f); len(keys) > 0 {
			ns, key, ok, err := pins(ctx, keys)
			if err != nil {
				return Result{}, err
			}
			if ok {
				out, verr := finalize(ns, "pin")
				if verr != nil {
					return Result{}, verr
				}
				return Result{Namespace: out, Source: SourcePin, PinKey: key}, nil
			}
		}
	}

	// 2. env — the client's MEMINI_NAMESPACE.
	if f.EnvNamespace != "" {
		out, err := finalize(f.EnvNamespace, "env_namespace")
		if err != nil {
			return Result{}, err
		}
		return Result{Namespace: out, Source: SourceEnv}, nil
	}

	// 3. declared — a gateway/CI caller's stated namespace.
	if f.DeclaredNamespace != "" {
		out, err := finalize(f.DeclaredNamespace, "declared_namespace")
		if err != nil {
			return Result{}, err
		}
		return Result{Namespace: out, Source: SourceDeclared}, nil
	}

	// 4. derive — from remote/toplevel/cwd, then prefix + agent suffix.
	if base, source, fact := derive(f, s); base != "" {
		base = applyPrefix(base, s)
		base = withAgent(base, f.Agent)
		out, err := finalize(base, fact)
		if err != nil {
			return Result{}, err
		}
		return Result{Namespace: out, Source: source}, nil
	}

	// 5. key-default — the caller's per-key default namespace.
	if keyDefault != "" {
		out, err := finalize(keyDefault, "key_default")
		if err != nil {
			return Result{}, err
		}
		return Result{Namespace: out, Source: SourceKeyDefault}, nil
	}

	// 6. server-default — the last resort.
	out, err := finalize(serverDefault, "server_default")
	if err != nil {
		return Result{}, err
	}
	return Result{Namespace: out, Source: SourceServerDefault}, nil
}

// derive picks the raw derivation basename and its source from the project
// facts, returning "" when the facts carry nothing to derive from. fact is the
// name of the input the basename came from, for an ErrInvalidInput message.
func derive(f Facts, s store.ClientSettings) (base, source, fact string) {
	if f.RemoteURL != "" {
		var name string
		if scope(s) == scopeOwnerRepo {
			name = repoSlugFromRemote(f.RemoteURL)
		} else {
			name = repoNameFromRemote(f.RemoteURL)
		}
		if name != "" {
			return name, SourceRemote, "remote_url"
		}
	}
	if f.ToplevelBasename != "" {
		return f.ToplevelBasename, SourceToplevel, "toplevel_basename"
	}
	if f.CwdBasename != "" {
		return f.CwdBasename, SourceCwd, "cwd_basename"
	}
	return "", "", ""
}

// scope returns the effective namespace_scope, defaulting to "repo".
func scope(s store.ClientSettings) string {
	if s.NamespaceScope != nil {
		return *s.NamespaceScope
	}
	return "repo"
}

// applyPrefix prepends namespace_prefix ahead of a derived base
// ("prefix/base"); an unset/empty prefix leaves base untouched.
func applyPrefix(base string, s store.ClientSettings) string {
	if s.NamespacePrefix != nil && *s.NamespacePrefix != "" {
		return *s.NamespacePrefix + "/" + base
	}
	return base
}

// withAgent nests base under a per-agent segment ("base/reviewer") when agent
// is non-blank after sanitization, matching plugin/scripts/_shared.mjs:153-158.
// A blank agent, or one that sanitizes to nothing (e.g. "!!!"), adds no suffix.
func withAgent(base, agent string) string {
	seg := sanitizeSegment(agent)
	if seg == "" {
		return base
	}
	return base + "/" + seg
}

// finalize normalizes and validates a resolved namespace, wrapping a validation
// failure in ErrInvalidInput naming the fact it came from.
func finalize(ns, fact string) (string, error) {
	out := httputil.NormalizeNamespace(ns)
	if err := httputil.ValidateNamespace(out); err != nil {
		return "", fmt.Errorf("%w: %s %q: %v", ErrInvalidInput, fact, ns, err)
	}
	return out, nil
}
