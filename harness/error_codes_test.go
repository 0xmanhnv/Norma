package harness

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/0xmanhnv/norma/compaction"
	"github.com/0xmanhnv/norma/llm"
	"github.com/0xmanhnv/norma/permission"
	"github.com/0xmanhnv/norma/tool"
)

// fnTool builds a read-only tool whose Run is fn.
func fnTool(name string, fn func() (tool.Result, error)) tool.CoreTool {
	return tool.Build(tool.Spec{
		Name:   name,
		Schema: map[string]any{"type": "object"},
		ReadOnly: func(json.RawMessage) bool {
			return true
		},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) { return fn() },
	})
}

// runOneCall drives one tool call through the loop and returns the tool_result
// block as the conversation holds it, as the event stream delivered it, and as
// the recorder persisted it.
func runOneCall(t *testing.T, in QueryInput, name, input string) (inHistory, inEvent, recorded llm.ContentBlock) {
	t.Helper()
	m := &scriptedModel{turns: [][]llm.StreamEvent{toolTurn("c1", name, input), textTurn("done")}}
	rec := &capRecorder{}
	in.Messages = []llm.Message{llm.UserText("go")}
	in.Recorder = rec
	if in.WorkingDir == "" {
		in.WorkingDir = t.TempDir()
	}
	var term *Terminal
	for ev, err := range Query(context.Background(), in, QueryDeps{CallModel: m.call}) {
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Kind {
		case KindToolResult:
			inEvent = *ev.ToolResult
		case KindResult:
			term = ev.Terminal
		}
	}
	if term == nil || term.Reason != ReasonCompleted {
		t.Fatalf("terminal = %+v", term)
	}
	inHistory = term.Messages[2].Content[0]
	for _, msg := range rec.msgs {
		for _, b := range msg.Content {
			if b.Type == llm.BlockToolResult {
				recorded = b
			}
		}
	}
	return inHistory, inEvent, recorded
}

func wantToolError(t *testing.T, where string, b llm.ContentBlock, name string, code llm.Code) {
	t.Helper()
	if !b.IsError || b.ToolError == nil || b.ToolError.Name != name || b.ToolError.Code != code {
		t.Fatalf("%s: tool_result = %+v (tool error %+v), want %s/%s", where, b, b.ToolError, name, code)
	}
}

// Every way the harness itself fails a call is classified, and the class
// reaches the history, the tool_result event and the recorder alike. The text
// the model reads is the same as before codes existed.
func TestHarnessFailuresCarryCodes(t *testing.T) {
	ok := fnTool("Probe", func() (tool.Result, error) { return tool.Text("fine"), nil })
	cases := []struct {
		name     string
		in       QueryInput
		call     string
		input    string
		code     llm.Code
		wantText string
	}{
		{"unknown tool", QueryInput{Tools: tool.NewRegistry(ok)}, "Nope", `{}`, tool.CodeUnknownTool, `"Nope"`},
		{"invalid input", QueryInput{Tools: tool.NewRegistry(tool.NewRead())}, "Read", `{}`, tool.CodeInvalidInput, "Error: invalid tool input:"},
		{"permission denied", QueryInput{Tools: tool.NewRegistry(tool.NewWrite()), PermissionMode: permission.ModePlan},
			"Write", `{"file_path":"x","content":"y"}`, tool.CodePermissionDenied, "Tool call was not permitted:"},
		{"hook blocked", QueryInput{Tools: tool.NewRegistry(ok), Hooks: blockingHooks{}}, "Probe", `{}`, tool.CodeHookBlocked, "Blocked by hook: no"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, ev, rec := runOneCall(t, tc.in, tc.call, tc.input)
			wantToolError(t, "history", h, tc.call, tc.code)
			wantToolError(t, "event", ev, tc.call, tc.code)
			wantToolError(t, "recorder", rec, tc.call, tc.code)
			if !strings.Contains(h.Content[0].Text, tc.wantText) {
				t.Fatalf("model-facing text changed: %q", h.Content[0].Text)
			}
		})
	}
}

// A tool's own error keeps its code across the seam; a plain error and a panic
// are CodeUnknown. A success, or an error result nothing classified, carries no
// tool error at all.
func TestToolErrorCodePropagation(t *testing.T) {
	const quota llm.Code = "host_quota_exceeded"
	cases := []struct {
		name     string
		fn       func() (tool.Result, error)
		code     llm.Code // "" = no ToolError expected
		wantText string
	}{
		{"coded error", func() (tool.Result, error) {
			return tool.Result{}, llm.NewError(quota, "quota exceeded")
		}, quota, "Error: quota exceeded"},
		{"wrapped coded error", func() (tool.Result, error) {
			return tool.Result{}, errors.Join(errors.New("ctx"), llm.NewError(quota, "quota exceeded"))
		}, quota, "quota exceeded"},
		{"plain error", func() (tool.Result, error) { return tool.Result{}, errors.New("disk on fire") }, llm.CodeUnknown, "Error: disk on fire"},
		{"panic with a string", func() (tool.Result, error) { panic("kaboom") }, llm.CodeUnknown, "Error: panic: kaboom"},
		{"error result with a code", func() (tool.Result, error) {
			r := tool.Errorf("not today")
			r.ErrorCode = quota
			return r, nil
		}, quota, "not today"},
		{"unclassified error result", func() (tool.Result, error) { return tool.Errorf("meh"), nil }, "", "meh"},
		{"success", func() (tool.Result, error) {
			r := tool.Text("fine")
			r.ErrorCode = quota // ignored: not an error
			return r, nil
		}, "", "fine"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, ev, rec := runOneCall(t, QueryInput{Tools: tool.NewRegistry(fnTool("Probe", tc.fn))}, "Probe", `{}`)
			if !strings.Contains(h.Content[0].Text, tc.wantText) {
				t.Fatalf("text = %q, want %q", h.Content[0].Text, tc.wantText)
			}
			for where, b := range map[string]llm.ContentBlock{"history": h, "event": ev, "recorder": rec} {
				if tc.code == "" {
					if b.ToolError != nil {
						t.Fatalf("%s: unexpected tool error %+v", where, b.ToolError)
					}
					continue
				}
				wantToolError(t, where, b, "Probe", tc.code)
			}
		})
	}
}

