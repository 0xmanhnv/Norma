package harness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// A non-streaming response the output limit cut inside a tool call is treated
// like a cut stream: the call is answered with the truncation error and never
// runs, and with escalation on the retry gets the raised cap. The adapter marks
// such a call (ContentBlock.Truncated) because its arguments can arrive as a
// well-formed object that only looks complete; arguments that are not JSON at
// all on a max_tokens turn are caught without the mark.
func TestNonStreamingTruncatedToolCallIsNotRun(t *testing.T) {
	cut := func(input string, marked bool, stop string) syncTurn {
		return syncTurn{stop: stop, msg: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: llm.BlockToolUse, ID: "c1", Name: "Write", Input: []byte(input), Truncated: marked},
		}}}
	}
	cases := []struct {
		name      string
		first     syncTurn
		wantTrunc bool
	}{
		{"marked by the adapter, arguments parse", cut(`{"path":"a"}`, true, "max_tokens"), true},
		{"unmarked, arguments are not JSON", cut(`{"path":"a","content":"very lo`, false, "max_tokens"), true},
		{"complete call at the limit runs", cut(`{"path":"full"}`, false, "max_tokens"), false},
		{"ordinary call runs", cut(`{"path":"full"}`, false, "tool_use"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var ran []string
			write := tool.Build(tool.Spec{
				Name:   "Write",
				Schema: map[string]any{"type": "object"},
				Run: func(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
					ran = append(ran, string(input))
					return tool.Text("written"), nil
				},
			})
			m := &scriptedSyncModel{turns: []syncTurn{c.first, syncTextTurn("done")}}
			var caps []int
			call := func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				caps = append(caps, req.MaxTokens)
				return m.call(ctx, req)
			}
			term := drain(t, QueryInput{
				Messages:          []llm.Message{llm.UserText("go")},
				Tools:             tool.NewRegistry(write),
				PermissionMode:    permission.ModeBypass,
				WorkingDir:        t.TempDir(),
				NonStreaming:      true,
				MaxTokens:         1000,
				EscalateMaxTokens: true,
			}, QueryDeps{CallModelSync: call})
			if term.Reason != ReasonCompleted {
				t.Fatalf("reason = %v", term.Reason)
			}
			var result llm.ContentBlock
			for _, msg := range term.Messages {
				for _, b := range msg.Content {
					if b.Type == llm.BlockToolResult {
						result = b
					}
				}
			}
			truncated := result.IsError && result.ToolError != nil && result.ToolError.Code == tool.CodeTruncated &&
				strings.Contains(result.Content[0].Text, "truncated")
			if truncated != c.wantTrunc {
				t.Fatalf("truncation error = %v, want %v (result %+v)", truncated, c.wantTrunc, result)
			}
			if c.wantTrunc && len(ran) != 0 {
				t.Fatalf("the cut call ran with %v", ran)
			}
			if !c.wantTrunc && len(ran) != 1 {
				t.Fatalf("the complete call ran %d times", len(ran))
			}
			wantCap := 1000
			if c.wantTrunc {
				wantCap = defaultEscalatedMaxTokens
			}
			if len(caps) != 2 || caps[1] != wantCap {
				t.Fatalf("output caps = %v, want the second to be %d", caps, wantCap)
			}
		})
	}
}
