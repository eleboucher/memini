package redact

import "testing"

func TestRedactSecrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"no secret prose", "Refactored the auth module and ran the tests.", "Refactored the auth module and ran the tests."},
		{"token word in prose untouched", "the access token expires soon", "the access token expires soon"},
		{"bearer header", `curl -H "Authorization: Bearer abc123DEF456ghi" https://api`, `curl -H "Authorization: Bearer [REDACTED]" https://api`},
		{"env assignment", "DEPLOY_TOKEN=s3cr3t-value ./deploy.sh", "DEPLOY_TOKEN=[REDACTED] ./deploy.sh"},
		{"password colon double-quoted", `password: "hunter2pass"`, `password: "[REDACTED]"`},
		{"client secret single-quoted", `CLIENT_SECRET: 'topsecretvalue'`, `CLIENT_SECRET: '[REDACTED]'`},
		{"memini api key env", "MEMINI_API_KEY=sk-supersecretvalue123", "MEMINI_API_KEY=[REDACTED]"},
		{"api key flag", "tool --api-key=AKIAabcdef0123456789", "tool --api-key=[REDACTED]"},
		{"aws access key id", "key AKIAIOSFODNN7EXAMPLE here", "key [REDACTED] here"},
		{"github token", "use ghp_0123456789abcdefABCDEF0123456789xyz now", "use [REDACTED] now"},
		{"slack token", "xoxb-123456789012-abcdefABCDEF", "[REDACTED]"},
		{"openai key", "OPENAI: sk-ant-api03-abcdefghijklmnop", "OPENAI: [REDACTED]"},
		{"url userinfo", "clone https://alice:s3cret@github.com/x/y", "clone https://alice:[REDACTED]@github.com/x/y"},
		{
			"pem block",
			"prefix -----BEGIN RSA PRIVATE KEY-----\nMIIB...lines...\n-----END RSA PRIVATE KEY----- suffix",
			"prefix [REDACTED] suffix",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Secrets(c.in); got != c.want {
				t.Errorf("Secrets(%q)\n got = %q\nwant = %q", c.in, got, c.want)
			}
		})
	}
}

func TestRedactSecretsInMetadata(t *testing.T) {
	md := map[string]any{
		"files": []any{"a.go", "b.go"},
		"commands": []any{
			"go test ./...",
			"curl -H 'Authorization: Bearer tok12345678' x",
		},
		"nested": map[string]any{"password=swordfish99": "x", "note": "DEPLOY_TOKEN=abcd1234"},
		"count":  3, // non-string preserved
	}
	got := Metadata(md)

	cmds := got["commands"].([]any)
	if cmds[0].(string) != "go test ./..." {
		t.Errorf("benign command mangled: %q", cmds[0])
	}
	if want := "curl -H 'Authorization: Bearer [REDACTED]' x"; cmds[1].(string) != want {
		t.Errorf("command not redacted:\n got = %q\nwant = %q", cmds[1], want)
	}
	nested := got["nested"].(map[string]any)
	if nested["note"].(string) != "DEPLOY_TOKEN=[REDACTED]" {
		t.Errorf("nested value not redacted: %q", nested["note"])
	}
	if got["count"].(int) != 3 {
		t.Errorf("non-string value not preserved: %v", got["count"])
	}
	if files := got["files"].([]any); files[0].(string) != "a.go" {
		t.Errorf("file path mangled: %q", files[0])
	}
}

func TestRedactSecretsInMetadataNil(t *testing.T) {
	if got := Metadata(nil); got != nil {
		t.Errorf("nil metadata should stay nil, got %v", got)
	}
}
