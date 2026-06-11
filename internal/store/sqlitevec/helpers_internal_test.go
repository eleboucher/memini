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
		// Pinned behavior: tokenize keeps only [a-z0-9], so non-ASCII content
		// is invisible to the FTS leg (recall survives via the vector leg, and
		// the postgres backend's to_tsquery behaves differently). If this is
		// ever made unicode-aware, revisit backend parity in storetest.
		{"café au lait", []string{"caf", "au", "lait"}},
		{"東京タワー", nil},
		{"cat's toy", []string{"cat", "toy"}},
		{"naïve user", []string{"na", "ve", "user"}},
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
		// FTS5 metacharacters and operators are neutralized by construction:
		// every term is tokenized to [a-z0-9] and quoted, so user input can
		// never inject MATCH syntax.
		{`NEAR(foo bar)`, `"near" OR "foo" OR "bar"`},
		{`"quoted phrase"`, `"quoted" OR "phrase"`},
		{`col:value AND x*`, `"col" OR "value" OR "and"`},
		{"東京タワー", ""}, // non-ASCII query yields no FTS terms
	}
	for _, tt := range tests {
		if got := ftsQuery(tt.in); got != tt.want {
			t.Errorf("ftsQuery(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
