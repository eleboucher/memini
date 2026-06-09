package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/search"
	"github.com/eleboucher/memini/internal/store"
)

// consolidateCandidates is how many near-neighbours are offered to the LLM when
// deciding whether a new memory is novel, a refinement, or a contradiction.
const consolidateCandidates = 5

// Service wires storage and embeddings together. It is safe for concurrent use.
type Service struct {
	store    store.Store
	embedder embed.Embedder
	// consolidator is optional; when set, durable writes are deduplicated and
	// contradiction-resolved against existing memories.
	consolidator llm.Consolidator

	// shortTermCap bounds short-term memories per namespace during fsck (0 = off).
	shortTermCap int
	// now and newID are injectable for deterministic tests.
	now   func() time.Time
	newID func() string
}

// Option customizes a Service.
type Option func(*Service)

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithIDGenerator overrides ID generation (tests).
func WithIDGenerator(gen func() string) Option { return func(s *Service) { s.newID = gen } }

// WithConsolidator enables the opt-in LLM consolidation pipeline.
func WithConsolidator(c llm.Consolidator) Option { return func(s *Service) { s.consolidator = c } }

// WithShortTermCap bounds short-term memories per namespace, enforced by fsck.
func WithShortTermCap(cap int) Option { return func(s *Service) { s.shortTermCap = cap } }

