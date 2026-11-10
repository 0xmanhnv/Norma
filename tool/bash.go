package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
)

// catastrophicBash matches a small set of irreversibly destructive commands
// that are hard-denied regardless of mode (NFR-10). This is a safety floor, not
// a full sandbox — hosts should still gate Bash via CanUseTool.
var catastrophicBash = []*regexp.Regexp{
	regexp.MustCompile(`\brm\s+(-[a-zA-Z]*\s+)*-[a-zA-Z]*[rf][a-zA-Z]*\s+(-[a-zA-Z]+\s+)*/(\s|$)`), // rm -rf /
	regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;`),                               // fork bomb
	regexp.MustCompile(`\bmkfs\.\w+\s+/dev/`),                                                      // format a device
	regexp.MustCompile(`\bdd\b[^\n]*\bof=/dev/(sd|nvme|hd)`),                                       // overwrite a disk
}

// disallowedAutoBackground lists commands that must never be silently moved to
// the background on timeout.
var disallowedAutoBackground = []string{"sleep"}

// NewBash builds the Bash tool: run a shell command with a timeout, optionally in
// the background.
func NewBash() CoreTool {
	props := map[string]any{
		"command":    map[string]any{"type": "string", "description": "The shell command to execute."},
		"timeout_ms": map[string]any{"type": "integer", "description": "Timeout in ms (default 120000, max 600000)."},
	}
	desc := "Executes a shell command in the working directory and returns combined stdout and stderr. Use for running tests, builds, package managers, and git. Prefer the dedicated file tools for file work."
	// Only advertise the cap where it is actually enforced: the guard needs
	// /proc (linux) and the task manager (runBashSync has no watchdog), and the
	// cap can be switched off outright. Promising a bound that never fires is
	// worse than saying nothing.
	if limit := memCapBytes(); memGuardSupported && limit > 0 && !BackgroundTasksDisabled() {
		desc += fmt.Sprintf(" A per-command memory cap (%s) kills a command whose process group exceeds it; the error names the cap.", humanBytes(limit))
	}
	prompt := "The command runs through the shell, so you are not limited to one command: chain steps with && or ;, use pipes, and pass multi-line scripts or heredocs. For anything beyond a simple one-liner — loops, parsing, string/JSON handling, HTTP logic, computation — prefer writing a short script (Python, Node, shell, etc.) and executing it (e.g. via a heredoc or by writing a file then running it) rather than forcing everything into a single command. A timeout (default 120s, max 600s) applies. Avoid commands needing interactive input."
	if !BackgroundTasksDisabled() {
		props["run_in_background"] = map[string]any{
			"type":        "boolean",
			"description": "Run in the background and return immediately; read its output later with TaskOutput or Read, and you are notified when it completes. A foreground command that exceeds its timeout is also moved to the background automatically.",
		}
		desc += " Set run_in_background to return immediately and read the output later."
	}
	return Build(Spec{
		Name:        "Bash",
		Description: desc,
		Prompt:      prompt,
		Schema: map[string]any{
			"type":       "object",
			"properties": props,
			"required":   []any{"command"},
		},
		Permissions: bashPermissions,
		Run:         runBash,
		Output:      bashOutput,
	})
}

// CommandOutput is the Bash tool's value for a command that ran in the
// foreground to an end: it exited, failed to run, or hit its timeout. Starting
// a command in the background, or moving it there, is answered with plain text.
type CommandOutput struct {
	// Output is the combined stdout and stderr, already bounded for the
	// context window (see Capture): past the budget it is a head plus the
	// spill store's guidance.
	Output string `json:"output"`
	// ExitCode is the command's exit status (0 unless Failed).
	ExitCode int `json:"exit_code"`
	// Failed reports a command that ended unsuccessfully: a non-zero exit, or
	// a kill.
	Failed bool `json:"failed,omitempty"`
	// TimedOut reports a command killed at Timeout.
	TimedOut bool   `json:"timed_out,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
	// RunError says why the command could not run to an exit status.
	RunError string `json:"run_error,omitempty"`
	// KilledBy names the watchdog that killed the command: "memory_cap" (its
	// process group grew past the per-command memory cap) or "output_cap" (its
	// output grew past the per-task output cap). Cap is that limit, rendered.
	// A watchdog kill has no meaningful exit code, so these replace it.
	KilledBy string `json:"killed_by,omitempty"`
	Cap      string `json:"cap,omitempty"`
}

