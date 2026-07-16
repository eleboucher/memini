package service_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// seedForUpdate stores one fully-populated memory to edit.
func seedForUpdate(t *testing.T, svc *service.Service) *memory.Memory {
	t.Helper()
	m, err := svc.Remember(context.Background(), service.RememberInput{
		Namespace: "alice",
		Content:   "the deploy runbook lives in the ops wiki",
		Summary:   "runbook location",
		Tier:      memory.TierSemantic,
		Tags:      []string{"ops"},
		Metadata:  map[string]any{"source": "handbook", "reviewed": "no"},
	})
	if err != nil {
		t.Fatalf("seed remember: %v", err)
	}
	return m
}

func newUpdateService(t *testing.T) *service.Service {
	t.Helper()
	return service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce())
}

// TestUpdateOmittedFieldsAreKept is the core omit-to-keep contract: a caller
// changing one field must not have to resend the others to avoid losing them.
func TestUpdateOmittedFieldsAreKept(t *testing.T) {
	ctx := context.Background()
	svc := newUpdateService(t)
	seed := seedForUpdate(t, svc)

	got, err := svc.Update(ctx, service.UpdateInput{
		Namespace: "alice", ID: seed.ID, Tags: []string{"ops", "runbook"},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Content != seed.Content {
		t.Fatalf("Content = %q, want the stored content kept", got.Content)
	}
	if got.Summary != seed.Summary {
		t.Fatalf("Summary = %q, want the stored summary kept", got.Summary)
	}
	if got.Tier != seed.Tier {
		t.Fatalf("Tier = %q, want the stored tier kept", got.Tier)
	}
	if got.Metadata["source"] != "handbook" {
		t.Fatalf("Metadata[source] = %v, want the stored metadata kept", got.Metadata["source"])
	}
	if !slices.Equal(got.Tags, []string{"ops", "runbook"}) {
		t.Fatalf("Tags = %v, want the update applied", got.Tags)
	}
}

// TestUpdateClearsSummaryWithExplicitEmpty pins the behaviour the ""-sentinel
// form could not express: an explicit empty value is a write, not an omission.
// Before pointers, summary: "" was indistinguishable from "field absent" and was
// silently ignored, so a summary could never be cleared.
func TestUpdateClearsSummaryWithExplicitEmpty(t *testing.T) {
	ctx := context.Background()
	svc := newUpdateService(t)
	seed := seedForUpdate(t, svc)

	got, err := svc.Update(ctx, service.UpdateInput{
		Namespace: "alice", ID: seed.ID, Summary: new(""),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Summary != "" {
		t.Fatalf("Summary = %q, want cleared by an explicit empty string", got.Summary)
	}
}

// TestUpdateMergesMetadataKeyByKey: enriching one key must not drop the others,
// which is what a wholesale replace (POST /v1/memories with an id) would do.
func TestUpdateMergesMetadataKeyByKey(t *testing.T) {
	ctx := context.Background()
	svc := newUpdateService(t)
	seed := seedForUpdate(t, svc)

	got, err := svc.Update(ctx, service.UpdateInput{
		Namespace: "alice", ID: seed.ID,
		Metadata: map[string]any{"reviewed": "yes", "owner": "platform"},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Metadata["source"] != "handbook" {
		t.Fatalf("Metadata[source] = %v, want an untouched key preserved by the merge", got.Metadata["source"])
	}
	if got.Metadata["reviewed"] != "yes" {
		t.Fatalf("Metadata[reviewed] = %v, want overwritten", got.Metadata["reviewed"])
	}
	if got.Metadata["owner"] != "platform" {
		t.Fatalf("Metadata[owner] = %v, want added", got.Metadata["owner"])
	}
}

// TestUpdateNilMetadataValueDeletesKey: a nil value is a delete, which is
// otherwise impossible — the only alternative is a wholesale re-POST of every
// other key, which is both racy and lossy.
func TestUpdateNilMetadataValueDeletesKey(t *testing.T) {
	ctx := context.Background()
	svc := newUpdateService(t)
	seed := seedForUpdate(t, svc)

	got, err := svc.Update(ctx, service.UpdateInput{
		Namespace: "alice", ID: seed.ID,
		Metadata: map[string]any{"reviewed": nil},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, ok := got.Metadata["reviewed"]; ok {
		t.Fatalf("Metadata[reviewed] = %v, want deleted by an explicit nil value", got.Metadata["reviewed"])
	}
	if got.Metadata["source"] != "handbook" {
		t.Fatalf("Metadata[source] = %v, want the other keys untouched", got.Metadata["source"])
	}
}

// TestUpdateInvalidTierIsInvalidInput pins the error CLASS, not its text: the
// REST layer maps ErrInvalidInput to 400 and anything unrecognized to 500, so a
// bare fmt.Errorf here would surface a caller's typo as a server error.
func TestUpdateInvalidTierIsInvalidInput(t *testing.T) {
	ctx := context.Background()
	svc := newUpdateService(t)
	seed := seedForUpdate(t, svc)

	_, err := svc.Update(ctx, service.UpdateInput{
		Namespace: "alice", ID: seed.ID, Tier: new(memory.Tier("nonsense")),
	})
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput so the REST layer renders 400 rather than 500", err)
	}
}

// TestUpdateMissingIDIsNotFound: the sentinel must survive the Get+Remember
// composition so each surface can render its own 404.
func TestUpdateMissingIDIsNotFound(t *testing.T) {
	ctx := context.Background()
	svc := newUpdateService(t)

	_, err := svc.Update(ctx, service.UpdateInput{Namespace: "alice", ID: "does-not-exist"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want store.ErrNotFound", err)
	}
}

// TestUpdateAppliesContentAndTier covers the ordinary explicit-write path.
func TestUpdateAppliesContentAndTier(t *testing.T) {
	ctx := context.Background()
	svc := newUpdateService(t)
	seed := seedForUpdate(t, svc)

	const replacement = "the deploy runbook moved to the platform handbook"
	got, err := svc.Update(ctx, service.UpdateInput{
		Namespace: "alice", ID: seed.ID,
		Content: new(replacement), Tier: new(memory.TierProcedural),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Content != replacement {
		t.Fatalf("Content = %q, want %q", got.Content, replacement)
	}
	if got.Tier != memory.TierProcedural {
		t.Fatalf("Tier = %q, want procedural", got.Tier)
	}
	if got.ID != seed.ID {
		t.Fatalf("ID = %q, want the edit to stay in place on %q", got.ID, seed.ID)
	}
}

// TestUpdateDoesNotMutateStoredMetadataMap guards the fresh-map allocation in
// mergeMetadata. Remember writes into the Metadata map it is handed
// (stampClassifiedTier / scrubInput / embedForRemember), so reusing the map that
// came off the stored record would let one write scribble on another's data.
func TestUpdateDoesNotMutateStoredMetadataMap(t *testing.T) {
	ctx := context.Background()
	svc := newUpdateService(t)
	seed := seedForUpdate(t, svc)

	before := make(map[string]any, len(seed.Metadata))
	maps.Copy(before, seed.Metadata)
	if _, err := svc.Update(ctx, service.UpdateInput{
		Namespace: "alice", ID: seed.ID, Metadata: map[string]any{"owner": "platform"},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	for k, want := range before {
		if seed.Metadata[k] != want {
			t.Fatalf("the seed's Metadata[%q] changed to %v (want %v) — Update mutated a caller's map",
				k, seed.Metadata[k], want)
		}
	}
	if _, ok := seed.Metadata["owner"]; ok {
		t.Fatal("Update leaked its merge into the caller's metadata map")
	}
}
