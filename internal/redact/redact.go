// Package redact scrubs live credentials from text before it is persisted.
//
// A memory store inevitably records conversations and shell activity, so the
// realistic security goal is not "the content is invisible if the DB leaks"
// (it can't be — that's what the data is) but "the DB holds no usable
// credentials". Scrubbing secrets at every ingestion path bounds the blast
// radius of a database compromise to information disclosure, never lateral
// movement via leaked tokens/keys.
//
// The rules target high-signal, low-false-positive secret shapes. They are
// best-effort: an exotic credential format can slip through, so this is one
// layer of defense, not a guarantee.
package redact

import "regexp"

const marker = "[REDACTED]"

// keyValRe matches key=value / key: value where the key name implies a secret.
// Keeps the key and separator, redacts the value (up to the next space/quote).
// Split across the join rune so the combined string is the original regex.
const keyValRe = "(?i)([A-Za-z0-9_.-]*" +
	"(?:secret|password|passwd|token|api[_-]?key|" +
	"access[_-]?key|client[_-]?secret|auth[_-]?token)" +
	"[A-Za-z0-9_.-]*\\s*[=:]\\s*)(['\"]?)[^\\s\"']{4,}"

// rule pairs a pattern with its replacement template. Templates use ${n} group
// references (RE2 has no backreferences).
type rule struct {
	re   *regexp.Regexp
	repl string
}

// rules run in order: PEM blocks first (multi-line), then specific token
// formats, then the generic key=value and URL-userinfo shapes.
var rules = []rule{
	// PEM private key blocks (any key type), including the newlines between.
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), marker},
	// Provider-specific token formats with distinctive prefixes.
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), marker},                                              // AWS access key id
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`), marker},                                    // GitHub tokens
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`), marker},                                  // Slack tokens
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`), marker},                                         // OpenAI / Anthropic (sk-, sk-ant-)
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), marker},                                         // Google API key
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`), marker}, // JWTs
	// key=value / key: value where the key name implies a secret. Keeps the key
	// and separator, redacts the value (up to the next space/quote).
	{regexp.MustCompile(keyValRe), `${1}${2}` + marker},
	// HTTP "Authorization: Bearer <token>" — the space-separated form. Only
	// "bearer" (high-signal); the "token=" form is covered by the key=value rule
	// above, which avoids mangling prose like "token required".
	{regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`), `${1}` + marker},
	// URL userinfo: scheme://user:password@host -> scheme://user:[REDACTED]@host
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+:)[^/\s:@]+(@)`), `${1}` + marker + `${2}`},
}

// Secrets scrubs known secret shapes from s, returning the cleaned text. Empty
// input is returned unchanged.
func Secrets(s string) string {
	if s == "" {
		return s
	}
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// Metadata walks a metadata map and scrubs every string value in place
// (recursing into nested maps and slices), so secrets buried in structured
// fields — e.g. a digest's captured shell commands — are caught too. Returns
// the same map for call-site convenience; nil stays nil.
func Metadata(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	for k, v := range m {
		m[k] = value(v)
	}
	return m
}

func value(v any) any {
	switch t := v.(type) {
	case string:
		return Secrets(t)
	case map[string]any:
		return Metadata(t)
	case []any:
		for i, e := range t {
			t[i] = value(e)
		}
		return t
	default:
		return v
	}
}