// The host callback sees the same code the transcript gets.
func TestOnToolResultSeesErrorCode(t *testing.T) {
	var got llm.Code
	in := QueryInput{
		Tools: tool.NewRegistry(fnTool("Probe", func() (tool.Result, error) { return tool.Result{}, errors.New("x") })),
		OnToolResult: func(_ context.Context, _, _ string, _ json.RawMessage, res tool.Result) {
			got = res.ErrorCode
		},
	}
	runOneCall(t, in, "Probe", `{}`)
	if got != llm.CodeUnknown {
		t.Fatalf("OnToolResult saw code %q", got)
	}
}

// A call cut off by cancellation gets a synthetic result classified as
// interrupted, so a retry policy can tell it from a call that failed.
func TestInterruptedCallIsClassified(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	stuck := fnTool("Stuck", func() (tool.Result, error) { <-release; return tool.Text("late"), nil })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	m := &scriptedModel{turns: [][]llm.StreamEvent{toolTurn("c1", "Stuck", `{}`), textTurn("unreached")}}
	in := QueryInput{Messages: []llm.Message{llm.UserText("go")}, Tools: tool.NewRegistry(stuck), WorkingDir: t.TempDir()}
	term := drainWithCtx(t, ctx, in, QueryDeps{CallModel: m.call})
	if term.Reason != ReasonAbortedTools {
		t.Fatalf("reason = %v", term.Reason)
	}
	wantToolError(t, "history", term.Messages[2].Content[0], "Stuck", tool.CodeInterrupted)
}

type blockingHooks struct{}

func (blockingHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return true, "no", nil
}
func (blockingHooks) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (blockingHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return false, nil, ""
}

// overflowOnce fails the first model call with err, then answers.
func overflowOnce(err error) func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	calls := 0
	return func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		calls++
		first := calls == 1
		return func(yield func(llm.StreamEvent, error) bool) {
			if first {
				yield(llm.StreamEvent{}, err)
				return
			}
			for _, e := range textTurn("recovered") {
				if !yield(e, nil) {
					return
				}
			}
		}
	}
}

// reactiveCounter recovers every time and uses compaction's real overflow test.
type reactiveCounter struct{ n int }

func (c *reactiveCounter) Pre(_ context.Context, msgs []llm.Message, _ int) []llm.Message {
	return msgs
}
func (c *reactiveCounter) Reactive(_ context.Context, msgs []llm.Message) ([]llm.Message, bool) {
	c.n++
	return msgs, true
}
func (c *reactiveCounter) IsOverflow(err error) bool { return compaction.IsOverflow(err) }

// A provider overflow classified by code drives reactive recovery even when its
// text matches none of the known phrasings; a rate limit whose text happens to
// match does not. Without recovery, the code reaches the terminal error.
func TestOverflowCodeDrivesRecovery(t *testing.T) {
	run := func(err error, c Compactor) *Terminal {
		in := QueryInput{Messages: []llm.Message{llm.UserText("go")}, Tools: tool.NewRegistry(), Compactor: c}
		return drain(t, in, QueryDeps{CallModel: overflowOnce(err)})
	}

	c := &reactiveCounter{}
	term := run(llm.NewError(llm.CodeContextOverflow, "provider: la solicitud excede la ventana"), c)
	if c.n != 1 || term.Reason != ReasonCompleted {
		t.Fatalf("classified overflow: reactive=%d terminal=%v", c.n, term.Reason)
	}

	c = &reactiveCounter{}
	term = run(llm.NewError(llm.CodeRateLimited, "provider: status 429: too many tokens for this context length"), c)
	if c.n != 0 || term.Reason != ReasonModelError || !llm.IsCode(term.Err, llm.CodeRateLimited) {
		t.Fatalf("rate limit: reactive=%d terminal=%v err=%v", c.n, term.Reason, term.Err)
	}

	term = run(llm.NewError(llm.CodeContextOverflow, "x"), nil)
	if !llm.IsCode(term.Err, llm.CodeContextOverflow) {
		t.Fatalf("terminal error lost its code: %v (%q)", term.Err, llm.CodeOf(term.Err))
	}
}
