package subagent

import (
	"context"
	"testing"

	"github.com/0xmanhnv/corvid/harness"
	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// A subagent charges its parent's Budget as it goes: fanning out cannot
// overrun the limit. Here the parent's first call (53 tokens) and the child's
// first call (53) reach the 100-token limit, so the child makes no second
// call and the parent ends with ReasonBudgetExhausted instead of calling the
// model again.
func TestSubagentSharesParentBudget(t *testing.T) {
	var lastTotal llm.Usage
	budget := harness.NewBudget(func(total llm.Usage) *harness.StopRequest {
		lastTotal = total
		if total.InputTokens+total.OutputTokens >= 100 {
			return &harness.StopRequest{Detail: "token limit"}
		}
		return nil
	})
	child := &scripted{turns: [][]llm.StreamEvent{toolTurn("c1", "Bash", `{"command":"echo hi"}`), textTurn("child done")}}
	agentTool := NewTool(Config{Provider: child, ParentTools: tool.NewRegistry(tool.DefaultTools()...),
		Agents: map[string]Definition{"w": {Prompt: []string{"child"}}}})
	parentModel := &scripted{turns: [][]llm.StreamEvent{toolTurn("p1", "Agent", `{"agent_type":"w","prompt":"go"}`), textTurn("parent done")}}
	in := harness.QueryInput{
		Provider:       parentModel,
		Messages:       []llm.Message{llm.UserText("start")},
		Tools:          tool.NewRegistry(append(tool.DefaultTools(), agentTool)...),
		WorkingDir:     t.TempDir(),
		PermissionMode: permission.ModeBypass,
		Budget:         budget,
	}
	var term *harness.Terminal
	for ev, err := range harness.Query(context.Background(), in, harness.QueryDeps{}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == harness.KindResult {
			term = ev.Terminal
		}
	}
	if term == nil || term.Reason != harness.ReasonBudgetExhausted {
		t.Fatalf("terminal = %+v, want budget_exhausted", term)
	}
	if len(child.turns) != 1 {
		t.Fatalf("the child made %d model calls after the budget ran out", 1-len(child.turns))
	}
	if len(parentModel.turns) != 1 {
		t.Fatal("the parent called the model again after the budget ran out")
	}
	if lastTotal.InputTokens+lastTotal.OutputTokens != 106 {
		t.Fatalf("budget saw %+v, want parent + child = 106 tokens", lastTotal)
	}
}
