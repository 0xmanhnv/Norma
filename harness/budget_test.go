package harness

import (
	"testing"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// A Budget stops the run before the model call that would go over it, with
// ReasonBudgetExhausted, and sees every token the run spent, the last
// response's included.
func TestBudgetStopsTheRun(t *testing.T) {
	var spent llm.Usage
	b := NewBudget(func(total llm.Usage) *StopRequest {
		spent = total
		if total.OutputTokens >= 20 {
			return &StopRequest{Detail: "output token cap"}
		}
		return nil
	})
	m := &scriptedModel{turns: [][]llm.StreamEvent{
		usageToolTurn("a", "noop", `{}`, 100, 10),
		usageToolTurn("b", "noop", `{}`, 100, 10),
		usageToolTurn("c", "noop", `{}`, 100, 10),
		usageTextTurn("done", 100, 10),
	}}
	in := QueryInput{
		Messages:       []llm.Message{llm.UserText("go")},
		Tools:          tool.NewRegistry(fnTool("noop", func() (tool.Result, error) { return tool.Text("ok"), nil })),
		PermissionMode: permission.ModeBypass,
		Budget:         b,
	}
	term := drain(t, in, QueryDeps{CallModel: m.call})
	if term.Reason != ReasonBudgetExhausted || term.Detail != "output token cap" {
		t.Fatalf("terminal = %+v", term)
	}
	if m.calls != 2 {
		t.Fatalf("model calls = %d, want 2 (the third is refused)", m.calls)
	}
	if spent.OutputTokens != 20 || spent.InputTokens != 200 {
		t.Fatalf("budget saw %+v, want the two calls made", spent)
	}
}

// The same holds on the non-streaming path.
func TestBudgetStopsTheRunNonStreaming(t *testing.T) {
	b := NewBudget(func(total llm.Usage) *StopRequest {
		if total.OutputTokens >= 5 {
			return &StopRequest{}
		}
		return nil
	})
	paid := func(st syncTurn) syncTurn { st.usage = llm.Usage{InputTokens: 10, OutputTokens: 5}; return st }
	m := &scriptedSyncModel{turns: []syncTurn{paid(syncToolTurn("a", "noop", `{}`)), paid(syncToolTurn("b", "noop", `{}`)), syncTextTurn("done")}}
	in := QueryInput{
		Messages:       []llm.Message{llm.UserText("go")},
		Tools:          tool.NewRegistry(fnTool("noop", func() (tool.Result, error) { return tool.Text("ok"), nil })),
		PermissionMode: permission.ModeBypass,
		NonStreaming:   true,
		Budget:         b,
	}
	term := drain(t, in, QueryDeps{CallModelSync: m.call})
	if term.Reason != ReasonBudgetExhausted || m.calls != 1 {
		t.Fatalf("terminal = %+v after %d calls, want budget_exhausted after 1", term, m.calls)
	}
}
