package contradict

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		name    string
		oldText string
		newText string
		want    Class
	}{
		// Restatements that must never read as updates.
		{"identical", "We use Postgres.", "we use  postgres.", Restatement},
		{"entity alias postgres/postgresql",
			"The project stores vectors in Postgres with the pgvector extension.",
			"Vectors are stored in PostgreSQL with the pgvector extension.",
			Restatement},
		{"one-sided added number is added detail, not a swap",
			"Retries use exponential backoff.",
			"Retries use exponential backoff capped at five attempts.",
			Restatement},
		{"unit alias 10 MB vs 10 megabytes",
			"Image uploads are limited to 10 megabytes.",
			"Image uploads are limited to 10 MB.",
			Restatement},
		{"time alias 02:00 vs 2am",
			"The search index is rebuilt nightly at 02:00 UTC.",
			"The search index is rebuilt nightly at 2am UTC.",
			Restatement},
		{"number word vs digit",
			"Cache entries expire after a 10 minute TTL.",
			"Cache entries expire after a ten minute TTL.",
			Restatement},
		{"reordered list",
			"The frontend is built with React and Vite.",
			"The frontend is built with Vite and React.",
			Restatement},
		{"positive rephrase of a negated fact must not flip",
			"Pagination uses opaque cursors, not page numbers.",
			"Pagination uses opaque cursors rather than page numbers.",
			Restatement},

		// Distinct same-topic facts that must not fire.
		{"shared entity, different attribute",
			"The project stores vectors in Postgres with the pgvector extension.",
			"The project runs database migrations against Postgres with golang-migrate.",
			Distinct},
		{"negation with substantive residue",
			"Retries use exponential backoff capped at five attempts.",
			"Retries are not enabled for POST requests.",
			Distinct},
		{"same topic, different claim with numbers",
			"Database backups run hourly and are kept for seven days.",
			"Database backups are encrypted at rest with a KMS key.",
			Distinct},
		{"unrelated", "Email is sent through Postmark.", "The queue worker pool size is eight.", Distinct},

		// Genuine updates that must fire.
		{"number value swap",
			"Cache entries expire after a 10 minute TTL.",
			"Cache entries expire after a 30 minute TTL.",
			Update},
		{"number word to digit swap",
			"Cache entries expire after a ten minute TTL.",
			"Cache entries now expire after a 30 minute TTL.",
			Update},
		{"entity value swap",
			"Email is sent through Postmark.",
			"Email is sent through SES.",
			Update},
		{"entity swap with retro cue",
			"Background jobs run on NATS JetStream.",
			"Background jobs switched from NATS JetStream to Kafka.",
			Update},
		{"retraction with retro cue",
			"Background jobs run on NATS JetStream.",
			"We no longer run background jobs on NATS JetStream.",
			Update},
		{"bare negation retraction",
			"Background jobs run on NATS JetStream.",
			"Background jobs do not run on NATS JetStream.",
			Update},
		{"no-longer flips a restriction",
			"The admin UI is restricted to the internal network only.",
			"The admin UI is no longer restricted to the internal network.",
			Update},
		{"stopped-doing process change",
			"The team does code review on every change before merge.",
			"The team stopped doing code review on docs-only changes.",
			Update},
		{"port value swap",
			"The reranker is Qwen3-Reranker-0.6B served on port 8002.",
			"The reranker is Qwen3-Reranker-0.6B served on port 9002.",
			Update},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.newText, tc.oldText, Default)
			if got.Class != tc.want {
				t.Errorf("Classify(%q, %q) = %v (%s), want %v",
					tc.newText, tc.oldText, got.Class, got.Reason, tc.want)
			}
		})
	}
}

// TestClassifyGoGolangResidualRisk pins the accepted residual risk: "Go" and
// "Golang" are too short for the alias prefix rule, but a swap needs a changed
// value on BOTH sides, so the pair alone cannot fire as an update.
func TestClassifyGoGolangResidualRisk(t *testing.T) {
	got := Classify(
		"The primary language for services is Golang.",
		"The primary language for services is Go.",
		Default)
	if got.Class == Update {
		t.Errorf("go/golang rename classified as update (%s)", got.Reason)
	}
}
