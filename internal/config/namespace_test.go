package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eleboucher/memini/internal/nsresolve"
)

func TestResolveDefaultNamespace_EnvWins(t *testing.T) {
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "team-a")
	t.Setenv("MEMINI_NAMESPACE", "")
	stubRunGit(t, gitOK("/home/dev/some-repo"))

	ns, src := resolveDefaultNamespace()
	if ns != "team-a" || src != NamespaceFromEnv {
		t.Fatalf("got (%q,%q), want (team-a, env)", ns, src)
	}
}

func TestResolveDefaultNamespace_AltEnvName(t *testing.T) {
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "")
	t.Setenv("MEMINI_NAMESPACE", "team-b")
	stubRunGit(t, gitOK("/home/dev/some-repo"))

	ns, src := resolveDefaultNamespace()
	if ns != "team-b" || src != NamespaceFromEnv {
		t.Fatalf("got (%q,%q), want (team-b, env)", ns, src)
	}
}

func TestResolveDefaultNamespace_GitToplevel(t *testing.T) {
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "")
	t.Setenv("MEMINI_NAMESPACE", "")
	stubRunGit(t, gitOK("/home/dev/my-cool-app"))

	ns, src := resolveDefaultNamespace()
	if ns != "my-cool-app" || src != NamespaceFromGit {
		t.Fatalf("got (%q,%q), want (my-cool-app, git)", ns, src)
	}
}

func TestResolveDefaultNamespace_CWDFallback(t *testing.T) {
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "")
	t.Setenv("MEMINI_NAMESPACE", "")
	stubRunGit(t, gitMissing())

	cwd := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	// Put cwd under a known basename so we can assert it.
	leaf := filepath.Join(cwd, "scratch-project")
	if err := os.Mkdir(leaf, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Chdir(leaf); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	ns, src := resolveDefaultNamespace()
	if ns != "scratch-project" || src != NamespaceFromCWD {
		t.Fatalf("got (%q,%q), want (scratch-project, cwd)", ns, src)
	}
}

func TestResolveDefaultNamespace_LiteralFallback(t *testing.T) {
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "")
	t.Setenv("MEMINI_NAMESPACE", "")
	stubRunGit(t, gitMissing())

	cwd := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	ns, src := resolveDefaultNamespace()
	// t.TempDir() returns something like /var/folders/.../.../T/TestFoo123/001
	// whose basename is the trailing segment. The sanitizer keeps that.
	if ns == "" || src != NamespaceFromCWD {
		t.Fatalf("got (%q,%q), want non-empty cwd fallback", ns, src)
	}
}

// TestRepoNameFromRemote mirrors the cases the plugin's repoNameFromRemote
// (plugin/scripts/_shared.mjs) handles, so server and plugin resolve the same
// namespace from a given origin URL.
func TestRepoNameFromRemote(t *testing.T) {
	cases := map[string]string{
		"git@github.com:eleboucher/memini.git":       "memini",
		"git@github.com:eleboucher/memini":           "memini",
		"https://github.com/eleboucher/memini.git":   "memini",
		"https://github.com/eleboucher/memini":       "memini",
		"ssh://git@github.com/eleboucher/memini.git": "memini",
		"https://github.com/eleboucher/memini/":      "memini",
		"https://github.com/eleboucher/Memini.GIT":   "Memini",
		"git@gitlab.com:group/subgroup/proj.git":     "proj",
		"":                                           "",
		"not-a-url":                                  "not-a-url",
	}
	for url, want := range cases {
		if got := repoNameFromRemote(url); got != want {
			t.Errorf("repoNameFromRemote(%q) = %q, want %q", url, got, want)
		}
	}
}

// gitRemoteThenToplevel stubs runGit so `git remote get-url origin` returns
// remoteURL and `git rev-parse --show-toplevel` returns toplevel. Either may be
// "" to simulate that command failing.
func gitRemoteThenToplevel(remoteURL, toplevel string) fakeRunner {
	return func(_ context.Context, _, _ string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "remote" {
			if remoteURL == "" {
				return "", context.Canceled
			}
			return remoteURL + "\n", nil
		}
		if toplevel == "" {
			return "", context.Canceled
		}
		return toplevel + "\n", nil
	}
}

