package tool

import (
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
)

// ChildProcess controls how every subprocess corvid starts is launched: Bash
// (sync and managed), background tasks, interactive shell sessions, Grep's
// ripgrep and stdio MCP servers. nil (the default) keeps the historical
// behaviour: same user, full parent environment.
type ChildProcess struct {
	// Credential runs children as this uid/gid with no supplementary groups.
	// Unix only (the parent needs CAP_SETUID/CAP_SETGID); on Windows a set
	// Credential makes every spawn fail instead of running unconfined.
	Credential *Credential
	// CleanEnv starts children from an allowlist of the parent environment
	// (PATH, LANG, LANGUAGE, LC_*, TERM, TZ, TMPDIR + KeepEnv) instead of all of
	// it; HOME is Home, else the child's working directory. Per-call extra
	// variables (ToolContext.Env, MCP server env) are still added on top.
	CleanEnv bool
	KeepEnv  []string
	Home     string
	// Deny, when set, refuses every spawn with this error (fail closed).
	Deny error
}

// Credential is a uid/gid pair for ChildProcess.
type Credential struct{ UID, GID uint32 }

var childProc atomic.Pointer[ChildProcess]

// SetChildProcess installs the process-wide child settings (nil restores the default).
func SetChildProcess(p *ChildProcess) { childProc.Store(p) }

// CurrentChildProcess returns the installed settings (nil = default).
func CurrentChildProcess() *ChildProcess { return childProc.Load() }

// ChildEnv is the environment a child started in dir gets: the base environment
// (all of it, or the CleanEnv allowlist) with extra layered on top.
func ChildEnv(dir string, extra []string) []string {
	return append(baseEnv(dir), extra...)
}

func baseEnv(dir string) []string {
	p := childProc.Load()
	if p == nil || !p.CleanEnv {
		return os.Environ()
	}
	keep := map[string]bool{"PATH": true, "LANG": true, "LANGUAGE": true, "TERM": true, "TZ": true, "TMPDIR": true}
	for _, k := range p.KeepEnv {
		keep[k] = true
	}
	var out []string
	for _, kv := range os.Environ() {
		if k := envKey(kv); keep[k] || strings.HasPrefix(k, "LC_") {
			out = append(out, kv)
		}
	}
	if home := p.Home; home != "" {
		out = append(out, "HOME="+home)
	} else if dir != "" {
		out = append(out, "HOME="+dir)
	}
	return out
}

// PrepareChild applies the installed settings to cmd before Start: it refuses
// (Deny), sets cmd.Env from cmd.Dir + extra unless env is false, and sets the
// credential. The pty and process-group flags already on cmd are kept.
func PrepareChild(cmd *exec.Cmd, extra []string, env bool) error {
	p := childProc.Load()
	if p == nil {
		if env {
			cmd.Env = ChildEnv(cmd.Dir, extra)
		}
		return nil
	}
	if p.Deny != nil {
		return p.Deny
	}
	if env {
		cmd.Env = ChildEnv(cmd.Dir, extra)
	}
	if p.Credential != nil {
		return setCredential(cmd, *p.Credential)
	}
	return nil
}

// applyChild is PrepareChild for corvid's own shells, whose env is already built
// by withEnvIn.
func applyChild(cmd *exec.Cmd) error { return PrepareChild(cmd, nil, false) }
