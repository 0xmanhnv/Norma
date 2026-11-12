package tool

import (
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
)

// ChildProcess controls how every subprocess corvid starts is launched: Bash
// (sync and managed), background tasks, interactive shell sessions, Grep's
// ripgrep and stdio MCP servers.
//
// The environment a child gets:
//
//   - with no settings installed, or settings with neither Allowlist nor
//     CleanEnv: the whole parent environment, as before (secrets included);
//   - with Allowlist: an allowlist of the parent environment, PATH, LANG,
//     LANGUAGE, LC_*, TERM, TZ, TMPDIR, HOME, USER, LOGNAME and SHELL, plus
//     the names in KeepEnv;
//   - with CleanEnv: the same without HOME, USER, LOGNAME and SHELL, and HOME
//     set to Home, else the child's working directory.
//
// Per-call variables (ToolContext.Env, Options.BashEnv, an MCP server's env)
// are always added on top. A session whose permission.Policy.ChildEnv is
// "allowlist" gets the allowlist for its own tools' children whatever is
// installed here (see ToolContext.ChildEnvAllowlist).
type ChildProcess struct {
	// Credential runs children as this uid/gid with no supplementary groups.
	// Unix only (the parent needs CAP_SETUID/CAP_SETGID); on Windows a set
	// Credential makes every spawn fail instead of running unconfined.
	Credential *Credential
	// Allowlist starts children from the allowlist instead of the whole
	// parent environment.
	Allowlist bool
	// CleanEnv narrows the allowlist further: HOME, USER, LOGNAME and SHELL are
	// not taken from the parent, and HOME is Home, else the child's working
	// directory. It implies Allowlist.
	CleanEnv bool
	// KeepEnv names further parent variables a child gets with Allowlist or
	// CleanEnv.
	KeepEnv []string
	// Home is the child's HOME with CleanEnv, or with Allowlist when set.
	Home string
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

// ChildEnv is the environment a child started in dir gets under the installed
// settings: the base environment (all of it, or the allowlist) with extra
// layered on top.
func ChildEnv(dir string, extra []string) []string {
	return append(baseEnv(dir, false), extra...)
}

// childEnvAllow is the part of the parent environment every child gets.
var childEnvAllow = []string{"PATH", "LANG", "LANGUAGE", "TERM", "TZ", "TMPDIR"}

// childEnvAccount is the account part of the allowlist, left out with CleanEnv.
var childEnvAccount = []string{"HOME", "USER", "LOGNAME", "SHELL"}

// baseEnv is the parent part of a child's environment. allowlist forces the
// allowlist (a session with Policy.ChildEnv "allowlist") whatever is installed.
func baseEnv(dir string, allowlist bool) []string {
	p := childProc.Load()
	if p == nil {
		p = &ChildProcess{}
	}
	if !allowlist && !p.Allowlist && !p.CleanEnv {
		return os.Environ()
	}
	keep := map[string]bool{}
	for _, k := range childEnvAllow {
		keep[k] = true
	}
	homeOverride := p.CleanEnv || p.Home != ""
	if !p.CleanEnv {
		for _, k := range childEnvAccount {
			keep[k] = true
		}
	}
	if homeOverride {
		delete(keep, "HOME")
	}
	for _, k := range p.KeepEnv {
		keep[k] = true
	}
	var out []string
	for _, kv := range os.Environ() {
		if k := envKey(kv); keep[k] || strings.HasPrefix(k, "LC_") {
			out = append(out, kv)
		}
	}
	if homeOverride {
		if p.Home != "" {
			out = append(out, "HOME="+p.Home)
		} else if dir != "" {
			out = append(out, "HOME="+dir)
		}
	}
	return out
}

// PrepareChild applies the installed settings to cmd before Start: it refuses
// (Deny), sets cmd.Env from cmd.Dir + extra unless env is false (the caller
// has already built cmd.Env with ChildEnv), and sets the credential. The pty
// and process-group flags already on cmd are kept. A caller that passes env
// false must set cmd.Env itself: a nil cmd.Env inherits everything.
func PrepareChild(cmd *exec.Cmd, extra []string, env bool) error {
	return prepareChild(cmd, extra, env, false)
}

// prepareChild is PrepareChild; allowlist forces the environment allowlist.
func prepareChild(cmd *exec.Cmd, extra []string, env, allowlist bool) error {
	p := childProc.Load()
	if p == nil {
		if env {
			cmd.Env = append(baseEnv(cmd.Dir, allowlist), extra...)
		}
		return nil
	}
	if p.Deny != nil {
		return p.Deny
	}
	if env {
		cmd.Env = append(baseEnv(cmd.Dir, allowlist), extra...)
	}
	if p.Credential != nil {
		return setCredential(cmd, *p.Credential)
	}
	return nil
}

// applyChild is PrepareChild for corvid's own shells, whose env is already built
// by withEnvIn.
func applyChild(cmd *exec.Cmd) error { return PrepareChild(cmd, nil, false) }
