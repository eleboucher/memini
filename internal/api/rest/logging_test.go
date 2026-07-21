package rest_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/eleboucher/memini/internal/httputil"
)

// TestV1RequestsRecordActor asserts the /v1 middleware chain records the
// authenticated actor into an actor holder installed by an outer wrapper (in
// production, internal/server's request logger): a named key records (name,
// "key"), the admin env key ("", "env"). Without this every access-log line
// is anonymous and operators cannot tell whose session did what.
func TestV1RequestsRecordActor(t *testing.T) {
	fileYAML := `
keys:
  - name: filebot
    secret: "tok-file"
`
	h, _ := newConfigServer(t, "admin-secret", fileYAML, nil)

	cases := []struct {
		name, token, wantName, wantKind string
	}{
		{"named file key", "tok-file", "filebot", "key"},
		{"admin env key", "admin-secret", "", "env"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotName, gotKind string
			var recorded bool
			wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r = r.WithContext(httputil.WithActorHolder(r.Context()))
				h.ServeHTTP(w, r)
				gotName, gotKind, recorded = httputil.RecordedActor(r.Context())
			})

			rec := do(t, wrapped, http.MethodGet, "/v1/self", "", c.token, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /v1/self: want 200, got %d (%s)", rec.Code, rec.Body)
			}
			if !recorded {
				t.Fatal("no actor recorded, want one")
			}
			if gotName != c.wantName || gotKind != c.wantKind {
				t.Errorf("recorded actor = (%q, %q), want (%q, %q)", gotName, gotKind, c.wantName, c.wantKind)
			}
		})
	}
}

// TestHandshakeLogsResolution asserts a successful handshake emits one info
// log line stating the resolved namespace and its full provenance — key,
// source, and the prefix with the settings layer it came from. This is the
// operator-side record of every session's namespace decision; the handshake
// response alone reports it only to the client that asked.
func TestHandshakeLogsResolution(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	fileYAML := `
keys:
  - name: filebot
    secret: "tok-file"
    settings:
      namespace_prefix: team
`
	h, _ := newConfigServer(t, "admin-secret", fileYAML, nil)

	body := handshakeBody(map[string]any{
		"cwd_basename": "proj",
		"remote_url":   "https://github.com/acme/phoenix.git",
	})
	rec := do(t, h, http.MethodPost, "/v1/handshake", "", "tok-file", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("handshake: want 200, got %d (%s)", rec.Code, rec.Body)
	}

	logged := logBuf.String()
	for _, want := range []string{
		`"msg":"handshake: namespace resolved"`,
		`"key":"filebot"`,
		`"namespace":"team/phoenix"`,
		`"source":"remote"`,
		`"remote_url":"https://github.com/acme/phoenix.git"`,
		`"prefix":"team"`,
		`"prefix_source":"key"`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("handshake log missing %s; got: %s", want, logged)
		}
	}
}
