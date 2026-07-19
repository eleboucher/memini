package store_test

import (
	"testing"

	"github.com/eleboucher/memini/internal/store"
)

// TestValidEventKind pins the recorded-kind vocabulary, and in particular that
// the injection-telemetry kind ("inject") is recognized: the REST ?kind=
// filter validates through ValidEventKind, so a missing case here would 400
// the very filter the spec advertises.
func TestValidEventKind(t *testing.T) {
	for _, k := range []store.EventKind{
		store.EventRecall, store.EventGet, store.EventBriefing, store.EventRemember,
		store.EventUpdate, store.EventForget, store.EventSupersede,
		store.EventPin, store.EventUnpin, store.EventSettings, store.EventInject,
	} {
		if !store.ValidEventKind(k) {
			t.Errorf("ValidEventKind(%q) = false, want true", k)
		}
	}
	if store.ValidEventKind(store.EventInject) != true || store.EventInject != "inject" {
		t.Errorf("EventInject = %q, want the wire value \"inject\"", store.EventInject)
	}
	for _, k := range []store.EventKind{"", "bogus", "injected"} {
		if store.ValidEventKind(k) {
			t.Errorf("ValidEventKind(%q) = true, want false", k)
		}
	}
}
