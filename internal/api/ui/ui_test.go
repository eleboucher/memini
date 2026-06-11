package ui

import (
	"bytes"
	"testing"
)

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
