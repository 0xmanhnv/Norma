package harness

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// recordingCalls returns a model that plays turns and records the tool names of
// every request it is sent.
func recordingCalls(turns [][]llm.StreamEvent, toolsSeen *[][]string) func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		names := make([]string, 0, len(req.Tools))
		for _, ts := range req.Tools {
			names = append(names, ts.Name)
		}
		i := len(*toolsSeen)
		*toolsSeen = append(*toolsSeen, names)
		evs := turns[i]
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, ev := range evs {
				if !yield(ev, nil) {
					return
				}
			}
		}
	}
}

// prefixCompactor is a Compactor that only records the request prefixes the
// loop announces to it.
type prefixCompactor struct{ tools [][]string }

func (p *prefixCompactor) Pre(_ context.Context, msgs []llm.Message, _ int) []llm.Message {
	return msgs
}
func (p *prefixCompactor) Reactive(_ context.Context, msgs []llm.Message) ([]llm.Message, bool) {
	return msgs, false
}
func (p *prefixCompactor) IsOverflow(error) bool { return false }
func (p *prefixCompactor) SetRequestPrefix(prefix llm.CompletionRequest) {
	var names []string
	for _, ts := range prefix.Tools {
		names = append(names, ts.Name)
	}
	p.tools = append(p.tools, names)
}

// On hitting MaxTurns with a Settlement configured, the loop injects the
// wrap-up prompt and finishes with the original budget reason (not
// ReasonCompleted). By default the wrap-up requests carry the same tool list as
// the run before them, so a prompt cache over the tools and the history still
// matches, and the compactor is told nothing new.
func TestSettlementOnBudgetHit(t *testing.T) {
	var toolsSeen [][]string
	turns := [][]llm.StreamEvent{
		toolTurn("a", "Bash", `{"command":"echo 1"}`), // turn 1 hits MaxTurns=1
		textTurn("wrote back the facts"),              // wrap-up turn, a natural stop
	}
	comp := &prefixCompactor{}
	in := QueryInput{
		Messages:       []llm.Message{llm.UserText("go")},
		Tools:          tool.NewRegistry(tool.NewBash(), tool.NewSleep()),
		PermissionMode: permission.ModeBypass,
		WorkingDir:     t.TempDir(),
		MaxTurns:       1,
		Compactor:      comp,
		Settlement:     &Settlement{Prompt: "SETTLE_NOW", DisabledTools: []string{"Bash"}},
	}
	term := drain(t, in, QueryDeps{CallModel: recordingCalls(turns, &toolsSeen)})

	if term.Reason != ReasonMaxTurns {
		t.Fatalf("reason=%v, want ReasonMaxTurns (the budget's reason survives the wrap-up)", term.Reason)
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
		t.Fatal("the wrap-up prompt was not injected")
	}
	if len(toolsSeen) != 2 {
		t.Fatalf("model calls = %d, want 2 (the run, then the wrap-up)", len(toolsSeen))
	}
	if strings.Join(toolsSeen[1], ",") != strings.Join(toolsSeen[0], ",") {
		t.Fatalf("the tool list changed for the wrap-up: %v then %v", toolsSeen[0], toolsSeen[1])
	}
	if len(comp.tools) != 1 {
		t.Fatalf("the prefix was announced %d times, want once (it never changed)", len(comp.tools))
	}
}

// HideDisabledTools removes the disabled schemas from the wrap-up requests.
// The prefix announcement follows: the compactor is told the tool list the
// wrap-up requests really carry, so a summary made during wrap-up shares their
// prefix. A hidden tool the model calls anyway is still refused.
func TestSettlementHideDisabledTools(t *testing.T) {
	var toolsSeen [][]string
	turns := [][]llm.StreamEvent{
		toolTurn("a", "Bash", `{"command":"echo 1"}`),
		toolTurn("b", "Bash", `{"command":"touch ran-in-wrapup"}`), // from memory: the schema is gone
		textTurn("wrote back the facts"),
	}
	comp := &prefixCompactor{}
	dir := t.TempDir()
	in := QueryInput{
		Messages:       []llm.Message{llm.UserText("go")},
		Tools:          tool.NewRegistry(tool.NewBash(), tool.NewSleep()),
		PermissionMode: permission.ModeBypass,
		WorkingDir:     dir,
		MaxTurns:       1,
		Compactor:      comp,
		Settlement:     &Settlement{Prompt: "SETTLE_NOW", DisabledTools: []string{"Bash"}, HideDisabledTools: true, MaxTurns: 3},
	}
	term := drain(t, in, QueryDeps{CallModel: recordingCalls(turns, &toolsSeen)})
	if term.Reason != ReasonMaxTurns {
		t.Fatalf("reason=%v, want ReasonMaxTurns", term.Reason)
	}
	if len(toolsSeen) != 3 || !contains(toolsSeen[0], "Bash") {
		t.Fatalf("tool lists seen: %v", toolsSeen)
	}
	for i := 1; i < len(toolsSeen); i++ {
		if contains(toolsSeen[i], "Bash") || !contains(toolsSeen[i], tool.SleepName) {
			t.Fatalf("wrap-up request %d lists %v, want Bash hidden and the rest kept", i, toolsSeen[i])
		}
	}
	if len(comp.tools) != 2 {
		t.Fatalf("the prefix was announced %d times, want twice (run start, then the wrap-up list)", len(comp.tools))
	}
	if strings.Join(comp.tools[1], ",") != strings.Join(toolsSeen[1], ",") {
		t.Fatalf("the compactor was told %v, the wrap-up requests carry %v", comp.tools[1], toolsSeen[1])
	}
	if _, err := os.Stat(filepath.Join(dir, "ran-in-wrapup")); err == nil {
		t.Fatal("a hidden, disabled tool ran during wrap-up")
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

// The wrap-up requests carry the same tool list as the run before it (so a
// prompt cache over the tools and history still matches), and a call to a
// disabled tool, direct or through ExecuteExtraTool, is refused without running.
func TestSettlementRefusesDisabledTools(t *testing.T) {
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
		Settlement:     &Settlement{Prompt: "SETTLE_NOW", DisabledTools: []string{"Bash"}, MaxTurns: 5},
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
