package service

import "testing"

func TestStripCaptureBoilerplate(t *testing.T) {
	banner := "★ Insight ─────────────────────────────────────\n" +
		"stylized commentary the harness decorated the answer with\n" +
		"─────────────────────────────────────────────────"

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "memini-context block stripped",
			content: "before\n\n<memini-context session=\"abc\">\ninjected recall\n</memini-context>\n\nafter",
			want:    "before\n\nafter",
		},
		{
			name:    "memini-recall block stripped",
			content: "before\n\n<memini-recall query=\"x\">\nhits\n</memini-recall>\n\nafter",
			want:    "before\n\nafter",
		},
		{
			name:    "memini-pretool block stripped",
			content: "before\n\n<memini-pretool tool=\"Read\" read-only>\nrelated memories\n</memini-pretool>\n\nafter",
			want:    "before\n\nafter",
		},
		{
			name:    "memini-memory-directive block stripped",
			content: "before\n\n<memini-memory-directive>\nsave things\n</memini-memory-directive>\n\nafter",
			want:    "before\n\nafter",
		},
		{
			name:    "memini-compact-recovery block stripped",
			content: "before\n\n<memini-compact-recovery>\nrestored context\n</memini-compact-recovery>\n\nafter",
			want:    "before\n\nafter",
		},
		{
			name:    "system-reminder block stripped",
			content: "before\n\n<system-reminder>\nhook context\n</system-reminder>\n\nafter",
			want:    "before\n\nafter",
		},
		{
			name:    "unterminated block strips to end",
			content: "the real answer\n\n<system-reminder>\nnever closed",
			want:    "the real answer",
		},
		{
			name:    "insight banner with closing line stripped whole",
			content: "prose above\n" + banner + "\nprose below",
			want:    "prose above\nprose below",
		},
		{
			name:    "insight banner without closing line strips to end",
			content: "prose above\n★ Insight ─────────────\nbody that never closes\nstill body",
			want:    "prose above",
		},
		{
			name: "mixed content preserves surrounding prose",
			content: "first paragraph\n\n" + banner + "\n\nsecond paragraph\n\n" +
				"<memini-context>echo</memini-context>\n\nthird paragraph",
			want: "first paragraph\n\nsecond paragraph\n\nthird paragraph",
		},
		{
			name:    "newline runs collapse to two",
			content: "a\n<system-reminder>x</system-reminder>\n\n\n<system-reminder>y</system-reminder>\n\nb",
			want:    "a\n\nb",
		},
		{
			name:    "boilerplate only becomes empty",
			content: "<memini-context>\nall noise\n</memini-context>\n\n<system-reminder>more</system-reminder>",
			want:    "",
		},
		{
			name:    "tags are case-sensitive",
			content: "before <SYSTEM-REMINDER>shouty</SYSTEM-REMINDER> after",
			want:    "before <SYSTEM-REMINDER>shouty</SYSTEM-REMINDER> after",
		},
		{
			name:    "dash line without insight opener kept",
			content: "a\n─────────\nb",
			want:    "a\n─────────\nb",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripCaptureBoilerplate(tt.content); got != tt.want {
				t.Fatalf("stripCaptureBoilerplate(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}

	t.Run("clean text untouched", func(t *testing.T) {
		clean := "User: how do we deploy?\n\nAssistant: via the runbook, step by step.\n"
		if got := stripCaptureBoilerplate(clean); got != clean {
			t.Fatalf("clean text must round-trip unchanged, got %q", got)
		}
	})
}
