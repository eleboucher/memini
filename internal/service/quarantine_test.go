package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/service"
)

// sanitizeRecorder is a service.Metrics that only records WriteSanitized; every
// other method is a no-op. Used to assert the ingestion-hygiene counter fires.
type sanitizeRecorder struct{ actions map[string]int }

func (r *sanitizeRecorder) WriteSanitized(action string) {
	if r.actions == nil {
		r.actions = map[string]int{}
	}
	r.actions[action]++
}

func (*sanitizeRecorder) ConsolidateResult(string)            {}
func (*sanitizeRecorder) ConsolidateQueueDepth(int)           {}
func (*sanitizeRecorder) RememberResult(string, string)       {}
func (*sanitizeRecorder) RecallResult(string, string, string) {}
func (*sanitizeRecorder) ForgetResult(string)                 {}
func (*sanitizeRecorder) SupersedeResult(string)              {}
func (*sanitizeRecorder) PromoteResult(string, int)           {}
func (*sanitizeRecorder) FsckResult(string)                   {}
func (*sanitizeRecorder) OpDuration(string, time.Duration)    {}
func (*sanitizeRecorder) AnswerResult(string)                 {}
func (*sanitizeRecorder) RerankResult(string, string)         {}
func (*sanitizeRecorder) RecallDegraded(string)               {}
func (*sanitizeRecorder) ReinforceResult(string)              {}
func (*sanitizeRecorder) DedupTombstoned(int)                 {}
func (*sanitizeRecorder) CorroborateResult(string)            {}
func (*sanitizeRecorder) TierClassified(string)               {}

// A garbled, script-salad digest of the kind an upstream model/harness glitch
// produces: Latin glued to CJK over and over. Triggers sanitize.Garbled.
const garbledContent = "Thank you I'm这a家b制c品d with在e上f世g纪h and的i more"

func TestRememberCleansControlChars(t *testing.T) {
	rec := &sanitizeRecorder{}
	svc := newService(t, service.WithMetrics(rec))
	m, err := svc.Remember(context.Background(), service.RememberInput{
		Namespace: "alice",
		Content:   "User: hello\x00\x07\nAssistant: ok\x7f",
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if got := m.Content; got != "User: hello\nAssistant: ok" {
		t.Fatalf("content not cleaned: %q", got)
	}
	if rec.actions["cleaned"] != 1 {
		t.Fatalf("WriteSanitized(cleaned) = %d, want 1", rec.actions["cleaned"])
	}
}

func TestRememberRejectsContentEmptyAfterClean(t *testing.T) {
	svc := newService(t)
	// Pure control bytes are non-empty on input but clean to nothing.
	_, err := svc.Remember(context.Background(), service.RememberInput{
		Namespace: "alice",
		Content:   "\x00\x01\x02\x7f",
	})
	if err == nil {
		t.Fatal("expected error for content empty after sanitization")
	}
	if !strings.Contains(err.Error(), "empty after sanitization") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQuarantineOffByDefaultKeepsGarbledWrite(t *testing.T) {
	svc := newService(t)
	m, err := svc.Remember(context.Background(), service.RememberInput{
		Namespace:  "alice",
		Content:    garbledContent,
		Importance: 0.8,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if m.Importance != 0.8 {
		t.Fatalf("importance changed with quarantine off: %v", m.Importance)
	}
	if _, ok := m.Metadata["quarantined"]; ok {
		t.Fatal("quarantined tag set with quarantine off")
	}
}

func TestQuarantineDownranksGarbledWrite(t *testing.T) {
	rec := &sanitizeRecorder{}
	svc := newService(t, service.WithCorruptionQuarantine(true), service.WithMetrics(rec))
	m, err := svc.Remember(context.Background(), service.RememberInput{
		Namespace:  "alice",
		Content:    garbledContent,
		Importance: 0.8,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if m.Importance != 0 {
		t.Fatalf("importance not zeroed for quarantined write: %v", m.Importance)
	}
	if m.Metadata["quarantined"] != true {
		t.Fatalf("quarantined tag not set: %v", m.Metadata["quarantined"])
	}
	if rec.actions["quarantined"] != 1 {
		t.Fatalf("WriteSanitized(quarantined) = %d, want 1", rec.actions["quarantined"])
	}
}

func TestQuarantineLeavesLegitMixedScriptAlone(t *testing.T) {
	svc := newService(t, service.WithCorruptionQuarantine(true))
	// Legitimate Chinese embedding a Latin tech term — must not be quarantined.
	m, err := svc.Remember(context.Background(), service.RememberInput{
		Namespace:  "alice",
		Content:    "使用React框架开发应用程序非常方便快捷高效",
		Importance: 0.8,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if m.Importance != 0.8 {
		t.Fatalf("legit content was downranked: %v", m.Importance)
	}
	if _, ok := m.Metadata["quarantined"]; ok {
		t.Fatal("legit content was quarantined")
	}
}
