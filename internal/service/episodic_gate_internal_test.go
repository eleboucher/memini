package service

import (
	"strings"
	"testing"
)

func TestEpisodicSignalChars(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
	}{
		{"plain trivial", "keep going", 10},
		{"role labels stripped", "User: keep going\nAssistant: ok", len("keep going\nok")},
		{"lowercase labels stripped", "user: yes\nassistant: done", len("yes\ndone")},
		{"whitespace only", "   \n\t ", 0},
		{"substantive kept whole", strings.Repeat("a", 200), 200},
		{"non-label colon not stripped", "airflow: restarted the scheduler", len("airflow: restarted the scheduler")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := episodicSignalChars(tt.content); got != tt.want {
				t.Fatalf("episodicSignalChars(%q) = %d, want %d", tt.content, got, tt.want)
			}
		})
	}
}
