package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/maintenance"
)

var (
	assessYes       bool
	assessBatch     int
	assessMaxPerRun int
	assessMinAge    time.Duration
)

var assessCmd = &cobra.Command{
	Use:   "assess",
	Short: "Backfill LLM-assessed importance on durable memories, on demand",
	Long: "Send durable (semantic / procedural) memories still sitting at their " +
		"tier-seed importance to the configured LLM for an intrinsic-importance " +
		"score, writing the result to assessed_importance.\n\n" +
		"This is the same pass MEMINI_ASSESS_INTERVAL runs on a timer, exposed as " +
		"a command so a deployment can populate the column without leaving an " +
		"hourly LLM job switched on — including deployments that keep " +
		"MEMINI_ASSESS_INTERVAL=0 permanently and point MEMINI_LLM_BASE_URL at a " +
		"model for this invocation alone.\n\n" +
		"Idempotent, and never second-guesses a human: a memory whose importance " +
		"differs from its tier seed was set deliberately and is skipped. " +
		"Dry-run by default.",
	RunE: runAssess,
}

func init() {
	assessCmd.Flags().BoolVar(&assessYes, "yes", false,
		"spend LLM calls and write the assessments (default is a dry-run count)")
	assessCmd.Flags().IntVar(&assessBatch, "batch", 0,
		"memories per LLM call (0 uses MEMINI_ASSESS_BATCH)")
	assessCmd.Flags().IntVar(&assessMaxPerRun, "max-per-run", 0,
		"cap rows touched by this pass (0 uses MEMINI_ASSESS_MAX_PER_RUN)")
	assessCmd.Flags().DurationVar(&assessMinAge, "min-age", 0,
		"skip memories younger than this (0 uses MEMINI_ASSESS_MIN_AGE)")
	rootCmd.AddCommand(assessCmd)
}

func runAssess(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	opts := maintenance.AssessOptions{
		Batch:     firstPositive(assessBatch, cfg.AssessBatch),
		MaxPerRun: firstPositive(assessMaxPerRun, cfg.AssessMaxPerRun),
		MinAge:    assessMinAge,
	}
	if opts.MinAge <= 0 {
		opts.MinAge = cfg.AssessMinAge
	}

	st, err := buildStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	out := cmd.OutOrStdout()
	now := time.Now().UTC()

	// Preview first even under --yes: the candidate count is what makes the
	// "N LLM calls" line honest, and it costs one store scan rather than a
	// model call.
	pending, err := maintenance.AssessImportancePreview(ctx, st, opts, now)
	if err != nil {
		return err
	}
	batch := firstPositive(opts.Batch, 20)
	calls := (pending + batch - 1) / batch

	if !assessYes {
		fmt.Fprintf(out, "assess importance: preview (re-run with --yes to apply)\n") //nolint:errcheck
		fmt.Fprintf(out, "  unassessed at tier seed: %d durable memories\n", pending) //nolint:errcheck
		fmt.Fprintf(out, "  would cost:              %d LLM call(s) at batch %d\n",   //nolint:errcheck
			calls, batch)
		if pending > 0 && !cfg.LLMEnabled() {
			fmt.Fprintf(out, //nolint:errcheck
				"  NOTE: no LLM configured — set MEMINI_LLM_BASE_URL (and MEMINI_LLM_MODEL)\n"+
					"        before --yes, or nothing will be assessed.\n")
		}
		return nil
	}

	if !cfg.LLMEnabled() {
		return fmt.Errorf("no LLM configured: set MEMINI_LLM_BASE_URL to run the assessment " +
			"(assessed importance is written only by LLM paths)")
	}
	extraBody, err := cfg.LLMExtraBodyMap()
	if err != nil {
		return err
	}
	client, err := llm.New(llm.API(cfg.LLMAPI), llm.Config{
		BaseURL:   cfg.LLMBaseURL,
		APIKey:    cfg.LLMAPIKey,
		Model:     cfg.LLMModel,
		MaxTokens: cfg.LLMMaxTokens,
		ExtraBody: extraBody,
	})
	if err != nil {
		return err
	}

	n, err := maintenance.AssessImportanceBackfill(ctx, st, client, opts, now)
	// Report the count before surfacing the error: the pass persists per batch,
	// so a mid-run failure still leaves real work committed and the operator
	// needs to know how much before deciding whether to re-run.
	fmt.Fprintf(out, "assess importance: applied\n")                    //nolint:errcheck
	fmt.Fprintf(out, "  assessed: %d of %d candidate(s)\n", n, pending) //nolint:errcheck
	if n < pending {
		fmt.Fprintf(out, "  remaining candidates are picked up by the next run\n") //nolint:errcheck
	}
	return err
}

// firstPositive returns v when it is set, else the configured fallback. Both
// zero means the maintenance package applies its own default.
func firstPositive(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}
