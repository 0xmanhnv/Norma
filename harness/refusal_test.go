package harness

import (
	"testing"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// The "refusal" stop reason is its own terminal state, not a completion, and
// the terminal carries the stop reason of the last model turn.
func TestStopReasonReachesTheTerminal(t *testing.T) {
	refusal := func(text string) []llm.StreamEvent {
		var evs []llm.StreamEvent
		if text != "" {
			evs = append(evs, llm.StreamEvent{Type: llm.SETextDelta, Text: text})
		}
		return append(evs, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "refusal"}, llm.StreamEvent{Type: llm.SEMessageStop})
	}
	cases := []struct {
		name       string
		turns      [][]llm.StreamEvent
		nonStream  bool
		wantReason TerminalReason
		wantStop   string
		wantCode   llm.Code
		wantMsgs   int
		wantText   string
	}{
		{"refusal with no content", [][]llm.StreamEvent{refusal("")}, false, ReasonRefusal, "refusal", llm.CodeRefusal, 1, ""},
		{"refusal with text", [][]llm.StreamEvent{refusal("I can't help with that.")}, false, ReasonRefusal, "refusal", llm.CodeRefusal, 2, "I can't help with that."},
		{"refusal after a tool turn", [][]llm.StreamEvent{toolTurn("c1", "Noop", `{}`), refusal("")}, false, ReasonRefusal, "refusal", llm.CodeRefusal, 3, ""},
		{"refusal, non-streaming", [][]llm.StreamEvent{refusal("")}, true, ReasonRefusal, "refusal", llm.CodeRefusal, 1, ""},
		{"ordinary completion", [][]llm.StreamEvent{textTurn("done")}, false, ReasonCompleted, "end_turn", "", 2, "done"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			noop := fnTool("Noop", func() (tool.Result, error) { return tool.Text("ok"), nil })
			in := QueryInput{
				Messages:       []llm.Message{llm.UserText("go")},
				Tools:          tool.NewRegistry(noop),
				PermissionMode: permission.ModeBypass,
				WorkingDir:     t.TempDir(),
				NonStreaming:   c.nonStream,
			}
			m := &scriptedModel{turns: c.turns}
			deps := QueryDeps{CallModel: m.call}
			if c.nonStream {
				deps.CallModelSync = (&scriptedSyncModel{turns: []syncTurn{{stop: "refusal"}}}).call
			}
			term := drain(t, in, deps)
			if term.Reason != c.wantReason || term.StopReason != c.wantStop {
				t.Fatalf("terminal = %s / stop %q, want %s / %q", term.Reason, term.StopReason, c.wantReason, c.wantStop)
			}
			if got := llm.CodeOf(term.Err); got != c.wantCode {
				t.Fatalf("terminal error code = %q (%v), want %q", got, term.Err, c.wantCode)
			}
			if term.Text != c.wantText {
				t.Fatalf("terminal text = %q, want %q", term.Text, c.wantText)
			}
			if len(term.Messages) != c.wantMsgs {
				t.Fatalf("conversation has %d messages, want %d: %+v", len(term.Messages), c.wantMsgs, term.Messages)
			}
			for i, msg := range term.Messages {
				if len(msg.Content) == 0 {
					t.Fatalf("message %d (%s) has no content", i, msg.Role)
				}
			}
		})
	}
}