// TestResolvePluginNamespace_GitRemoteWins asserts ResolvePluginNamespace
// still prefers the git remote over the worktree basename (nsresolve's own
// derive order), but now reports nsresolve's source label ("remote") rather
// than this package's old git-remote-specific one — see the doc comment on
// ResolvePluginNamespace for why the two resolvers' labels are allowed to
// diverge from each other.
func TestResolvePluginNamespace_GitRemoteWins(t *testing.T) {
	t.Setenv("MEMINI_NAMESPACE", "")
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "")
	// The remote names the repo "canonical" but the worktree dir is "renamed".
	stubRunGit(t, gitRemoteThenToplevel("git@github.com:org/canonical.git", "/tmp/renamed"))

	ns, src := ResolvePluginNamespace("/tmp/renamed")
	if ns != "canonical" || src != NamespaceSource(nsresolve.SourceRemote) {
		t.Fatalf("plugin resolution = (%q,%q), want (canonical, %q)", ns, src, nsresolve.SourceRemote)
	}
	// The server's resolution ignores the remote and uses the worktree basename,
	// so doctor can flag the divergence.
	server := sanitizeNamespace(filepath.Base("/tmp/renamed"))
	if server == ns {
		t.Fatalf("expected server (%q) and plugin (%q) to diverge", server, ns)
	}
	if !strings.HasPrefix(server, "renamed") {
		t.Fatalf("server namespace = %q, want renamed", server)
	}
}

// TestResolvePluginNamespace_EnvWins covers the behavior override.go used to
// share this file with (before the override mechanism was retired): the env
// var still outranks derivation, reported with nsresolve's own "env" label.
func TestResolvePluginNamespace_EnvWins(t *testing.T) {
	t.Setenv("MEMINI_NAMESPACE", "pinned-everywhere")
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "")
	t.Setenv("MEMINI_AGENT", "")

	ns, src := ResolvePluginNamespace(t.TempDir())
	if ns != "pinned-everywhere" || src != NamespaceFromEnv {
		t.Fatalf("ResolvePluginNamespace = (%q, %q), want (\"pinned-everywhere\", %q)", ns, src, NamespaceFromEnv)
	}
}

// TestResolvePluginNamespace_AgentSuffixOnDerive: the derive leg nests the
// result under MEMINI_AGENT, matching nsresolve.Resolve's own derive-branch
// behavior (and the plugin's withAgent).
func TestResolvePluginNamespace_AgentSuffixOnDerive(t *testing.T) {
	t.Setenv("MEMINI_NAMESPACE", "")
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "")
	t.Setenv("MEMINI_AGENT", "reviewer")
	stubRunGit(t, gitRemoteThenToplevel("git@github.com:org/canonical.git", "/tmp/renamed"))

	ns, src := ResolvePluginNamespace("/tmp/renamed")
	if ns != "canonical/reviewer" || src != NamespaceSource(nsresolve.SourceRemote) {
		t.Fatalf("ResolvePluginNamespace = (%q,%q), want (canonical/reviewer, %q)", ns, src, nsresolve.SourceRemote)
	}
}

// TestResolvePluginNamespace_AgentSuffixNotAppliedToEnv: nsresolve.Resolve
// returns the env leg verbatim, with no agent suffix — a deliberate
// divergence from the (still-unmigrated) JS plugin, which applies MEMINI_AGENT
// unconditionally. Mirroring nsresolve exactly means mirroring this too.
func TestResolvePluginNamespace_AgentSuffixNotAppliedToEnv(t *testing.T) {
	t.Setenv("MEMINI_NAMESPACE", "pinned-everywhere")
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "")
	t.Setenv("MEMINI_AGENT", "reviewer")

	ns, src := ResolvePluginNamespace(t.TempDir())
	if ns != "pinned-everywhere" || src != NamespaceFromEnv {
		t.Fatalf("ResolvePluginNamespace = (%q, %q), want (\"pinned-everywhere\", %q) with no agent suffix", ns, src, NamespaceFromEnv)
	}
}

