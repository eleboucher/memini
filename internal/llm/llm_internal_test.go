package llm

import (
	"net/http"
	"testing"
)

func TestHTTPClientOr(t *testing.T) {
	// nil → a client with the default per-attempt timeout (so a hung provider
	// cannot park a goroutine forever).
	got := httpClientOr(nil)
	if got == nil {
		t.Fatal("httpClientOr(nil) = nil, want a default client")
	}
	if got.Timeout != defaultHTTPTimeout {
		t.Errorf("default client Timeout = %v, want %v", got.Timeout, defaultHTTPTimeout)
	}

	// A caller-supplied client is returned as-is (tests inject one).
	custom := &http.Client{}
	if httpClientOr(custom) != custom {
		t.Error("httpClientOr(custom) should return the provided client unchanged")
	}
}
