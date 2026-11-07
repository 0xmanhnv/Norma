package tool

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRipgrepPathDisabled(t *testing.T) {
	t.Setenv("NORMA_DISABLE_RIPGREP", "1")
	if RipgrepPath() != "" {
		t.Fatal("NORMA_DISABLE_RIPGREP should force the Go implementation")
	}
}

func TestGrepViaRipgrepIfAvailable(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed — skipping the ripgrep-backed path")
	}
	// Use the existing rg; never auto-install during tests.
	t.Setenv("NORMA_RIPGREP_INSTALL", "")
	os.Unsetenv("NORMA_DISABLE_RIPGREP")

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.go"), []byte("package main\nfunc Zed(){}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "y.txt"), []byte("nothing here\n"), 0o644)

	res := run(t, NewGrep(), dir, map[string]any{"pattern": "Zed", "output_mode": "files_with_matches"})
	if !strings.Contains(res.Flatten(), "x.go") || strings.Contains(res.Flatten(), "y.txt") {
		t.Fatalf("ripgrep-backed grep: %q", res.Flatten())
	}
}

// Without rg on PATH, resolution falls back to the Go grep and installs nothing,
// unless the host opted in.
func TestResolveRipgrepNeverInstallsByDefault(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no rg, no npm
	t.Setenv("NORMA_RIPGREP_INSTALL", "")
	called := false
	orig := installRipgrep
	installRipgrep = func() string { called = true; return "" }
	defer func() { installRipgrep = orig }()
	if p := resolveRipgrep(); p != "" || called {
		t.Fatalf("resolve = %q, install called = %v", p, called)
	}
	t.Setenv("NORMA_RIPGREP_INSTALL", "1")
	if resolveRipgrep(); !called {
		t.Fatal("opt-in did not reach the installer")
	}
}
