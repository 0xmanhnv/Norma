package harness

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// failingStream yields evs and then fails with err, like a connection that
// drops in the middle of a response.
func failingStream(evs []llm.StreamEvent, err error) func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, ev := range evs {
				if !yield(ev, nil) {
					return
				}
			}
			yield(llm.StreamEvent{}, err)
		}
	}
}

// callLog records the arguments each call of a tool ran with.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (c *callLog) tool(name string, delay time.Duration, running *atomic.Int32) tool.CoreTool {
	return tool.Build(tool.Spec{
		Name:   name,
		Schema: map[string]any{"type": "object"},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(_ context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
			running.Add(1)
			defer running.Add(-1)
			time.Sleep(delay)
			c.mu.Lock()
			c.calls = append(c.calls, string(in))
			c.mu.Unlock()
			return tool.Text("wrote " + string(in)), nil
		},
	})
}

// A stream that breaks mid-response must not run the call it was still
// receiving, must wait for the calls that had already started, and must leave
// what ran in the conversation and the transcript.
func TestStreamErrorKeepsWhatRan(t *testing.T) {
	boom := errors.New("connection reset")
	complete := []llm.StreamEvent{
		{Type: llm.SETextDelta, Text: "writing both files"},
		{Type: llm.SEToolUseStart, ToolID: "c1", ToolName: "Write"},
		{Type: llm.SEToolInputJSON, Text: `{"path":"a"}`},
	}
	half := []llm.StreamEvent{
		{Type: llm.SEToolUseStart, ToolID: "c2", ToolName: "Write"},
		{Type: llm.SEToolInputJSON, Text: `{"path":"b","content":"half of the`},
	}
	cases := []struct {
		name      string
		events    []llm.StreamEvent
		wantCalls []string // arguments of the calls that ran
		wantMsgs  int      // messages in the conversation afterwards
	}{
		{"error before any tool call", complete[:1], nil, 1},
		{"error inside the only tool call", half, nil, 1},
		{"error after a complete call, inside the next", append(append([]llm.StreamEvent{}, complete...), half...), []string{`{"path":"a"}`}, 3},
		{"error after a complete call", append(append([]llm.StreamEvent{}, complete...), llm.StreamEvent{Type: llm.SETextDelta, Text: "and"}), []string{`{"path":"a"}`}, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var log callLog
			var running atomic.Int32
			rec := &capRecorder{}
			in := QueryInput{
				Messages:       []llm.Message{llm.UserText("go")},
				Tools:          tool.NewRegistry(log.tool("Write", 30*time.Millisecond, &running)),
				PermissionMode: permission.ModeBypass,
				WorkingDir:     t.TempDir(),
				Recorder:       rec,
			}
			term := drain(t, in, QueryDeps{CallModel: failingStream(c.events, boom)})
			if term.Reason != ReasonModelError || !errors.Is(term.Err, boom) {
				t.Fatalf("terminal = %s / %v, want model_error / %v", term.Reason, term.Err, boom)
			}
			if n := running.Load(); n != 0 {
				t.Fatalf("%d tool call(s) still running after the loop returned", n)
			}
			time.Sleep(60 * time.Millisecond) // a call started late would land here
			runtime.Gosched()
			log.mu.Lock()
			got := append([]string(nil), log.calls...)
			log.mu.Unlock()
			if len(got) != len(c.wantCalls) {
				t.Fatalf("tool ran with %q, want %q", got, c.wantCalls)
			}
			for i := range got {
				if got[i] != c.wantCalls[i] {
					t.Fatalf("tool ran with %q, want %q", got, c.wantCalls)
				}
			}
			if len(term.Messages) != c.wantMsgs {
				t.Fatalf("conversation has %d messages, want %d: %+v", len(term.Messages), c.wantMsgs, term.Messages)
			}
			if len(rec.msgs) != c.wantMsgs-1 {
				t.Fatalf("recorder got %d messages, want %d", len(rec.msgs), c.wantMsgs-1)
			}
			if c.wantMsgs != 3 {
				return
			}
			asst, results := term.Messages[1], term.Messages[2]
			uses := asst.ToolUses()
			if asst.Role != llm.RoleAssistant || len(uses) != 1 || uses[0].ID != "c1" || string(uses[0].Input) != `{"path":"a"}` {
				t.Fatalf("assistant turn = %+v, want the one completed call c1", asst)
			}
			if last := asst.Content[len(asst.Content)-1]; last.Type != llm.BlockToolUse {
				t.Fatalf("assistant turn ends in a %s block; nothing after the last completed call may be kept", last.Type)
			}
			if len(results.Content) != 1 || results.Content[0].ToolUseID != "c1" || results.Content[0].IsError {
				t.Fatalf("results = %+v, want the result of c1", results)
			}
			if len(llm.MessagesForAPI(term.Messages)) != 3 {
				t.Fatalf("the recorded exchange does not pair up: %+v", llm.MessagesForAPI(term.Messages))
			}
		})
	}
}
