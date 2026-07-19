package main

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// gatheredCounter pulls one counter's value out of a registry gather by metric
// name and exact label set; ok is false when no such series was collected.
func gatheredCounter(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			if len(got) != len(labels) {
				continue
			}
			match := true
			for k, v := range labels {
				if got[k] != v {
					match = false
					break
				}
			}
			if match {
				return m.GetCounter().GetValue(), true
			}
		}
	}
	return 0, false
}

// TestInjectedMetrics covers the injection-telemetry collectors: one report's
// buckets land on memini_injected_memories_total{surface,result} with the
// injected count and each suppressed_* count, and the client token estimate
// accumulates on memini_injected_tokens_total{surface}. Asserted through a
// registry gather, so registration is proven along with the values.
func TestInjectedMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newConsolidateMetrics(reg)

	// One report: 3 injected, 2 suppressed by seen, 1 by score, 412 tokens.
	m.InjectedResult("prompt", "injected", 3)
	m.InjectedResult("prompt", "suppressed_seen", 2)
	m.InjectedResult("prompt", "suppressed_score", 1)
	m.InjectedTokens("prompt", 412)
	// A later briefing report accumulates independently per surface.
	m.InjectedResult("briefing", "injected", 5)
	m.InjectedTokens("briefing", 100)
	m.InjectedTokens("briefing", 50)

	for _, tc := range []struct {
		surface, result string
		want            float64
	}{
		{"prompt", "injected", 3},
		{"prompt", "suppressed_seen", 2},
		{"prompt", "suppressed_score", 1},
		{"briefing", "injected", 5},
	} {
		got, ok := gatheredCounter(t, reg, "memini_injected_memories_total",
			map[string]string{"surface": tc.surface, "result": tc.result})
		if !ok || got != tc.want {
			t.Errorf("memini_injected_memories_total{surface=%q,result=%q} = %v (found=%v), want %v",
				tc.surface, tc.result, got, ok, tc.want)
		}
	}
	if got, ok := gatheredCounter(t, reg, "memini_injected_tokens_total",
		map[string]string{"surface": "prompt"}); !ok || got != 412 {
		t.Errorf("memini_injected_tokens_total{surface=prompt} = %v (found=%v), want 412", got, ok)
	}
	if got, ok := gatheredCounter(t, reg, "memini_injected_tokens_total",
		map[string]string{"surface": "briefing"}); !ok || got != 150 {
		t.Errorf("memini_injected_tokens_total{surface=briefing} = %v (found=%v), want 150", got, ok)
	}
}
