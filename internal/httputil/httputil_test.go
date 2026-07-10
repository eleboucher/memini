package httputil

import "testing"

func TestNormalizeNamespace(t *testing.T) {
	cases := map[string]string{
		"work/memini":     "work/memini",
		"  work/memini  ": "work/memini",
		"/work/memini/":   "work/memini",
		"work//memini":    "work/memini",
		"work/_shared/":   "work/_shared",
		"///":             "",
		"  ":              "",
		"":                "",
		"work":            "work",
	}
	for in, want := range cases {
		if got := NormalizeNamespace(in); got != want {
			t.Errorf("NormalizeNamespace(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateNamespace(t *testing.T) {
	if err := ValidateNamespace("work/memini"); err != nil {
		t.Errorf("valid namespace rejected: %v", err)
	}
	if err := ValidateNamespace(""); err == nil {
		t.Error("empty namespace should be rejected")
	}
	if err := ValidateNamespace("bad\x00ns"); err == nil {
		t.Error("NUL-bearing namespace should be rejected")
	}
	long := make([]byte, 257)
	for i := range long {
		long[i] = 'x'
	}
	if err := ValidateNamespace(string(long)); err == nil {
		t.Error(">256-byte namespace should be rejected")
	}
}

// TestNormalizeThenValidate pins the middleware/read-path contract: a
// non-canonical but otherwise valid namespace normalizes to a form that
// passes validation, while a slash-only value collapses to empty (rejected).
func TestNormalizeThenValidate(t *testing.T) {
	if got := NormalizeNamespace("work/_shared/"); got != "work/_shared" || ValidateNamespace(got) != nil {
		t.Errorf("normalize+validate of %q = %q (err %v)", "work/_shared/", got, ValidateNamespace(got))
	}
	if got := NormalizeNamespace("///"); got != "" || ValidateNamespace(got) == nil {
		t.Errorf("slash-only should normalize to empty and fail validation, got %q", got)
	}
}
