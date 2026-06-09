package sqlitevec

import (
	"reflect"
	"testing"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"Hello World", []string{"hello", "world"}},
		{"a bb ccc", []string{"bb", "ccc"}}, // single-char terms dropped
		{"foo-bar.baz", []string{"foo", "bar", "baz"}},
		{"  ", nil},
		{"MixedCASE123", []string{"mixedcase123"}},
	}
	for _, tt := range tests {
		if got := tokenize(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("tokenize(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestFtsQuery(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"the quick fox", `"the" OR "quick" OR "fox"`},
		{"single", `"single"`},
		{"", ""},
		{"a", ""}, // no usable terms
	}
	for _, tt := range tests {
		if got := ftsQuery(tt.in); got != tt.want {
			t.Errorf("ftsQuery(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
