package sanitize

import "testing"

func TestClean(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain ascii untouched", "User: hello", "User: hello"},
		{"keeps newlines and tabs", "a\nb\tc\rd", "a\nb\tc\rd"},
		{"strips C0 control", "ab\x00\x07cd", "abcd"},
		{"strips DEL and C1", "a\x7fb\x9fc", "abc"},
		{"strips U+FFFD", "good�text", "goodtext"},
		{"strips non-characters", "a￾b￿c﷐d", "abcd"},
		{"legit chinese passes through", "使用React框架开发", "使用React框架开发"},
		{"legit japanese passes through", "日本語をする", "日本語をする"},
		{"legit emoji passes through", "ship it 🚀", "ship it 🚀"},
		{"empty stays empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Clean(tc.in); got != tc.want {
				t.Fatalf("Clean(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCleanDropsInvalidUTF8(t *testing.T) {
	// A lone continuation byte is not valid UTF-8 and must be dropped.
	if got := Clean("ab\xffcd"); got != "abcd" {
		t.Fatalf("Clean dropped-invalid = %q, want %q", got, "abcd")
	}
}

func TestGarbled(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		// The reported poisoning: Latin glued to CJK over and over.
		{
			"script salad",
			"Thank you I'm这a家b制c品d with在e上f世g纪h and的i more",
			true,
		},
		// Legitimate: CJK embedding a Latin tech term — must not flag.
		{"chinese with latin term", "使用React框架开发应用程序非常方便快捷", false},
		// Legitimate: Japanese mixes Han and kana constantly — must not flag.
		{"japanese han plus kana", "日本語のテキストを処理するプログラムを書く", false},
		// Legitimate: space-separated code-switching breaks adjacency.
		{"spaced code switching", "open the 这个 file and read 那个 line then stop", false},
		// Plain English: zero transitions.
		{"plain english", "the quick brown fox jumps over the lazy dog again", false},
		// Too short to judge even if mixed.
		{"short mixed", "hi你好", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Garbled(tc.in); got != tc.want {
				t.Fatalf("Garbled(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
