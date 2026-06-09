package embed_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/eleboucher/memini/internal/embed"
)

// countingEmbedder records every text passed to Embed and returns a fixed-dim
// zero vector per text (the first element encodes the text length, so callers
// can tell vectors apart).
type countingEmbedder struct {
	mu    sync.Mutex
	dims  int
	calls int
	seen  []string
}

func (c *countingEmbedder) Dims() int { return c.dims }

func (c *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	out := make([][]float32, len(texts))
	for i, t := range texts {
		c.seen = append(c.seen, t)
		v := make([]float32, c.dims)
		v[0] = float32(len(t))
		out[i] = v
	}
	return out, nil
}

func TestDisabled(t *testing.T) {
	d := embed.Disabled{D: 16}
	if d.Dims() != 16 {
		t.Fatalf("Dims() = %d, want 16", d.Dims())
	}
	_, err := d.Embed(context.Background(), []string{"x"})
	if !errors.Is(err, embed.ErrDisabled) {
		t.Fatalf("Embed err = %v, want ErrDisabled", err)
	}
}

func TestCachedHitMiss(t *testing.T) {
	inner := &countingEmbedder{dims: 4}
	c, err := embed.NewCached(inner, 128)
	if err != nil {
		t.Fatalf("NewCached: %v", err)
	}
	ctx := context.Background()

	if _, err := c.Embed(ctx, []string{"a", "b"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if inner.calls != 1 || len(inner.seen) != 2 {
		t.Fatalf("first call: calls=%d seen=%v", inner.calls, inner.seen)
	}

	// "a" is cached; only "c" should reach the inner embedder.
	if _, err := c.Embed(ctx, []string{"a", "c"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if inner.calls != 2 {
		t.Fatalf("expected 2 inner calls, got %d", inner.calls)
	}
	if got := inner.seen[2:]; len(got) != 1 || got[0] != "c" {
		t.Fatalf("expected only 'c' embedded on miss, got %v", got)
	}
}

func TestCachedDims(t *testing.T) {
	c, _ := embed.NewCached(&countingEmbedder{dims: 7}, 8)
	if c.Dims() != 7 {
		t.Fatalf("Dims() = %d, want 7", c.Dims())
	}
}

func TestBatchedSplitsByMaxItems(t *testing.T) {
	inner := &countingEmbedder{dims: 4}
	b := embed.NewBatched(inner, 2, 0, 0) // 2 items per request, no char limits
	texts := []string{"a", "b", "c", "d", "e"}
	out, err := b.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(out) != len(texts) {
		t.Fatalf("len(out) = %d, want %d", len(out), len(texts))
	}
	// 5 items, 2 per batch -> 3 requests.
	if inner.calls != 3 {
		t.Fatalf("expected 3 sub-requests, got %d", inner.calls)
	}
	// Order preserved.
	if strings.Join(inner.seen, "") != "abcde" {
		t.Fatalf("order not preserved: %v", inner.seen)
	}
}

func TestBatchedSplitsByMaxChars(t *testing.T) {
	inner := &countingEmbedder{dims: 4}
	// maxChars 5 forces a split: "aaa"(3) + "bbb"(3) exceeds 5 together.
	b := embed.NewBatched(inner, 100, 5, 0)
	out, err := b.Embed(context.Background(), []string{"aaa", "bbb", "cc"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	if inner.calls < 2 {
		t.Fatalf("expected splitting by char budget, got %d calls", inner.calls)
	}
}

func TestBatchedTruncatesItems(t *testing.T) {
	inner := &countingEmbedder{dims: 4}
	b := embed.NewBatched(inner, 10, 0, 3) // truncate each item to 3 runes
	if _, err := b.Embed(context.Background(), []string{"abcdef"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(inner.seen) != 1 || inner.seen[0] != "abc" {
		t.Fatalf("expected truncation to 'abc', got %v", inner.seen)
	}
}

func TestBatchedPreservesOrderWithVectors(t *testing.T) {
	inner := &countingEmbedder{dims: 4}
	b := embed.NewBatched(inner, 2, 0, 0)
	texts := []string{"x", "yy", "zzz"}
	out, err := b.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	// vector[0] encodes text length, so we can verify alignment.
	for i, txt := range texts {
		if int(out[i][0]) != len(txt) {
			t.Fatalf("out[%d][0] = %v, want %d (text %q)", i, out[i][0], len(txt), txt)
		}
	}
}

func TestOpenAIEmbedHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		// Return out-of-order indices to exercise reordering.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"index":1,"embedding":[0.1,0.2]},
			{"index":0,"embedding":[0.3,0.4]}
		]}`))
	}))
	defer srv.Close()

	c, err := embed.NewOpenAI(embed.OpenAIConfig{BaseURL: srv.URL, Model: "m", Dims: 2})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	out, err := c.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	// index 0 -> first vector after reordering.
	if out[0][0] != 0.3 || out[1][0] != 0.1 {
		t.Fatalf("reordering wrong: %v", out)
	}
}

func TestOpenAIEmbedDimMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()

	c, _ := embed.NewOpenAI(embed.OpenAIConfig{BaseURL: srv.URL, Model: "m", Dims: 2})
	if _, err := c.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("expected dim-mismatch error, got nil")
	}
}

func TestOpenAIEmbedNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad model"}}`))
	}))
	defer srv.Close()

	c, _ := embed.NewOpenAI(embed.OpenAIConfig{BaseURL: srv.URL, Model: "m", Dims: 2})
	if _, err := c.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("expected error on 400, got nil")
	}
}

func TestOpenAICountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2]}]}`))
	}))
	defer srv.Close()

	c, _ := embed.NewOpenAI(embed.OpenAIConfig{BaseURL: srv.URL, Model: "m", Dims: 2})
	if _, err := c.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("expected count-mismatch error, got nil")
	}
}

func TestDiskCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.gob")
	ctx := context.Background()

	inner := &countingEmbedder{dims: 4}
	dc, err := embed.NewDiskCache(inner, path)
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	if _, err := dc.Embed(ctx, []string{"hello", "world"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if dc.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", dc.Len())
	}
	if err := dc.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A fresh instance over the same path should serve hits without embedding.
	inner2 := &countingEmbedder{dims: 4}
	dc2, err := embed.NewDiskCache(inner2, path)
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	if dc2.Len() != 2 {
		t.Fatalf("loaded Len() = %d, want 2", dc2.Len())
	}
	if _, err := dc2.Embed(ctx, []string{"hello", "world"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if inner2.calls != 0 {
		t.Fatalf("expected cross-instance cache hits, inner called %d times", inner2.calls)
	}
}
