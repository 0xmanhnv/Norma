package harness

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xmanhnv/norma/llm"
	"github.com/0xmanhnv/norma/permission"
	"github.com/0xmanhnv/norma/tool"
)

// On hitting MaxTurns with a Settlement configured, the loop injects the wrap-up
// prompt, hides DisabledTools during wrap-up, and finishes with the ORIGINAL
// budget reason (not ReasonCompleted).
func TestSettlementOnBudgetHit(t *testing.T) {
	var toolsSeen [][]string
	turns := [][]llm.StreamEvent{
		toolTurn("a", "Bash", `{"command":"echo 1"}`), // turn 1 → hits MaxTurns=1
		textTurn("已写回事实并总结"),                          // settlement turn → natural stop
	}
	call := func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		names := make([]string, 0, len(req.Tools))
		for _, ts := range req.Tools {
			names = append(names, ts.Name)
		}
		i := len(toolsSeen)
		toolsSeen = append(toolsSeen, names)
		evs := turns[i]
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, ev := range evs {
				if !yield(ev, nil) {
					return
				}
			}
		}
	}
	in := QueryInput{
		Messages:       []llm.Message{llm.UserText("go")},
		Tools:          tool.NewRegistry(tool.NewBash()),
		PermissionMode: permission.ModeBypass,
		WorkingDir:     t.TempDir(),
		MaxTurns:       1,
		Settlement:     &Settlement{Prompt: "SETTLE_NOW 请写回事实", DisabledTools: []string{"Bash"}},
	}
	term := drain(t, in, QueryDeps{CallModel: call})

	if term.Reason != ReasonMaxTurns {
		t.Fatalf("reason=%v, want ReasonMaxTurns（收尾后仍报原预算 reason）", term.Reason)
	}
	found := false
	for _, m := range term.Messages {
		for _, c := range m.Content {
			if strings.Contains(c.Text, "SETTLE_NOW") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("收尾提示未被注入到对话")
	}
	if len(toolsSeen) < 2 {
		t.Fatalf("应至少 2 次模型调用(主+收尾)，得 %d", len(toolsSeen))
	}
	if !contains(toolsSeen[0], "Bash") {
		t.Fatalf("主阶段应能看到 Bash: %v", toolsSeen[0])
	}
	if contains(toolsSeen[1], "Bash") {
		t.Fatalf("收尾阶段应屏蔽 Bash: %v", toolsSeen[1])
	}
}

// With no Settlement, hitting the budget finishes immediately (unchanged behavior).
func TestNoSettlementFinishesImmediately(t *testing.T) {
	m := &scriptedModel{turns: [][]llm.StreamEvent{
		toolTurn("a", "Bash", `{"command":"echo 1"}`),
		toolTurn("b", "Bash", `{"command":"echo 2"}`),
	}}
	in := QueryInput{
		Tools:          tool.NewRegistry(tool.NewBash()),
		PermissionMode: permission.ModeBypass,
		WorkingDir:     t.TempDir(),
		MaxTurns:       1,
	}
	term := drain(t, in, QueryDeps{CallModel: m.call})
	if term.Reason != ReasonMaxTurns {
		t.Fatalf("reason=%v", term.Reason)
	}
	if m.calls != 1 {
		t.Fatalf("无收尾应只调用 1 次模型，得 %d", m.calls)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// With KeepToolSchemas the wrap-up requests carry the same tool list as the run
// before it (so a prompt cache over the tools and history still matches), and a
// call to a disabled tool, direct or through ExecuteExtraTool, is refused
// without running.
func TestSettlementKeepToolSchemas(t *testing.T) {
	var toolsSeen [][]string
	turns := [][]llm.StreamEvent{
		toolTurn("a", "Bash", `{"command":"echo 1"}`),
		toolTurn("b", "Bash", `{"command":"touch ran-in-wrapup"}`),
		toolTurn("c", tool.ExecuteExtraToolName, `{"tool_name":"Bash","params":{"command":"touch ran-via-extra"}}`),
		textTurn("wrote back the facts"),
	}
	call := func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		names := make([]string, 0, len(req.Tools))
		for _, ts := range req.Tools {
			names = append(names, ts.Name)
		}
		i := len(toolsSeen)
		toolsSeen = append(toolsSeen, names)
		evs := turns[i]
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, ev := range evs {
				if !yield(ev, nil) {
					return
				}
			}
		}
	}
	dir := t.TempDir()
	in := QueryInput{
		Messages:       []llm.Message{llm.UserText("go")},
		Tools:          tool.NewRegistry(tool.NewBash(), tool.NewExecuteExtraTool(tool.NewRegistry(tool.NewBash()), nil)),
		PermissionMode: permission.ModeBypass,
		WorkingDir:     dir,
		MaxTurns:       1,
		Settlement:     &Settlement{Prompt: "SETTLE_NOW", DisabledTools: []string{"Bash"}, KeepToolSchemas: true, MaxTurns: 5},
	}
	term := drain(t, in, QueryDeps{CallModel: call})
	if term.Reason != ReasonMaxTurns {
		t.Fatalf("reason=%v, want ReasonMaxTurns", term.Reason)
	}
	if len(toolsSeen) != 4 {
		t.Fatalf("model calls = %d, want 4", len(toolsSeen))
	}
	for i := 1; i < len(toolsSeen); i++ {
		if strings.Join(toolsSeen[i], ",") != strings.Join(toolsSeen[0], ",") {
			t.Fatalf("tool list changed in wrap-up call %d: %v vs %v", i, toolsSeen[i], toolsSeen[0])
		}
	}
	refused := 0
	for _, m := range term.Messages {
		for _, c := range m.Content {
			if c.Type == llm.BlockToolResult && c.IsError && len(c.Content) == 1 && strings.Contains(c.Content[0].Text, "not available during wrap-up") {
				refused++
			}
		}
	}
	if refused != 2 {
		t.Fatalf("refused calls = %d, want 2", refused)
	}
	for _, f := range []string{"ran-in-wrapup", "ran-via-extra"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Fatalf("%s: the disabled tool ran during wrap-up", f)
		}
	}
}
