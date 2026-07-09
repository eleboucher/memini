package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/store"
)

// ErrLinksUnsupported is returned by the namespace-link CRUD methods when the
// configured backend doesn't implement store.LinkStore. Reads are unaffected
// on such a backend — resolveDefaultReadSet simply has no links to consult —
// this only guards the write/list surface. API layers map it to a clear
// "unsupported" response (REST: 501; see internal/api/rest).
var ErrLinksUnsupported = errors.New("namespace links are not supported by this backend")

// normalizeLinkTiers validates a link's tier mode, defaulting the empty
// string to "durable" (semantic+procedural only). Any other value is a caller
// mistake, not an unsupported combination.
func normalizeLinkTiers(tiers string) (string, error) {
	switch tiers {
	case "":
		return "durable", nil
	case "durable", "all":
		return tiers, nil
	default:
		return "", invalidInputf("namespace link: invalid tiers %q, want \"durable\" or \"all\"", tiers)
	}
}

// LinkNamespaces creates or updates a read-only link: ns's default read set
// (recall/briefing without an explicit per-call namespace list) will also see
// target — 1-hop only, target's own links are never followed. target may end
// in "/*" to also include every namespace nested under it. Idempotent: a
// second call for the same (ns, target) overwrites tiers instead of erroring
// or duplicating, and an upsert of an already-linked target does not count
// against maxNamespaceLinks. Writes are never affected by links.
func (s *Service) LinkNamespaces(ctx context.Context, ns, target, tiers string) error {
	if s.linkStore == nil {
		return ErrLinksUnsupported
	}
	if err := httputil.ValidateNamespace(ns); err != nil {
		return invalidInputf("namespace link: invalid namespace %q: %v", ns, err)
	}
	bareTarget, _ := strings.CutSuffix(target, "/*")
	if err := httputil.ValidateNamespace(bareTarget); err != nil {
		return invalidInputf("namespace link: invalid target %q: %v", target, err)
	}
	// Only the exact self-reference is rejected; ns+"/*" is legal and means
	// "also read everything nested under me".
	if target == ns {
		return invalidInputf("namespace link: %q cannot link to itself", ns)
	}
	tiersNorm, err := normalizeLinkTiers(tiers)
	if err != nil {
		return err
	}

	existing, err := s.linkStore.ListNamespaceLinks(ctx, ns)
	if err != nil {
		return fmt.Errorf("namespace link: list existing links: %w", err)
	}
	isNew := true
	for _, l := range existing {
		if l.Target == target {
			isNew = false
			break
		}
	}
	if isNew && len(existing) >= maxNamespaceLinks {
		return invalidInputf("namespace link: %q already has %d links (max %d)",
			ns, len(existing), maxNamespaceLinks)
	}

	if err := s.linkStore.PutNamespaceLink(ctx, store.NamespaceLink{
		Namespace: ns, Target: target, Tiers: tiersNorm, CreatedAt: s.now(),
	}); err != nil {
		return fmt.Errorf("namespace link: %w", err)
	}
	return nil
}

// UnlinkNamespaces removes a link. Returns store.ErrNotFound when no such
// link exists (API layers map it to 404).
func (s *Service) UnlinkNamespaces(ctx context.Context, ns, target string) error {
	if s.linkStore == nil {
		return ErrLinksUnsupported
	}
	if err := httputil.ValidateNamespace(ns); err != nil {
		return invalidInputf("namespace unlink: invalid namespace %q: %v", ns, err)
	}
	bareTarget, _ := strings.CutSuffix(target, "/*")
	if err := httputil.ValidateNamespace(bareTarget); err != nil {
		return invalidInputf("namespace unlink: invalid target %q: %v", target, err)
	}
	return s.linkStore.DeleteNamespaceLink(ctx, ns, target)
}

// NamespaceLinks returns ns's outgoing links, ordered by target.
func (s *Service) NamespaceLinks(ctx context.Context, ns string) ([]store.NamespaceLink, error) {
	if s.linkStore == nil {
		return nil, ErrLinksUnsupported
	}
	if err := httputil.ValidateNamespace(ns); err != nil {
		return nil, invalidInputf("namespace links: invalid namespace %q: %v", ns, err)
	}
	return s.linkStore.ListNamespaceLinks(ctx, ns)
}
