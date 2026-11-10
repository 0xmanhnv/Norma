package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legacyReadText is Read's text-file answer as it was built before Read
// declared an output: the migration must not change a byte of it.
func legacyReadText(tc *ToolContext, data []byte, offset, limit int) string {
	lines := strings.Split(string(data), "\n")
	start := 0
	if offset > 0 {
		start = offset - 1
	}
	if start > len(lines) {
		start = len(lines)
	}
	if limit <= 0 {
		limit = 2000
	}
	end := min(start+limit, len(lines))
	var b strings.Builder
	for i := start; i < end; i++ {
		line := lines[i]
		if len(line) > maxLineLen {
			line = line[:maxLineLen] + "… [line truncated]"
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, line)
	}
	if b.Len() == 0 {
		if len(data) == 0 {
			return "(file is empty)"
		}
		return "(no lines in the requested range)"
	}
	return readCapture(tc, b.String(), start)
}

func TestReadOutputMatchesLegacyText(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 2500)
	files := map[string]string{
		"small.txt":   "alpha\nbeta\ngamma\n",
		"nonl.txt":    "one\ntwo",
		"crlf.txt":    "a\r\nb\r\n",
		"empty.txt":   "",
		"long.txt":    "short\n" + long + "\nend\n",
		"big.txt":     strings.Repeat("padding line of some length\n", 300) + "TAIL\n",
		"unicode.txt": "héllo\n世界\n",
	}
	for name, body := range files {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	cases := []struct {
		file          string
		offset, limit int
		maxOut        int
	}{
		{"small.txt", 0, 0, 0},
		{"small.txt", 2, 1, 0},
		{"small.txt", 99, 0, 0},
		{"nonl.txt", 0, 0, 0},
		{"crlf.txt", 0, 0, 0},
		{"empty.txt", 0, 0, 0},
		{"empty.txt", 5, 0, 0},
		{"long.txt", 0, 0, 0},
		{"big.txt", 0, 0, 200},  // cut to the budget at a line boundary
		{"big.txt", 50, 0, 200}, // ... from an offset
		{"long.txt", 2, 1, 10},  // a single line longer than the whole budget
		{"unicode.txt", 0, 0, 0},
	}
	for _, c := range cases {
		name := fmt.Sprintf("%s@%d+%d/%d", c.file, c.offset, c.limit, c.maxOut)
		t.Run(name, func(t *testing.T) {
			tc := &ToolContext{WorkingDir: dir, MaxOutputChars: c.maxOut}
			args, _ := json.Marshal(map[string]any{"file_path": c.file, "offset": c.offset, "limit": c.limit})
			res, err := NewRead().Call(context.Background(), args, tc)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(filepath.Join(dir, c.file))
			if want := legacyReadText(tc, data, c.offset, c.limit); res.Flatten() != want {
				t.Fatalf("content changed:\n got %q\nwant %q", res.Flatten(), want)
			}
		})
	}
}

func TestReadValue(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Repeat("padding line of some length\n", 300)), 0o644)
	args, _ := json.Marshal(map[string]any{"file_path": "big.txt", "offset": 10})
	res, err := NewRead().Call(context.Background(), args, &ToolContext{WorkingDir: dir, MaxOutputChars: 200})
	if err != nil {
		t.Fatal(err)
	}
	v, ok := res.Data.(FileLines)
	if !ok {
		t.Fatalf("Data = %#v", res.Data)
	}
	if v.FilePath != "big.txt" || v.StartLine != 10 || v.TotalLines != 301 || len(v.Lines) == 0 ||
		v.Lines[0] != "padding line of some length" || v.NextOffset != 10+len(v.Lines) {
		t.Fatalf("value = %+v", v)
	}

	// Answers that are not file lines stay plain text, with no value.
	args, _ = json.Marshal(map[string]any{"file_path": "."})
	res, _ = NewRead().Call(context.Background(), args, &ToolContext{WorkingDir: dir})
	if res.Data != nil || !strings.Contains(res.Flatten(), "is a directory") {
		t.Fatalf("directory: %+v", res)
	}
}

