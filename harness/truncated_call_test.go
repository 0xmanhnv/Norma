package harness

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// cutToolTurn is a tool call whose arguments stop where the stream does, with
// the given stop reason.
func cutToolTurn(id, name, partial, stop string) []llm.StreamEvent {
	return []llm.StreamEvent{
		{Type: llm.SEToolUseStart, ToolID: id, ToolName: name},
		{Type: llm.SEToolInputJSON, Text: partial},
		{Type: llm.SEMessageDelta, StopReason: stop},
		{Type: llm.SEMessageStop},
	}
}

// A tool call cut off by the output limit is not run with empty arguments. The
// model is told the call was truncated, and with escalation on the retry gets
// the raised output cap.
func TestTruncatedToolCallIsNotRun(t *testing.T) {
	cases := []struct {
		name      string
		first     []llm.StreamEvent
		escalate  bool
		wantRuns  []string // arguments the tool ran with, across the run
		wantTrunc bool     // first call answered with the truncation error
		wantCap   int      // max_tokens of the second request
	}{
		{"cut mid-arguments", cutToolTurn("c1", "Write", `{"path":"a","content":"very long`, "max_tokens"), false,
			[]string{`{"path":"a"}`}, true, 1000},
		{"cut mid-arguments, escalation on", cutToolTurn("c1", "Write", `{"path":"a","content":"very long`, "max_tokens"), true,
			[]string{`{"path":"a"}`}, true, escalatedMaxTokens},
		{"second of two calls cut", append([]llm.StreamEvent{
			{Type: llm.SEToolUseStart, ToolID: "c0", ToolName: "Write"},
			{Type: llm.SEToolInputJSON, Text: `{"path":"z"}`},
		}, cutToolTurn("c1", "Write", `{"path":"a","con`, "max_tokens")...), false,
			[]string{`{"path":"z"}`, `{"path":"a"}`}, true, 1000},
		{"complete arguments at the limit still run", cutToolTurn("c1", "Write", `{"path":"full"}`, "max_tokens"), false,
			[]string{`{"path":"full"}`, `{"path":"a"}`}, false, 1000},
		{"no arguments at all still run", cutToolTurn("c1", "Write", ``, "tool_use"), false,
			[]string{`{}`, `{"path":"a"}`}, false, 1000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var ran []string
			write := tool.Build(tool.Spec{
				Name:   "Write",
				Schema: map[string]any{"type": "object"},
				Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
					return permission.Allowed()
				},
				Run: func(_ context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
					ran = append(ran, string(in))
					return tool.Text("ok"), nil
				},
			})
			turns := [][]llm.StreamEvent{c.first, toolTurn("c2", "Write", `{"path":"a"}`), textTurn("done")}
			var caps []int
			call := func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
				caps = append(caps, req.MaxTokens)
				evs := turns[len(caps)-1]
				return func(yield func(llm.StreamEvent, error) bool) {
					for _, ev := range evs {
						if !yield(ev, nil) {
							return
						}
					}
				}
			}
			in := QueryInput{
				Messages:          []llm.Message{llm.UserText("go")},
				Tools:             tool.NewRegistry(write),
				PermissionMode:    permission.ModeBypass,
				WorkingDir:        t.TempDir(),
				MaxTokens:         1000,
				EscalateMaxTokens: c.escalate,
				MaxTurns:          6,
			}
			term := drain(t, in, QueryDeps{CallModel: call})
			if term.Reason != ReasonCompleted {
				t.Fatalf("terminal = %s (%v), want completed", term.Reason, term.Err)
			}
			if strings.Join(ran, "|") != strings.Join(c.wantRuns, "|") {
				t.Fatalf("tool ran with %q, want %q", ran, c.wantRuns)
			}
			results := term.Messages[2].Content
			last := results[len(results)-1]
			if last.ToolUseID != "c1" {
				t.Fatalf("last result of the first turn answers %q, want c1", last.ToolUseID)
			}
			gotTrunc := last.IsError && last.ToolError != nil && last.ToolError.Code == tool.CodeTruncated
			if gotTrunc != c.wantTrunc {
				t.Fatalf("truncation error = %v, want %v: %+v", gotTrunc, c.wantTrunc, last)
			}
			if c.wantTrunc {
				text := last.Content[0].Text
				for _, want := range []string{"truncated", "cut off at the output limit", "smaller"} {
					if !strings.Contains(text, want) {
						t.Fatalf("truncation error %q does not mention %q", text, want)
					}
				}
			}
			if len(caps) < 3 || caps[0] != 1000 || caps[1] != c.wantCap || caps[2] != 1000 {
				t.Fatalf("max_tokens per request = %v, want [1000 %d 1000]", caps, c.wantCap)
			}
		})
	}
}
