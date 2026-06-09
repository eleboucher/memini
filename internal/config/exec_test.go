package config

import (
	"context"
	"fmt"
	"testing"
)

// fakeRunner replaces runGit for the duration of the test. The provided
// function receives the same args as runGit would have passed to
// `exec.CommandContext`, plus the workdir, and returns (stdout, err).
type fakeRunner func(ctx context.Context, dir, name string, args ...string) (string, error)

func stubRunGit(t *testing.T, fn fakeRunner) {
	t.Helper()
	prev := runGit
	runGit = fn
	t.Cleanup(func() { runGit = prev })
}

// gitOK is a fakeRunner that mimics a successful `git rev-parse
// --show-toplevel` returning the given toplevel.
func gitOK(toplevel string) fakeRunner {
	return func(_ context.Context, _, _ string, _ ...string) (string, error) {
		return toplevel + "\n", nil
	}
}

// gitMissing is a fakeRunner that mimics a non-git directory: exit status 128.
func gitMissing() fakeRunner {
	return func(_ context.Context, _, _ string, _ ...string) (string, error) {
		return "", fmt.Errorf("exit status 128")
	}
}
