package maintenance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// Importance-assessment knobs. Defaults are picked so a pass costs a bounded,
// predictable amount of LLM budget: a backlog drains over successive passes
// (oldest first) rather than in one expensive burst.
const (
	defaultAssessBatch     = 20
	defaultAssessMaxPerRun = 200
	defaultAssessMinAge    = time.Hour
)

// Assessed-importance clamp bounds, kept in step with service's
// clampAssessedImportance: the 0.9 cap stops an over-eager model saturating the
// scale (and still clears the 0.75 demote bar), the 0.1 floor keeps a score out
// of the 0-sentinel dead zone that marks a quarantined write. Duplicated as two
// literals rather than exported, so neither package has to depend on the other.
const (
	minAssessedImportance = 0.1
	maxAssessedImportance = 0.9
)

// AssessOptions configures one importance-backfill pass.
type AssessOptions struct {
	// Batch is how many memory contents go into a single LLM call. 0 falls back
	// to defaultAssessBatch.
	Batch int
	// MaxPerRun caps the rows assessed by one pass, bounding its LLM spend.
	// 0 falls back to defaultAssessMaxPerRun.
	MaxPerRun int
	// MinAge skips memories younger than this, so the sweep never races the
	// write path's own assessment (a fresh write is rated inline by the
	// distill/consolidate call). 0 falls back to defaultAssessMinAge.
	MinAge time.Duration
	// Log receives progress messages; nil falls back to slog.Default().
	Log *slog.Logger
}

// assessCandidate is one row selected for assessment, carrying the namespace
// the write-back needs. Batches are filled across namespaces, so the namespace
// travels with the row rather than framing the loop.
type assessCandidate struct {
	namespace string
	id        string
	content   string
	createdAt time.Time
}

