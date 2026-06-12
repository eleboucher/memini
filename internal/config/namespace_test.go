package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDefaultNamespace_EnvWins(t *testing.T) {
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "tenant-a")
	t.Setenv("MEMINI_NAMESPACE", "")
	stubRunGit(t, gitOK("/home/dev/some-repo"))

	ns, src := resolveDefaultNamespace()
	if ns != "tenant-a" || src != NamespaceFromEnv {
		t.Fatalf("got (%q,%q), want (tenant-a, env)", ns, src)
	}
}

func TestResolveDefaultNamespace_AltEnvName(t *testing.T) {
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "")
	t.Setenv("MEMINI_NAMESPACE", "tenant-b")
	stubRunGit(t, gitOK("/home/dev/some-repo"))

	ns, src := resolveDefaultNamespace()
	if ns != "tenant-b" || src != NamespaceFromEnv {
		t.Fatalf("got (%q,%q), want (tenant-b, env)", ns, src)
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

func TestResolvePluginNamespace_GitRemoteWins(t *testing.T) {
	t.Setenv("MEMINI_NAMESPACE", "")
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "")
	// The remote names the repo "canonical" but the worktree dir is "renamed".
	stubRunGit(t, gitRemoteThenToplevel("git@github.com:org/canonical.git", "/tmp/renamed"))

	ns, src := ResolvePluginNamespace("/tmp/renamed")
	if ns != "canonical" || src != NamespaceFromGitRemote {
		t.Fatalf("plugin resolution = (%q,%q), want (canonical, git-remote)", ns, src)
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
