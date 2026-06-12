package ui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestMountWellKnown404(t *testing.T) {
	r := chi.NewRouter()
	if err := Mount(r, ""); err != nil {
		t.Fatalf("mount: %v", err)
	}
	srv := httptest.NewServer(r)
	defer srv.Close()

	t.Run("well-known returns 404 not the SPA shell", func(t *testing.T) {
		for _, p := range []string{
			"/.well-known/oauth-protected-resource",
			"/.well-known/oauth-authorization-server",
		} {
			resp, err := http.Get(srv.URL + p)
			if err != nil {
				t.Fatalf("get %s: %v", p, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s: got %d, want 404", p, resp.StatusCode)
			}
		}
	})

	t.Run("deep link still falls back to the SPA shell", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/some/client/route")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("deep link: got %d, want 200", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Fatalf("deep link content-type: got %q", ct)
		}
	})
}

func TestInjectToken(t *testing.T) {
	const shell = `<!doctype html><html><head><title>memini</title></head><body></body></html>`

	t.Run("blank key is a no-op", func(t *testing.T) {
		if got := injectToken([]byte(shell), ""); !bytes.Equal(got, []byte(shell)) {
			t.Fatalf("blank key mutated shell: %s", got)
		}
	})

	t.Run("injected before </head>", func(t *testing.T) {
		got := string(injectToken([]byte(shell), "s3cret"))
		want := `<meta name="memini-token" content="s3cret"></head>`
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Fatalf("tag not before </head>: %s", got)
		}
	})

	t.Run("attribute value is escaped", func(t *testing.T) {
		got := string(injectToken([]byte(shell), `a"><script>`))
		if bytes.Contains([]byte(got), []byte(`content="a"><script>`)) {
			t.Fatalf("unescaped token leaked markup: %s", got)
		}
		if !bytes.Contains([]byte(got), []byte(`a&#34;&gt;&lt;script&gt;`)) {
			t.Fatalf("token not escaped: %s", got)
		}
	})

	t.Run("prepended when no head", func(t *testing.T) {
		got := string(injectToken([]byte("<body>x</body>"), "k"))
		if want := `<meta name="memini-token" content="k"><body>`; !bytes.HasPrefix([]byte(got), []byte(want)) {
			t.Fatalf("not prepended: %s", got)
		}
	})
}