// AssessImportanceBackfill stamps LLM-assessed importance onto durable memories
// that never received one, and returns how many rows it assessed.
//
// A row is a candidate only when it has no assessment yet, is older than
// MinAge, and still carries exactly its tier's seed importance — a memory whose
// importance differs from the seed was set deliberately by whoever wrote it and
// is never second-guessed. (A user who happens to set exactly the seed value is
// a knowingly accepted false positive: the assessment would then replace that
// number with the model's, but ranking read the identical value beforehand, so
// nothing the user chose is lost in practice.) Candidates are assessed
// oldest-first and capped at MaxPerRun, so a large backlog drains predictably
// over successive passes.
//
// The pass is deliberately forgiving of a flaky LLM. If the very first batch
// fails the whole pass is abandoned with a single warning — the provider is
// most likely down, and hammering it with the remaining batches would only
// multiply the noise. A later batch failing is logged and skipped, keeping the
// scores already written. Either way the unassessed rows stay NULL and are
// picked up again next pass, as does any row the model explicitly declined to
// rate. Because "LLM is unavailable right now" is the expected state rather
// than a fault, that abandoned pass reports (0, nil): the warning is the
// report, and returning an error would make callers log it twice.
func AssessImportanceBackfill(
	ctx context.Context, st store.Store, a llm.ImportanceAssessor, opts AssessOptions, now time.Time,
) (int, error) {
	if a == nil {
		return 0, nil
	}
	if opts.Batch <= 0 {
		opts.Batch = defaultAssessBatch
	}
	if opts.MaxPerRun <= 0 {
		opts.MaxPerRun = defaultAssessMaxPerRun
	}
	if opts.MinAge <= 0 {
		opts.MinAge = defaultAssessMinAge
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	cands, err := assessCandidates(ctx, st, opts, now)
	if err != nil {
		return 0, err
	}

	total := 0
	for start := 0; start < len(cands); start += opts.Batch {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		batch := cands[start:min(start+opts.Batch, len(cands))]
		texts := make([]string, len(batch))
		for i, c := range batch {
			texts[i] = c.content
		}
		scores, err := a.AssessImportance(ctx, texts)
		if err == nil && len(scores) != len(batch) {
			// Scores are positional: a mismatched length means no score can be
			// attributed to a memory with confidence, so the batch is a loss.
			err = fmt.Errorf("assessor returned %d scores for %d memories", len(scores), len(batch))
		}
		if err != nil {
			if start == 0 {
				log.WarnContext(ctx, "importance assessment: LLM unavailable, deferring", "error", err)
				return 0, nil
			}
			log.WarnContext(ctx, "importance assessment batch failed, skipping",
				"error", err, "batch_size", len(batch))
			continue
		}
		n, err := persistAssessments(ctx, st, batch, scores, now)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// assessCandidates collects the rows eligible for assessment across every
// namespace, oldest-first and capped at opts.MaxPerRun.
func assessCandidates(
	ctx context.Context, st store.Store, opts AssessOptions, now time.Time,
) ([]assessCandidate, error) {
	namespaces, err := st.ListNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	var cands []assessCandidate
	for _, ns := range namespaces {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		mems, err := st.List(ctx, ns, store.Filter{Tiers: longTermTiers, Now: now}, 0)
		if err != nil {
			return nil, err
		}
		for _, m := range mems {
			if m.AssessedImportance != nil {
				continue
			}
			if now.Sub(m.CreatedAt) <= opts.MinAge {
				continue
			}
			if m.Importance != memory.SeedImportance(m.Tier) {
				continue
			}
			cands = append(cands, assessCandidate{
				namespace: ns, id: m.ID, content: m.Content, createdAt: m.CreatedAt,
			})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		return cands[i].createdAt.Before(cands[j].createdAt)
	})
	if len(cands) > opts.MaxPerRun {
		cands = cands[:opts.MaxPerRun]
	}
	return cands, nil
}

// persistAssessments writes one batch's scores, returning how many rows it
// stamped. A nil score is the model declining that memory: the row keeps its
// NULL assessment and comes back next pass. A row that vanished under the pass
// is skipped rather than failing the batch.
func persistAssessments(
	ctx context.Context, st store.Store, batch []assessCandidate, scores []*float64, now time.Time,
) (int, error) {
	n := 0
	for i, c := range batch {
		if scores[i] == nil {
			continue
		}
		if err := st.SetAssessedImportance(ctx, c.namespace, c.id, clampAssessed(*scores[i]), now); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return n, err
		}
		n++
	}
	return n, nil
}

// clampAssessed bounds an LLM-assessed importance to [0.1, 0.9].
func clampAssessed(v float64) float64 {
	switch {
	case v < minAssessedImportance:
		return minAssessedImportance
	case v > maxAssessedImportance:
		return maxAssessedImportance
	default:
		return v
	}
}

// AssessJob is a periodic LLM importance-backfill pass. With interval <= 0, Run
// is a no-op (the function returns immediately).
type AssessJob struct {
	store    store.Store
	assessor llm.ImportanceAssessor
	log      *slog.Logger
	interval time.Duration
	opts     AssessOptions
}

// NewAssessJob builds an AssessJob that calls AssessImportanceBackfill(opts)
// every interval. interval <= 0 disables the job, as does a nil assessor.
func NewAssessJob(st store.Store, a llm.ImportanceAssessor, log *slog.Logger,
	interval time.Duration, opts AssessOptions) *AssessJob {
	return &AssessJob{
		store:    st,
		assessor: a,
		log:      log,
		interval: interval,
		opts:     opts,
	}
}

// Run loops on a ticker until ctx is cancelled. It runs one pass immediately
// and again on every tick. It is a no-op if the job was built with
// interval <= 0 or without an assessor.
func (j *AssessJob) Run(ctx context.Context) {
	if j.interval <= 0 || j.assessor == nil {
		return
	}
	j.pass(ctx)
	t := time.NewTicker(j.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.pass(ctx)
		}
	}
}

func (j *AssessJob) pass(ctx context.Context) {
	opts := j.opts
	opts.Log = j.log
	n, err := AssessImportanceBackfill(ctx, j.store, j.assessor, opts, time.Now().UTC())
	if err != nil {
		j.log.WarnContext(ctx, "importance assessment pass failed", "error", err)
		return
	}
	if n > 0 {
		j.log.InfoContext(ctx, "assessed memory importance", "assessed", n)
	}
}