func TestBashValue(t *testing.T) {
	run := func(t *testing.T, tc *ToolContext, cmd string) Result {
		t.Helper()
		args, _ := json.Marshal(map[string]any{"command": cmd})
		res, err := NewBash().Call(context.Background(), args, tc)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	for name, mk := range map[string]func(t *testing.T) *ToolContext{
		"sync": func(t *testing.T) *ToolContext { return &ToolContext{WorkingDir: t.TempDir()} },
		"managed": func(t *testing.T) *ToolContext {
			m, err := NewManager("bash-value-" + filepath.Base(t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(m.Cleanup)
			return &ToolContext{WorkingDir: t.TempDir(), Tasks: m}
		},
	} {
		t.Run(name, func(t *testing.T) {
			res := run(t, mk(t), "echo hi")
			if res.IsError || res.Flatten() != "hi\n" {
				t.Fatalf("success: %+v", res)
			}
			if v, ok := res.Data.(CommandOutput); !ok || v.Output != "hi\n" || v.Failed {
				t.Fatalf("success value: %#v", res.Data)
			}

			res = run(t, mk(t), "echo oops; exit 3")
			if !res.IsError || res.Flatten() != "oops\n\n\n[exit code 3]" {
				t.Fatalf("failure: %q", res.Flatten())
			}
			if v, ok := res.Data.(CommandOutput); !ok || !v.Failed || v.ExitCode != 3 {
				t.Fatalf("failure value: %#v", res.Data)
			}

			res = run(t, mk(t), "exit 1")
			if res.Flatten() != "(no output)\n\n[exit code 1]" {
				t.Fatalf("silent failure: %q", res.Flatten())
			}
			res = run(t, mk(t), "true")
			if res.IsError || res.Flatten() != "(command produced no output)" {
				t.Fatalf("silent success: %q", res.Flatten())
			}
		})
	}
}

func TestCommandOutputRender(t *testing.T) {
	for _, c := range []struct {
		v    CommandOutput
		want string
	}{
		{CommandOutput{Output: "partial", TimedOut: true, Timeout: "2m0s"}, "partial\n\n[command timed out after 2m0s]"},
		{CommandOutput{Output: "", RunError: "exec: not found"}, "\n\n[failed to run: exec: not found]"},
		{CommandOutput{Output: "  \n", Failed: true, ExitCode: 2}, "  \n\n\n[exit code 2]"},
		{CommandOutput{Failed: true, KilledBy: killedByMemoryCap, Cap: "2GiB"}, "(no output)\n\n[terminated: the command's process group exceeded the 2GiB per-command memory cap and was killed — stream or page the data instead of accumulating it in memory]"},
		{CommandOutput{Output: "tail", Failed: true, KilledBy: killedByOutputCap, Cap: "50MiB"}, "tail\n\n[terminated: output exceeded the 50MiB per-task output cap]"},
	} {
		p, err := bashOutput.Project(nil, c.v)
		if err != nil || p.Content[0].Text != c.want {
			t.Fatalf("render %+v = %q, %v; want %q", c.v, p.Content[0].Text, err, c.want)
		}
	}
}

// recordingStore is a SpillStore that keeps items in memory.
type recordingStore struct {
	items []SpillItem
	err   error
}

func (s *recordingStore) Store(_ context.Context, item SpillItem) (Spilled, error) {
	if s.err != nil {
		return Spilled{}, s.err
	}
	s.items = append(s.items, item)
	loc := fmt.Sprintf("mem://%d", len(s.items))
	return Spilled{Locator: loc, Guidance: "Fetch the rest from " + loc}, nil
}

// A host store replaces the local files: Capture stores the whole output there
// and shows the store's guidance; the result is still recognised as captured.
func TestCaptureUsesSpillStore(t *testing.T) {
	store := &recordingStore{}
	out := strings.Repeat("y", 500)
	got := Capture(&ToolContext{MaxOutputChars: 100, Spill: store, OutputDir: t.TempDir()}, out)
	if len(store.items) != 1 || string(store.items[0].Data) != out || store.items[0].MediaType != MediaText {
		t.Fatalf("stored = %+v", store.items)
	}
	if !strings.HasPrefix(got, strings.Repeat("y", 100)) || !strings.Contains(got, "Fetch the rest from mem://1") {
		t.Fatalf("captured = %q", got)
	}
	if !alreadyCaptured(got) {
		t.Fatal("output through a custom store must still be recognised as captured")
	}
}

// A failing store costs the overflow, never the call.
func TestCaptureFallsBackWhenStoreFails(t *testing.T) {
	got := Capture(&ToolContext{MaxOutputChars: 100, Spill: &recordingStore{err: errors.New("down")}}, strings.Repeat("z", 500))
	if !strings.Contains(got, "characters truncated") {
		t.Fatalf("captured = %q", got)
	}
}

// The default store is unchanged: an absolute path in OutputDir.
func TestLocalSpillDefault(t *testing.T) {
	dir := t.TempDir()
	got := Capture(&ToolContext{MaxOutputChars: 10, OutputDir: dir}, strings.Repeat("q", 50))
	files, _ := os.ReadDir(dir)
	if len(files) != 1 || !strings.HasSuffix(files[0].Name(), ".txt") {
		t.Fatalf("files = %v", files)
	}
	if want := "Full output saved to  " + filepath.Join(dir, files[0].Name()); !strings.Contains(got, want) {
		t.Fatalf("captured = %q, want it to contain %q", got, want)
	}
}

func TestSpillValue(t *testing.T) {
	if _, ok, _ := SpillValue(context.Background(), &ToolContext{}, 1); ok {
		t.Fatal("no store: nothing to spill to")
	}
	store := &recordingStore{}
	sp, ok, err := SpillValue(context.Background(), &ToolContext{Spill: store}, point{X: 1, Y: 2})
	if !ok || err != nil || sp.Locator != "mem://1" {
		t.Fatalf("SpillValue = %+v, %v, %v", sp, ok, err)
	}
	if store.items[0].MediaType != MediaJSON || string(store.items[0].Data) != `{"x":1,"y":2}` {
		t.Fatalf("stored = %+v", store.items[0])
	}
	dir := t.TempDir()
	sp, _, err = SpillValue(context.Background(), &ToolContext{OutputDir: dir}, point{X: 1})
	if err != nil || !strings.HasSuffix(sp.Locator, ".json") {
		t.Fatalf("local value spill = %+v, %v", sp, err)
	}
}
