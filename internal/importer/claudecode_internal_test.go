package importer

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/eleboucher/memini/internal/config"
)

// TestNSResolverMemoizes: the resolver runs the (expensive, git-shelling) fn at
// most once per distinct cwd, and returns "" for an empty cwd without calling fn.
func TestNSResolverMemoizes(t *testing.T) {
	var calls atomic.Int32
	r := &nsResolver{
		cache:   map[string]string{},
		demoted: map[string]string{},
		fn: func(cwd string) (string, config.NamespaceSource) {
			calls.Add(1)
			return "ns-for-" + cwd, config.NamespaceFromGitRemote
		},
	}

	if got := r.resolve(""); got != "" {
		t.Fatalf("empty cwd should resolve to %q, got %q", "", got)
	}
	for range 5 {
		if got := r.resolve("/proj/a"); got != "ns-for-/proj/a" {
			t.Fatalf("resolve = %q", got)
		}
	}
	r.resolve("/proj/b")
	if calls.Load() != 2 {
		t.Fatalf("fn called %d times, want 2 (once per distinct cwd, never for empty)", calls.Load())
	}
}

// TestNSResolverRecordsDemotion: a cwd resolved without the git-remote step is
// recorded so the backfill can warn that history may have landed off-namespace.
func TestNSResolverRecordsDemotion(t *testing.T) {
	r := &nsResolver{
		cache:   map[string]string{},
		demoted: map[string]string{},
		fn: func(cwd string) (string, config.NamespaceSource) {
			return "dirbase", config.NamespaceFromCWD // git failed → basename
		},
	}
	r.resolve("/gone/project")
	w := r.warnings()
	if len(w) != 1 {
		t.Fatalf("want one demotion warning, got %v", w)
	}
}

// TestNSResolverConcurrent exercises the lock under the parallel directory walk:
// many goroutines hammering shared cwds must still call fn once per distinct cwd.
// Run with -race.
func TestNSResolverConcurrent(t *testing.T) {
	var calls atomic.Int32
	r := &nsResolver{
		cache:   map[string]string{},
		demoted: map[string]string{},
		fn: func(cwd string) (string, config.NamespaceSource) {
			calls.Add(1)
			return "ns-" + cwd, config.NamespaceFromGitRemote
		},
	}
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.resolve("/shared/proj")
			r.resolve("/other/proj")
		}()
	}
	wg.Wait()
	if calls.Load() != 2 {
		t.Fatalf("fn called %d times across 50 goroutines, want 2 distinct cwds", calls.Load())
	}
}
