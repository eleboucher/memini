package embed

import (
	"context"
	"encoding/gob"
	"os"
	"sync"
)

// DiskCache wraps an Embedder with a persistent content-hashed cache, so
// identical text is embedded once across process runs. Safe for concurrent use.
type DiskCache struct {
	inner Embedder
	path  string

	mu        sync.Mutex
	cache     map[string][]float32
	dirty     bool
	lastSaved int
}

// autosaveEvery flushes to disk after this many new vectors accumulate.
const autosaveEvery = 500

// NewDiskCache wraps inner, loading any existing cache at path.
func NewDiskCache(inner Embedder, path string) (*DiskCache, error) {
	d := &DiskCache{inner: inner, path: path, cache: map[string][]float32{}}
	if f, err := os.Open(path); err == nil {
		_ = gob.NewDecoder(f).Decode(&d.cache)
		_ = f.Close()
	}
	return d, nil
}

// Dims returns the wrapped embedder's dimensionality.
func (d *DiskCache) Dims() int { return d.inner.Dims() }

// Len reports how many vectors are cached.
func (d *DiskCache) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.cache)
}

// Embed serves cache hits and embeds only misses, recording new vectors.
func (d *DiskCache) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	var missIdx []int
	var missText []string

	d.mu.Lock()
	for i, t := range texts {
		if v, ok := d.cache[key(t)]; ok {
			out[i] = v
		} else {
			missIdx = append(missIdx, i)
			missText = append(missText, t)
		}
	}
	d.mu.Unlock()

	if len(missText) > 0 {
		vecs, err := d.inner.Embed(ctx, missText)
		if err != nil {
			return nil, err
		}
		d.mu.Lock()
		for j, v := range vecs {
			out[missIdx[j]] = v
			d.cache[key(missText[j])] = v
		}
		d.dirty = true
		shouldSave := len(d.cache)-d.lastSaved >= autosaveEvery
		d.mu.Unlock()
		if shouldSave {
			_ = d.Save()
		}
	}
	return out, nil
}

// Save persists the cache to disk if it changed.
func (d *DiskCache) Save() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.dirty {
		return nil
	}
	tmp := d.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(d.cache); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, d.path); err != nil {
		return err
	}
	d.dirty = false
	d.lastSaved = len(d.cache)
	return nil
}