// Values of CommandOutput.KilledBy.
const (
	killedByMemoryCap = "memory_cap"
	killedByOutputCap = "output_cap"
)

var bashOutput = DeclareOutput(OutputSpec[CommandOutput]{
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"output":    map[string]any{"type": "string"},
			"exit_code": map[string]any{"type": "integer"},
			"failed":    map[string]any{"type": "boolean"},
			"timed_out": map[string]any{"type": "boolean"},
			"timeout":   map[string]any{"type": "string"},
			"run_error": map[string]any{"type": "string"},
			"killed_by": map[string]any{"type": "string", "enum": []any{killedByMemoryCap, killedByOutputCap}},
			"cap":       map[string]any{"type": "string"},
		},
		"required": []any{"output", "exit_code"},
	},
	Render: func(_ json.RawMessage, v CommandOutput) ([]llm.ContentBlock, error) {
		var s string
		switch {
		case v.RunError != "":
			s = fmt.Sprintf("%s\n\n[failed to run: %s]", v.Output, v.RunError)
		case v.KilledBy == killedByMemoryCap:
			s = withNoOutput(v.Output) + fmt.Sprintf(
				"\n\n[terminated: the command's process group exceeded the %s per-command memory cap and was killed — stream or page the data instead of accumulating it in memory]", v.Cap)
		case v.KilledBy == killedByOutputCap:
			s = withNoOutput(v.Output) + fmt.Sprintf("\n\n[terminated: output exceeded the %s per-task output cap]", v.Cap)
		case v.TimedOut:
			s = v.Output + fmt.Sprintf("\n\n[command timed out after %s]", v.Timeout)
		case v.Failed:
			s = fmt.Sprintf("%s\n\n[exit code %d]", withNoOutput(v.Output), v.ExitCode)
		case strings.TrimSpace(v.Output) == "":
			s = "(command produced no output)"
		default:
			s = v.Output
		}
		return []llm.ContentBlock{llm.TextBlock(s)}, nil
	},
})

// withNoOutput stands in "(no output)" for an empty output ahead of a status
// line.
func withNoOutput(out string) string {
	if out == "" {
		return "(no output)"
	}
	return out
}

// commandResult wraps v as the tool's result; anything but a clean exit is an
// error result.
func commandResult(v CommandOutput) Result {
	return Result{Data: v, IsError: v.Failed || v.TimedOut || v.RunError != ""}
}

func bashPermissions(_ context.Context, input json.RawMessage, _ permission.Context) permission.Decision {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &in)
	for _, re := range catastrophicBash {
		if re.MatchString(in.Command) {
			return permission.Denied("denied: command appears irreversibly destructive and is blocked by the safety floor")
		}
	}
	return permission.AskUser("run shell command: " + in.Command)
}

