package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
)

// TestTurnCaptureBoilerplateHygiene pins the server-side capture hygiene: a
// write identified as a turn capture (metadata format="turn", the same
// predicate the recall echo guard uses; the Claude Code plugin also stamps
// source="turn_capture") has harness boilerplate stripped before the episodic
// value gate, so a boilerplate-only capture is value-gated instead of stored;
// any other write with the same markup is left alone.
func TestTurnCaptureBoilerplateHygiene(t *testing.T) {
	ctx := context.Background()
	ns := "alice"
	captureMeta := func() map[string]any {
		return map[string]any{"source": "turn_capture", "session_id": "sess-1", "format": "turn"}
	}
	prose := "User: deploy plan\n\nAssistant: " + strings.Repeat("postgres failover steps ", 10)
	markup := "<system-reminder>\nhook context echoed back\n</system-reminder>"

	t.Run("capture stripped before storage", func(t *testing.T) {
		svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce(), service.WithEpisodicMinChars(80))
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns, Content: prose + "\n\n" + markup,
			Tier: memory.TierEpisodic, Metadata: captureMeta(),
		})
		if err != nil || m == nil {
			t.Fatalf("substantive capture should persist, got m=%v err=%v", m, err)
		}
		if strings.Contains(m.Content, "<system-reminder>") {
			t.Fatalf("boilerplate survived stripping: %q", m.Content)
		}
		if want := strings.TrimSpace(prose); m.Content != want {
			t.Fatalf("stored content = %q, want the prose alone %q", m.Content, want)
		}
	})

	t.Run("boilerplate-only capture is value-gated", func(t *testing.T) {
		svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce(), service.WithEpisodicMinChars(80))
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns, Content: "ok\n\n" + markup,
			Tier: memory.TierEpisodic, Metadata: captureMeta(),
		})
		if err != nil {
			t.Fatalf("drop should not error: %v", err)
		}
		if m != nil {
			t.Fatalf("boilerplate-only capture should be value-gated (nil), got %q", m.Content)
		}
	})

	t.Run("capture emptied entirely is dropped even without the gate", func(t *testing.T) {
		svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce())
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns, Content: markup,
			Tier: memory.TierEpisodic, Metadata: captureMeta(),
		})
		if err != nil {
			t.Fatalf("drop should not error: %v", err)
		}
		if m != nil {
			t.Fatalf("emptied capture should be dropped (nil), got %q", m.Content)
		}
	})

	t.Run("capture from another integration is stripped too", func(t *testing.T) {
		svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce(), service.WithEpisodicMinChars(80))
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns, Content: prose + "\n\n" + markup,
			Tier:     memory.TierEpisodic,
			Metadata: map[string]any{"source": "pi", "format": "turn", "session_id": "sess-2"},
		})
		if err != nil || m == nil {
			t.Fatalf("substantive capture should persist, got m=%v err=%v", m, err)
		}
		if strings.Contains(m.Content, "<system-reminder>") {
			t.Fatalf("boilerplate survived stripping for a non-plugin capture: %q", m.Content)
		}
	})

	t.Run("non-capture write with the same markup is not stripped", func(t *testing.T) {
		svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce(), service.WithEpisodicMinChars(80))
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns, Content: prose + "\n\n" + markup,
			Tier: memory.TierEpisodic,
		})
		if err != nil || m == nil {
			t.Fatalf("non-capture write should persist, got m=%v err=%v", m, err)
		}
		if !strings.Contains(m.Content, "<system-reminder>") {
			t.Fatalf("non-capture write was stripped, stored %q", m.Content)
		}
	})
}
