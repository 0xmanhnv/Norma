package harness

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// A tool goroutine asks whether the run is settling when its call starts. A
// tool that ignores cancellation is still on that goroutine when the run's
// deadline moves the loop into wrap-up, and the loop then writes the same
// state with nothing in between to order the two. The state must be safe to
// share: under -race this test reports the unsynchronized access, every run.
func TestSettlementStateIsSafeForLateToolGoroutines(t *testing.T) {
	returned := make(chan struct{})
	slow := tool.Build(tool.Spec{
		Name:        "slow",
		Description: "ignores cancellation",
		Schema:      map[string]any{"type": "object"},
		Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			defer close(returned)
			time.Sleep(300 * time.Millisecond) // well past MaxDuration, and deaf to ctx
			return tool.Text("late"), nil
		},
	})
	m := &scriptedModel{turns: [][]llm.StreamEvent{toolTurn("a", "slow", `{}`), textTurn("wrapped up")}}
	in := QueryInput{
		Messages:       []llm.Message{llm.UserText("go")},
		Tools:          tool.NewRegistry(slow),
		PermissionMode: permission.ModeBypass,
		WorkingDir:     t.TempDir(),
		MaxDuration:    50 * time.Millisecond,
		Settlement:     &Settlement{Prompt: "wrap up", DisabledTools: []string{"slow"}},
	}
	term := drainWithCtx(t, context.Background(), in, QueryDeps{CallModel: m.call})
	if term.Reason != ReasonTimeout {
		t.Fatalf("reason = %v, want %v", term.Reason, ReasonTimeout)
	}
	<-returned // keep the test alive until the tool goroutine is done
}
