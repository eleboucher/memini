package config

import (
	"os"
	"path/filepath"
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