func runBash(ctx context.Context, input json.RawMessage, tc *ToolContext) (Result, error) {
	var in struct {
		Command         string `json:"command"`
		TimeoutMS       int    `json:"timeout_ms"`
		RunInBackground bool   `json:"run_in_background"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(in.Command) == "" {
		return Errorf("Error: command is required"), nil
	}
	timeout := 120 * time.Second
	if in.TimeoutMS > 0 {
		timeout = time.Duration(in.TimeoutMS) * time.Millisecond
		if timeout > 600*time.Second {
			timeout = 600 * time.Second
		}
	}

	// Background-capable path: requires a task manager on the context.
	if tc != nil && tc.Tasks != nil && !BackgroundTasksDisabled() {
		return runBashManaged(ctx, tc, in.Command, timeout, in.RunInBackground)
	}
	// Fallback: synchronous execution (unchanged legacy behavior).
	return runBashSync(ctx, tc, in.Command, timeout)
}

// runBashManaged runs the command through the task manager, supporting explicit
// background launch and timeout auto-backgrounding.
func runBashManaged(ctx context.Context, tc *ToolContext, command string, timeout time.Duration, explicitBG bool) (Result, error) {
	spec := SpawnSpec{Command: command, Kind: KindBash, WorkingDir: tc.WorkingDir, Description: short(command), Env: tc.Env}

	if explicitBG {
		t, err := tc.Tasks.Spawn(spec)
		if err != nil {
			return Errorf("Error: failed to start background command: " + err.Error()), nil
		}
		tc.Tasks.Background(t.ID)
		return Text(fmt.Sprintf(
			"Running in background as %s. Output file: %s\nUse TaskOutput{task_id:%q} or Read to read its output; you are notified when it completes.",
			t.ID, t.OutputPath, t.ID)), nil
	}

	t, finished, output, err := tc.Tasks.RunForeground(ctx, spec, timeout)
	if err != nil {
		if t != nil {
			tc.Tasks.Forget(t.ID)
		}
		return Errorf("Error: " + err.Error()), nil
	}
	if finished {
		info, _ := tc.Tasks.Get(t.ID)
		output = taskOutputFor(tc, t.ID, output) // before Forget removes the file
		tc.Tasks.Forget(t.ID)
		if strings.TrimSpace(output) == "" {
			output = "" // shown as "(no output)" or "(command produced no output)"
		}
		v := CommandOutput{Output: Capture(tc, output)}
		if info.Status == TaskFailed {
			v.Failed, v.ExitCode = true, info.ExitCode
		}
		// A watchdog kill carries no exit code of its own; the bare code would
		// read as a cryptic failure, so name the cap that fired instead.
		switch info.Reason {
		case terminateMemory:
			v.KilledBy, v.Cap = killedByMemoryCap, humanBytes(memCapBytes())
		case terminateOversize:
			v.KilledBy, v.Cap = killedByOutputCap, humanBytes(maxTaskOutputBytes)
		}
		return commandResult(v), nil
	}

	// Timed out while still running.
	if autoBackgroundAllowed(command) {
		tc.Tasks.Background(t.ID)
		return Text(fmt.Sprintf(
			"Command still running after %s; moved to background as %s. Output file: %s\nUse TaskOutput{task_id:%q} to read its output; you are notified when it completes.",
			timeout, t.ID, t.OutputPath, t.ID)), nil
	}
	out, _ := tc.Tasks.Output(t.ID, 0)
	tc.Tasks.Kill(t.ID)
	out = taskOutputFor(tc, t.ID, out)
	tc.Tasks.Forget(t.ID)
	return commandResult(CommandOutput{Output: Capture(tc, out), TimedOut: true, Timeout: timeout.String()}), nil
}

// runBashSync is the original blocking implementation, used when no task manager
// is available (background execution disabled).
func runBashSync(ctx context.Context, tc *ToolContext, command string, timeout time.Duration) (Result, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell, flags := shellCmd()
	cmd := exec.CommandContext(cctx, shell, append(flags, command)...)
	if tc != nil {
		cmd.Dir = tc.WorkingDir
		cmd.Env = withEnvIn(tc.WorkingDir, tc.Env)
	} else {
		cmd.Env = withEnv(nil)
	}
	if err := applyChild(cmd); err != nil {
		return Errorf("Error: failed to start command: " + err.Error()), nil
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	v := CommandOutput{Output: Capture(tc, buf.String())}
	if cctx.Err() == context.DeadlineExceeded {
		v.TimedOut, v.Timeout = true, timeout.String()
		return commandResult(v), nil
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			v.Failed, v.ExitCode = true, ee.ExitCode()
		} else {
			v.RunError = err.Error()
		}
	}
	return commandResult(v), nil
}

// autoBackgroundAllowed reports whether a timed-out command may be moved to the
// background instead of being killed.
func autoBackgroundAllowed(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	base := fields[0]
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	for _, d := range disallowedAutoBackground {
		if base == d {
			return false
		}
	}
	return true
}