// TestPluginFacts_GathersGitAndEnv covers the facts builder both
// ResolvePluginNamespace and doctor's handshake probe share.
func TestPluginFacts_GathersGitAndEnv(t *testing.T) {
	t.Setenv("MEMINI_NAMESPACE", "")
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "team/proj")
	t.Setenv("MEMINI_AGENT", "reviewer")
	stubRunGit(t, gitRemoteThenToplevel("git@github.com:org/canonical.git", "/tmp/renamed"))

	facts := PluginFacts("/tmp/renamed")
	if facts.RemoteURL != "git@github.com:org/canonical.git" {
		t.Errorf("RemoteURL = %q", facts.RemoteURL)
	}
	if facts.ToplevelPath != "/tmp/renamed" {
		t.Errorf("ToplevelPath = %q", facts.ToplevelPath)
	}
	if facts.ToplevelBasename != "renamed" {
		t.Errorf("ToplevelBasename = %q", facts.ToplevelBasename)
	}
	if facts.CwdBasename != "renamed" {
		t.Errorf("CwdBasename = %q", facts.CwdBasename)
	}
	if facts.Agent != "reviewer" {
		t.Errorf("Agent = %q", facts.Agent)
	}
	if facts.EnvNamespace != "team/proj" {
		t.Errorf("EnvNamespace = %q", facts.EnvNamespace)
	}
	if facts.DeclaredNamespace != "" {
		t.Errorf("DeclaredNamespace = %q, want empty (offline CLI resolution never declares)", facts.DeclaredNamespace)
	}
}

// TestPluginFacts_NoGitDegradesToCwdBasenameOnly ensures a directory outside
// any git repo still yields a usable CwdBasename with empty remote/toplevel
// facts, rather than a bogus "." basename.
func TestPluginFacts_NoGitDegradesToCwdBasenameOnly(t *testing.T) {
	t.Setenv("MEMINI_AGENT", "")
	stubRunGit(t, gitMissing())
	dir := t.TempDir()

	facts := PluginFacts(dir)
	if facts.RemoteURL != "" || facts.ToplevelPath != "" || facts.ToplevelBasename != "" {
		t.Errorf("expected no remote/toplevel facts outside a git repo, got %+v", facts)
	}
	if facts.CwdBasename != filepath.Base(dir) {
		t.Errorf("CwdBasename = %q, want %q", facts.CwdBasename, filepath.Base(dir))
	}
}

func TestSanitizeNamespace(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "default"},
		{"   ", "default"},
		{"my-project", "my-project"},
		{"/home/dev/my-project", "my-project"},
		{"/home/dev/my-project/", "my-project"},
		{"nested/path/leaf", "leaf"},
		{".", "default"},
		{"/", "default"},
	}
	for _, tt := range tests {
		if got := sanitizeNamespace(tt.in); got != tt.want {
			t.Errorf("sanitizeNamespace(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestSanitizeNamespacePath covers the env-sourced sanitizer, which must
// preserve a deliberate multi-segment namespace like "project/agent" instead
// of flattening it to a basename (the sanitizeNamespace bug this fixes).
func TestSanitizeNamespacePath(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"project/agent", "project/agent"},
		{"", "default"},
		{"   ", "default"},
		{"/x/", "x"},
		{"a//b", "a/b"},
		{"my-project", "my-project"},   // basename-style values unaffected
		{"  team/proj  ", "team/proj"}, // surrounding whitespace trimmed
		{"///", "default"},
	}
	for _, tt := range tests {
		if got := sanitizeNamespacePath(tt.in); got != tt.want {
			t.Errorf("sanitizeNamespacePath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestResolveDefaultNamespace_EnvPreservesSlash is the sanitize-slash-fix
// regression: MEMINI_DEFAULT_NAMESPACE=team/proj must resolve to "team/proj",
// not be flattened to "proj" by a basename pass.
func TestResolveDefaultNamespace_EnvPreservesSlash(t *testing.T) {
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "team/proj")
	t.Setenv("MEMINI_NAMESPACE", "")
	stubRunGit(t, gitOK("/home/dev/some-repo"))

	ns, src := resolveDefaultNamespace()
	if ns != "team/proj" || src != NamespaceFromEnv {
		t.Fatalf("got (%q,%q), want (team/proj, env)", ns, src)
	}
}
