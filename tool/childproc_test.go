package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func withChild(t *testing.T, p *ChildProcess) {
	t.Helper()
	SetChildProcess(p)
	t.Cleanup(func() { SetChildProcess(nil) })
}

// CleanEnv keeps only the allowlist + the per-call vars: a secret in the parent
// environment never reaches a child.
func TestChildProcessCleanEnv(t *testing.T) {
	t.Setenv("CHILDPROC_TEST_API_KEY", "sk-secret")
	t.Setenv("LC_CHILDPROC", "kept")
	t.Setenv("KEEP_ME", "1")
	withChild(t, &ChildProcess{CleanEnv: true, Home: "/home/tool", KeepEnv: []string{"KEEP_ME"}})

	env := withEnvIn("/runs/1", []string{"HTTPS_PROXY=http://127.0.0.1:8790"})
	if v := envValue(env, "CHILDPROC_TEST_API_KEY"); v != "" {
		t.Fatalf("secret leaked into child env: %q", v)
	}
	for k, want := range map[string]string{"HOME": "/home/tool", "HTTPS_PROXY": "http://127.0.0.1:8790",
		"PAGER": "cat", "LC_CHILDPROC": "kept", "KEEP_ME": "1"} {
		if got := envValue(env, k); got != want {
			t.Fatalf("%s=%q, want %q", k, got, want)
		}
	}
	if envValue(env, "PATH") == "" {
		t.Fatal("PATH must be kept")
	}
	if n := countKey(env, "HOME"); n != 1 {
		t.Fatalf("HOME entries: %d", n)
	}

	withChild(t, &ChildProcess{CleanEnv: true}) // no Home: the working dir
	if got := envValue(ChildEnv("/runs/2", nil), "HOME"); got != "/runs/2" {
		t.Fatalf("HOME=%q, want the working dir", got)
	}
}

// Without settings nothing changes: the full parent environment is inherited.
func TestChildProcessDefaultInherits(t *testing.T) {
	t.Setenv("CHILDPROC_TEST_INHERIT", "yes")
	SetChildProcess(nil)
	if got := envValue(withEnvIn("/x", nil), "CHILDPROC_TEST_INHERIT"); got != "yes" {
		t.Fatalf("default must inherit the parent env, got %q", got)
	}
}

// The scrub is real for a spawned Bash, both sync and managed.
func TestChildProcessBashScrubbed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell")
	}
	t.Setenv("CHILDPROC_TEST_TOKEN", "tok-secret")
	withChild(t, &ChildProcess{CleanEnv: true, Home: t.TempDir()})
	raw, _ := json.Marshal(map[string]any{"command": "echo token=[$CHILDPROC_TEST_TOKEN] proxy=$HTTPS_PROXY"})
	tc := &ToolContext{Env: []string{"HTTPS_PROXY=http://p"}}
	res, _ := NewBash().Call(context.Background(), raw, tc)
	if out := res.Flatten(); !strings.Contains(out, "token=[]") || !strings.Contains(out, "proxy=http://p") {
		t.Fatalf("sync bash env: %q", out)
	}

	m, err := NewManager("childproc-env")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Cleanup()
	tk, finished, out, err := m.RunForeground(context.Background(), SpawnSpec{Command: "echo token=[$CHILDPROC_TEST_TOKEN]"}, 10*time.Second)
	if err != nil || !finished || tk == nil {
		t.Fatalf("managed run: finished=%v err=%v", finished, err)
	}
	if !strings.Contains(out, "token=[]") {
		t.Fatalf("managed bash env: %q", out)
	}
}

// Deny refuses every spawn path (fail closed).
func TestChildProcessDeny(t *testing.T) {
	deny := errors.New("egress jail not active")
	withChild(t, &ChildProcess{Deny: deny})

	if err := PrepareChild(exec.Command("true"), nil, true); !errors.Is(err, deny) {
		t.Fatalf("PrepareChild: %v", err)
	}
	raw, _ := json.Marshal(map[string]any{"command": "echo ran"})
	res, _ := NewBash().Call(context.Background(), raw, &ToolContext{})
	if !res.IsError || strings.Contains(res.Flatten(), "ran\n") || !strings.Contains(res.Flatten(), "egress jail not active") {
		t.Fatalf("sync bash must be refused: %q", res.Flatten())
	}
	m, err := NewManager("childproc-deny")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Cleanup()
	if _, err := m.Spawn(SpawnSpec{Command: "echo ran"}); !errors.Is(err, deny) {
		t.Fatalf("Spawn must be refused, got %v", err)
	}
	if ptySupported() {
		if _, err := m.openSession("cat", "", nil, 24, 80, "hands-free", nil); !errors.Is(err, deny) {
			t.Fatalf("shell session must be refused, got %v", err)
		}
	}
}
