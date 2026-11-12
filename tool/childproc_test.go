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

// Without settings a child inherits the whole environment, as before.
// Allowlist (process-wide) and a session's ChildEnv "allowlist" give it the
// allowlist plus what the host names, never an unnamed secret.
func TestChildProcessEnvModes(t *testing.T) {
	t.Setenv("CHILDPROC_TEST_API_KEY", "sk-secret")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-secret")
	t.Setenv("HOME", "/home/someone")
	SetChildProcess(nil)
	if v := envValue(withEnvIn("/x", nil), "ANTHROPIC_API_KEY"); v != "sk-ant-secret" {
		t.Fatalf("with no settings a child must inherit the environment as before, got %q", v)
	}
	check := func(where string, env []string) {
		t.Helper()
		for _, k := range []string{"CHILDPROC_TEST_API_KEY", "ANTHROPIC_API_KEY"} {
			if v := envValue(env, k); v != "" {
				t.Fatalf("%s: %s leaked into the child env: %q", where, k, v)
			}
		}
		if envValue(env, "PATH") == "" || envValue(env, "HOME") != "/home/someone" || envValue(env, "NAMED") != "1" {
			t.Fatalf("%s: child env lost PATH, HOME or a named variable: %v", where, env)
		}
	}
	check("session allowlist", withEnvFor("/x", []string{"NAMED=1"}, true))

	withChild(t, &ChildProcess{Allowlist: true})
	check("process Allowlist", withEnvIn("/x", []string{"NAMED=1"}))

	withChild(t, &ChildProcess{Allowlist: true, KeepEnv: []string{"CHILDPROC_TEST_API_KEY"}})
	if v := envValue(withEnvIn("/x", nil), "CHILDPROC_TEST_API_KEY"); v != "sk-secret" {
		t.Fatalf("a variable named in KeepEnv must pass, got %q", v)
	}
	if v := envValue(withEnvIn("/x", nil), "ANTHROPIC_API_KEY"); v != "" {
		t.Fatalf("an unnamed secret passed with KeepEnv set: %q", v)
	}
}

// A session with ChildEnv "allowlist" (ToolContext.ChildEnvAllowlist) gets the
// allowlist on every spawn path with no settings installed: a sync and a
// managed Bash, and the ripgrep spawn.
func TestChildEnvAllowlistPerSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell")
	}
	t.Setenv("CHILDPROC_TEST_TOKEN", "tok-secret")
	SetChildProcess(nil)
	raw, _ := json.Marshal(map[string]any{"command": "echo token=[$CHILDPROC_TEST_TOKEN]"})
	res, _ := NewBash().Call(context.Background(), raw, &ToolContext{ChildEnvAllowlist: true})
	if out := res.Flatten(); !strings.Contains(out, "token=[]") {
		t.Fatalf("sync bash inherited a secret: %q", out)
	}
	res, _ = NewBash().Call(context.Background(), raw, &ToolContext{})
	if out := res.Flatten(); !strings.Contains(out, "token=[tok-secret]") {
		t.Fatalf("without the session setting, sync bash must inherit as before: %q", out)
	}
	m, err := NewManager("childproc-session")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Cleanup()
	_, finished, out, err := m.RunForeground(context.Background(), SpawnSpec{Command: "echo token=[$CHILDPROC_TEST_TOKEN]", EnvAllowlist: true}, 10*time.Second)
	if err != nil || !finished || !strings.Contains(out, "token=[]") {
		t.Fatalf("managed bash: finished=%v err=%v out=%q", finished, err, out)
	}
	cmd := exec.Command("true")
	if err := prepareChild(cmd, nil, true, true); err != nil || envValue(cmd.Env, "CHILDPROC_TEST_TOKEN") != "" || len(cmd.Env) == 0 {
		t.Fatalf("prepareChild with allowlist: err=%v env=%v", err, cmd.Env)
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
		if _, err := m.openSession("cat", "", nil, false, 24, 80, "hands-free", nil); !errors.Is(err, deny) {
			t.Fatalf("shell session must be refused, got %v", err)
		}
	}
}
