package config

import (
	"context"
	"os/exec"
	"time"
)

// gitTimeout caps how long resolveDefaultNamespace will block waiting for
// `git rev-parse`. Matches the 500ms bound used by agentmemory's
// resolveProject helper.
const gitTimeout = 500 * time.Millisecond

// execContext builds the bounded context for git lookups. Defined as a
// package-level var so tests can stub it.
var execContext = func() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), gitTimeout)
}

// runGit is the seam gitToplevel uses to shell out. Indirected so tests can
// stub the git call without forking a process.
var runGit = func(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}