// New builds a Service from a store and embedder.
func New(st store.Store, e embed.Embedder, opts ...Option) *Service {
	s := &Service{
		store:    st,
		embedder: e,
		now:      func() time.Time { return time.Now().UTC() },
		newID:    func() string { return uuid.NewString() },
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// RememberInput describes a memory to store. Only Namespace and Content are
// required; Tier defaults to working and TTL to the tier default.
type RememberInput struct {
	Namespace  string
	Content    string
	Tier       memory.Tier
	Summary    string
	Tags       []string
	Metadata   map[string]any
	Importance float64
	// TTL overrides the tier default. A negative TTL means "never expire".
	TTL *time.Duration
	// ID upserts an existing memory when set; otherwise a new ID is generated.
	ID string
}

// Remember embeds and stores a memory, returning the persisted record.
func (s *Service) Remember(ctx context.Context, in RememberInput) (*memory.Memory, error) {
	if in.Namespace == "" {
		return nil, fmt.Errorf("remember: namespace is required")
	}
	if in.Content == "" {
		return nil, fmt.Errorf("remember: content is required")
	}
	tier := in.Tier
	if tier == "" {
		tier = memory.TierWorking
	}
	if !tier.Valid() {
		return nil, fmt.Errorf("remember: invalid tier %q", tier)
	}

	vec, err := embed.EmbedOne(ctx, s.embedder, in.Content)
	if err != nil {
		return nil, fmt.Errorf("remember: embed: %w", err)
	}

	now := s.now()
	id := in.ID
	if id == "" {
		id = s.newID()
	}
	m := &memory.Memory{
		ID:             id,
		Namespace:      in.Namespace,
		Tier:           tier,
		Content:        in.Content,
		Summary:        in.Summary,
		Tags:           in.Tags,
		Metadata:       in.Metadata,
		Importance:     in.Importance,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastAccessedAt: now,
		Embedding:      vec,
	}
	if exp := resolveExpiry(now, tier, in.TTL); exp != nil {
		m.ExpiresAt = exp
	}

	// Opt-in consolidation: on fresh writes to durable tiers, let the LLM dedup
	// or contradiction-resolve against existing memories.
	if in.ID == "" && s.consolidator != nil && (tier == memory.TierSemantic || tier == memory.TierProcedural) {
		if result, handled, err := s.consolidate(ctx, m); err != nil {
			return nil, err
		} else if handled {
			return result, nil
		}
	}

	if err := s.store.Upsert(ctx, m); err != nil {
		return nil, fmt.Errorf("remember: store: %w", err)
	}
	return m, nil
}

// consolidate runs the LLM pipeline for a new durable memory m. It returns
// (result, true, nil) when it has fully handled the write (update/supersede),
// or (nil, false, nil) to fall through to a normal insert.
func (s *Service) consolidate(ctx context.Context, m *memory.Memory) (*memory.Memory, bool, error) {
	cands, err := s.store.VectorSearch(ctx, m.Namespace, m.Embedding,
		store.Filter{Tiers: []memory.Tier{memory.TierSemantic, memory.TierProcedural}}, consolidateCandidates)
	if err != nil {
		return nil, false, fmt.Errorf("remember: consolidate search: %w", err)
	}
	if len(cands) == 0 {
		return nil, false, nil
	}

	in := llm.Input{New: m.Content, Tier: string(m.Tier)}
	for _, c := range cands {
		in.Candidates = append(in.Candidates, llm.Candidate{ID: c.Memory.ID, Content: c.Memory.Content})
	}
	dec, err := s.consolidator.Consolidate(ctx, in)
	if err != nil {
		// LLM is best-effort; a transient failure must not lose the write.
		slog.WarnContext(ctx, "consolidation failed, storing raw", "err", err)
		return nil, false, nil
	}

	switch dec.Action {
	case llm.ActionUpdate:
		return s.applyUpdate(ctx, m, dec)
	case llm.ActionSupersede:
		return s.applySupersede(ctx, m, dec)
	default: // ActionNew
		return nil, false, nil
	}
}

// applyUpdate merges the new memory into an existing target, re-embedding the
// merged content. Falls through to a normal insert if the target is gone.
func (s *Service) applyUpdate(ctx context.Context, m *memory.Memory, dec llm.Decision) (*memory.Memory, bool, error) {
	if dec.Target == "" {
		return nil, false, nil
	}
	target, err := s.store.Get(ctx, m.Namespace, dec.Target)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	content := dec.Content
	if content == "" {
		content = m.Content
	}
	vec, err := embed.EmbedOne(ctx, s.embedder, content)
	if err != nil {
		return nil, false, fmt.Errorf("remember: re-embed merged memory: %w", err)
	}
	target.Content = content
	if dec.Summary != "" {
		target.Summary = dec.Summary
	}
	target.UpdatedAt = s.now()
	target.Embedding = vec
	if err := s.store.Upsert(ctx, target); err != nil {
		return nil, false, err
	}
	return target, true, nil
}

// applySupersede stores the new memory and tombstones the contradicted target.
func (s *Service) applySupersede(ctx context.Context, m *memory.Memory, dec llm.Decision) (*memory.Memory, bool, error) {
	if err := s.store.Upsert(ctx, m); err != nil {
		return nil, false, err
	}
	if dec.Target != "" {
		if err := s.store.SetSuperseded(ctx, m.Namespace, dec.Target, m.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			// Best-effort rollback: remove the new memory so we don't leave both
			// live. If this also fails, the next fsck will flag the duplicate.
			_ = s.store.Delete(ctx, m.Namespace, m.ID)
			return nil, false, err
		}
	}
	return m, true, nil
}

// RecallInput describes a hybrid recall query.
type RecallInput struct {
	Namespace string
	Query     string
	Tiers     []memory.Tier
	Limit     int
	// IncludeExpired / IncludeSuperseded relax the default live-only filter.
	IncludeExpired    bool
	IncludeSuperseded bool
}

// Recall runs hybrid (vector + keyword) retrieval fused with RRF.
func (s *Service) Recall(ctx context.Context, in RecallInput) ([]store.Scored, error) {
	if in.Namespace == "" {
		return nil, fmt.Errorf("recall: namespace is required")
	}
	if in.Query == "" {
		return nil, fmt.Errorf("recall: query is required")
	}
	k := in.Limit
	if k <= 0 {
		k = 10
	}
	filter := store.Filter{
		Tiers:             in.Tiers,
		IncludeExpired:    in.IncludeExpired,
		IncludeSuperseded: in.IncludeSuperseded,
	}

	vec, err := embed.EmbedOne(ctx, s.embedder, in.Query)
	if err != nil {
		return nil, fmt.Errorf("recall: embed: %w", err)
	}

	vres, err := s.store.VectorSearch(ctx, in.Namespace, vec, filter, k)
	if err != nil {
		return nil, fmt.Errorf("recall: vector search: %w", err)
	}
	kres, err := s.store.KeywordSearch(ctx, in.Namespace, in.Query, filter, k)
	if err != nil {
		return nil, fmt.Errorf("recall: keyword search: %w", err)
	}
	fused := search.Fuse([][]store.Scored{vres, kres}, k, search.DefaultRRFK)
	s.reinforce(ctx, in.Namespace, fused)
	return fused, nil
}

// reinforce records that recalled memories were just used: it bumps their
// access stats and slides the TTL forward for short-term tiers, so frequently
// recalled memories don't decay. Best-effort — a failure never fails the recall.
func (s *Service) reinforce(ctx context.Context, namespace string, results []store.Scored) {
	now := s.now()
	byTier := map[memory.Tier][]string{}
	for _, r := range results {
		byTier[r.Memory.Tier] = append(byTier[r.Memory.Tier], r.Memory.ID)
	}
	for tier, ids := range byTier {
		ttl := tier.DefaultTTL()
		var newExpiry *time.Time
		if ttl > 0 { // short-term tiers slide their expiry forward on use
			t := now.Add(ttl)
			newExpiry = &t
		}
		_ = s.store.Reinforce(ctx, namespace, ids, now, newExpiry)
	}
}

// Get returns a single memory by ID.
func (s *Service) Get(ctx context.Context, namespace, id string) (*memory.Memory, error) {
	return s.store.Get(ctx, namespace, id)
}

// Forget deletes a memory by ID.
func (s *Service) Forget(ctx context.Context, namespace, id string) error {
	return s.store.Delete(ctx, namespace, id)
}

// Fsck runs a consistency sweep: purge expired, enforce the short-term cap, and
// audit live memories for duplicate clusters.
func (s *Service) Fsck(ctx context.Context) (maintenance.Report, error) {
	return maintenance.Fsck(ctx, s.store, s.shortTermCap, s.now())
}

// resolveExpiry computes the absolute expiry from an optional TTL override and
// the tier default. A negative override disables expiry.
func resolveExpiry(now time.Time, tier memory.Tier, ttl *time.Duration) *time.Time {
	d := tier.DefaultTTL()
	if ttl != nil {
		if *ttl < 0 {
			return nil
		}
		d = *ttl
	}
	if d <= 0 {
		return nil
	}
	t := now.Add(d)
	return &t
}
