package service

import (
	"testing"
	"time"
)

// TestWithDistillTimeout verifies the write-time distillation deadline
// defaults to distillOnWriteTimeout and only positive overrides apply, so a
// zero-value config can never leave distillation unbounded or instantly
// cancelled.
func TestWithDistillTimeout(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
		want time.Duration
	}{
		{"default", nil, distillOnWriteTimeout},
		{"override", []Option{WithDistillTimeout(5 * time.Minute)}, 5 * time.Minute},
		{"zero keeps default", []Option{WithDistillTimeout(0)}, distillOnWriteTimeout},
		{"negative keeps default", []Option{WithDistillTimeout(-time.Second)}, distillOnWriteTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(nil, nil, tc.opts...)
			if s.distillTimeout != tc.want {
				t.Fatalf("distillTimeout = %v, want %v", s.distillTimeout, tc.want)
			}
		})
	}
}
