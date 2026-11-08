package tool

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// bigOutputCommand prints a FIRST marker, 60000 bytes of filler and a LAST
// marker: twice the default output budget, so a tail-only read drops FIRST.
const bigOutputCommand = `echo FIRST-LINE; head -c 60000 /dev/zero | tr '\0' 'x'; echo; echo LAST-LINE`

var spillPathRE = regexp.MustCompile(`Full output saved to\s+(\S+)`)

func spilled(t *testing.T, res Result) string {
	t.Helper()
	text := res.Content[0].Text
	m := spillPathRE.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no spill pointer in %q", text[max(0, len(text)-300):])
	}
	b, err := os.ReadFile(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBashSpillKeepsWholeOutput(t *testing.T) {
	if BackgroundTasksDisabled() {
		t.Skip("background tasks disabled")
	}
	tc := &ToolContext{WorkingDir: t.TempDir(), OutputDir: t.TempDir(), Tasks: newTestManager(t)}
	full := spilled(t, runTC(t, NewBash(), tc, map[string]any{"command": bigOutputCommand}))
	if !strings.HasPrefix(full, "FIRST-LINE\n") || !strings.HasSuffix(full, "LAST-LINE\n") || len(full) < 60000 {
		t.Fatalf("spill lost part of the output: %d bytes, starts %q", len(full), full[:min(20, len(full))])
	}

	// A failing command keeps its whole output too.
	res := runTC(t, NewBash(), tc, map[string]any{"command": bigOutputCommand + "; exit 3"})
	if full := spilled(t, res); !strings.HasPrefix(full, "FIRST-LINE\n") {
		t.Fatalf("failed command's spill starts %q", full[:20])
	}
	if !res.IsError || !strings.Contains(res.Content[0].Text, "[exit code 3]") {
		t.Fatalf("exit code lost: %q", res.Content[0].Text[max(0, len(res.Content[0].Text)-100):])
	}
}

func TestBashTailOnlyKeepsOldBehaviour(t *testing.T) {
	if BackgroundTasksDisabled() {
		t.Skip("background tasks disabled")
	}
	tc := &ToolContext{WorkingDir: t.TempDir(), OutputDir: t.TempDir(), Tasks: newTestManager(t), TailOnly: true}
	res := runTC(t, NewBash(), tc, map[string]any{"command": bigOutputCommand})
	text := res.Content[0].Text
	if strings.Contains(text, "FIRST-LINE") || !strings.Contains(text, "earlier output omitted") {
		t.Fatalf("tail-only result kept the head: %q", text[:min(120, len(text))])
	}
}

func TestTaskOutputOfFinishedTaskIsWhole(t *testing.T) {
	if BackgroundTasksDisabled() {
		t.Skip("background tasks disabled")
	}
	m := newTestManager(t)
	tc := &ToolContext{WorkingDir: t.TempDir(), OutputDir: t.TempDir(), Tasks: m}
	task, err := m.Spawn(SpawnSpec{Command: bigOutputCommand, Kind: KindBash, WorkingDir: tc.WorkingDir})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, m, task.ID)
	full := spilled(t, runTC(t, NewTaskOutput(), tc, map[string]any{"task_id": task.ID}))
	if !strings.Contains(full, "FIRST-LINE\n") || !strings.HasSuffix(full, "LAST-LINE\n") {
		t.Fatalf("TaskOutput spill lost part of the output (%d bytes)", len(full))
	}
	// Without an output directory there is nowhere to keep it: the tail, as before.
	tc.OutputDir = ""
	text := runTC(t, NewTaskOutput(), tc, map[string]any{"task_id": task.ID}).Content[0].Text
	if strings.Contains(text, "FIRST-LINE") {
		t.Fatal("no output dir, yet the head came back")
	}
}
