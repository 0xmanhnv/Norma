package subagent

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/0xmanhnv/corvid/harness"
	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// scripted plays back one list of stream events per model call.
type scripted struct {
	mu    sync.Mutex
	turns [][]llm.StreamEvent
}

func (s *scripted) Stream(_ context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	s.mu.Lock()
	var evs []llm.StreamEvent
	if len(s.turns) > 0 {
		evs, s.turns = s.turns[0], s.turns[1:]
	} else {
		evs = textTurn("(no more turns)")
	}
	s.mu.Unlock()
	return func(yield func(llm.StreamEvent, error) bool) {
		for _, ev := range evs {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func (s *scripted) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	acc := llm.NewAccumulator()
	for ev, err := range s.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		acc.Add(ev)
	}
	return acc.Message(), acc.StopReason, acc.Usage, nil
}

func textTurn(s string) []llm.StreamEvent {
	return []llm.StreamEvent{{Type: llm.SETextDelta, Text: s},
		{Type: llm.SEMessageDelta, StopReason: "end_turn", Usage: llm.Usage{InputTokens: 100, OutputTokens: 7}},
		{Type: llm.SEMessageStop}}
}

func toolTurn(id, name, input string) []llm.StreamEvent {
	return []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: id, ToolName: name}, {Type: llm.SEToolInputJSON, Text: input},
		{Type: llm.SEMessageDelta, StopReason: "tool_use", Usage: llm.Usage{InputTokens: 50, OutputTokens: 3}},
		{Type: llm.SEMessageStop}}
}

type childCall struct{ name, out string }

// runParent runs a parent agent whose only action is to start subagent "w"; the
// child runs one Bash command. It returns what the child's tool calls produced
// (seen through the inherited OnToolResult) and the parent's terminal.
func runParent(t *testing.T, parent harness.QueryInput, def Definition, childCmd string) ([]childCall, *harness.Terminal) {
	t.Helper()
	child := &scripted{turns: [][]llm.StreamEvent{toolTurn("c1", "Bash", `{"command":`+jsonString(childCmd)+`}`), textTurn("child done")}}
	agentTool := NewTool(Config{Provider: child, ParentTools: tool.NewRegistry(tool.DefaultTools()...),
		Agents: map[string]Definition{"w": def}})
	parent.Provider = &scripted{turns: [][]llm.StreamEvent{toolTurn("p1", "Agent", `{"agent_type":"w","prompt":"go"}`), textTurn("parent done")}}
	parent.Messages = []llm.Message{llm.UserText("start")}
	parent.Tools = tool.NewRegistry(append(tool.DefaultTools(), agentTool)...)
	parent.WorkingDir = t.TempDir()
	var mu sync.Mutex
	var calls []childCall
	parent.OnToolResult = func(_ context.Context, _, name string, _ json.RawMessage, res tool.Result) {
		mu.Lock()
		calls = append(calls, childCall{name, res.Flatten()})
		mu.Unlock()
	}
	var term *harness.Terminal
	for ev, err := range harness.Query(context.Background(), parent, harness.QueryDeps{}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == harness.KindResult {
			term = ev.Terminal
		}
	}
	return calls, term
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func bashOutput(calls []childCall) string {
	for _, c := range calls {
		if c.name == "Bash" {
			return c.out
		}
	}
	return "(no Bash call recorded)"
}

// A child with no tool filter must still obey the parent's deny rules: its Bash
// call must not run (checked by the side effect, not by a message).
func TestSubagentInheritsDenyRules(t *testing.T) {
	marker := t.TempDir() + "/ran"
	runParent(t, harness.QueryInput{PermissionMode: permission.ModeBypass, Disallowed: []string{"Bash"}},
		Definition{Prompt: []string{"child"}}, "touch "+marker)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("child ran Bash although the parent denies it")
	}
	// Control: without the deny rule the same child does run it.
	runParent(t, harness.QueryInput{PermissionMode: permission.ModeBypass}, Definition{Prompt: []string{"child"}}, "touch "+marker)
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("control: child did not run Bash with no deny rule, so the test proves nothing")
	}
}

// The child's shell gets the parent's BashEnv (proxy, CA, ...), and the parent's
// OnToolResult sees the child's tool calls.
func TestSubagentInheritsBashEnvAndObservers(t *testing.T) {
	calls, _ := runParent(t, harness.QueryInput{PermissionMode: permission.ModeBypass, BashEnv: []string{"PROBE_VAR=from-parent"}},
		Definition{Prompt: []string{"child"}}, "echo probe=$PROBE_VAR")
	if out := bashOutput(calls); !strings.Contains(out, "probe=from-parent") {
		t.Fatalf("child Bash did not see the parent's env: %q", out)
	}
}

// A definition can tighten the parent's mode but never loosen it. The parent is
// in dontAsk with only Agent allowed: a bypass definition must not let the
// child run Bash.
func TestSubagentModeOnlyTightens(t *testing.T) {
	marker := t.TempDir() + "/ran"
	strict := harness.QueryInput{PermissionMode: permission.ModeDontAsk, Allowed: []string{"Agent"}}
	runParent(t, strict, Definition{Prompt: []string{"child"}, PermissionMode: permission.ModeBypass}, "touch "+marker)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a bypass definition loosened a dontAsk parent")
	}
	// Control: a bypass parent with the same definition does run it.
	runParent(t, harness.QueryInput{PermissionMode: permission.ModeBypass},
		Definition{Prompt: []string{"child"}, PermissionMode: permission.ModeBypass}, "touch "+marker)
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("control: the child never ran Bash, so the test proves nothing")
	}
	if got := harness.StricterMode(permission.ModeBypass, ""); got != permission.ModeDefault {
		t.Fatalf("empty mode must count as default, got %q", got)
	}
}

// The child's usage is reported to the parent, apart from the parent's own.
func TestSubagentUsageRollsUp(t *testing.T) {
	_, term := runParent(t, harness.QueryInput{PermissionMode: permission.ModeBypass}, Definition{Prompt: []string{"child"}}, "echo hi")
	if term == nil {
		t.Fatal("no terminal")
	}
	// Child: one tool turn (50 in / 3 out) + one text turn (100 in / 7 out).
	if term.SubagentUsage.InputTokens != 150 || term.SubagentUsage.OutputTokens != 10 {
		t.Fatalf("subagent usage %+v", term.SubagentUsage)
	}
	if term.Usage.InputTokens != 150 || term.Usage.OutputTokens != 10 {
		t.Fatalf("parent's own usage must not include the child's: %+v", term.Usage)
	}
}
