package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// echoTool is a trivial deferred tool that echoes its "msg" param.
func echoTool() CoreTool {
	return Build(Spec{
		Name:        "echo",
		Description: "Echoes the msg parameter back.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"msg": map[string]any{"type": "string"}},
			"required":   []any{"msg"},
		},
		Run: func(_ context.Context, in json.RawMessage, _ *ToolContext) (Result, error) {
			var a struct {
				Msg string `json:"msg"`
			}
			_ = json.Unmarshal(in, &a)
			return Text("echo:" + a.Msg), nil
		},
	})
}

func TestUnlockSet(t *testing.T) {
	s := NewUnlockSet("a", "b")
	if !s.Has("a") || !s.Has("b") || s.Has("c") {
		t.Fatalf("seed wrong: %v", s.List())
	}
	s.Add("c")
	if !s.Has("c") {
		t.Fatal("Add failed")
	}
	if got := s.List(); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("List not sorted/complete: %v", got)
	}
}

func TestRenderDeferredToolsBlock(t *testing.T) {
	if RenderDeferredToolsBlock(nil) != "" {
		t.Fatal("empty names should render empty")
	}
	out := RenderDeferredToolsBlock([]string{"mcp__b__y", "mcp__a__x"})
	if !strings.Contains(out, "<available-deferred-tools>") ||
		!strings.Contains(out, "mcp__a__x") || !strings.Contains(out, "mcp__b__y") ||
		!strings.Contains(out, ExecuteExtraToolName) {
		t.Fatalf("block missing parts:\n%s", out)
	}
	// sorted: a before b
	if strings.Index(out, "mcp__a__x") > strings.Index(out, "mcp__b__y") {
		t.Fatal("names not sorted")
	}
}

func TestSearchExtraTools(t *testing.T) {
	reg := NewRegistry(echoTool())
	search := NewSearchExtraTools(reg, []string{"echo"})

	// exact select
	r, _ := search.Call(context.Background(), json.RawMessage(`{"query":"select:echo"}`), nil)
	txt := r.Content[0].Text
	if !strings.Contains(txt, "## echo") || !strings.Contains(txt, "params schema") {
		t.Fatalf("select:echo missing schema:\n%s", txt)
	}
	// keyword
	r, _ = search.Call(context.Background(), json.RawMessage(`{"query":"echoes"}`), nil)
	if !strings.Contains(r.Content[0].Text, "## echo") {
		t.Fatalf("keyword search failed:\n%s", r.Content[0].Text)
	}
	// non-deferred name → not found (registry has it but it is not in deferred list)
	reg2 := NewRegistry(echoTool())
	search2 := NewSearchExtraTools(reg2, []string{"other"})
	r, _ = search2.Call(context.Background(), json.RawMessage(`{"query":"select:echo"}`), nil)
	if !strings.Contains(r.Content[0].Text, "No matching") {
		t.Fatalf("non-deferred should not surface:\n%s", r.Content[0].Text)
	}
}

// delegateAndRun calls ExecuteExtraTool with a ToolContext whose RunDeferred
// does what the agent loop does, minus the permission check and hooks:
// validate the deferred tool's input and run it.
func delegateAndRun(t *testing.T, exec CoreTool, in string) Result {
	t.Helper()
	tc := &ToolContext{RunDeferred: func(ctx context.Context, inner CoreTool, params json.RawMessage) (Result, error) {
		if err := ValidateInput(inner.InputSchema(), params); err != nil {
			return Errorf("Error: invalid tool input: " + err.Error()), nil
		}
		return inner.Call(ctx, params, nil)
	}}
	r, err := exec.Call(context.Background(), json.RawMessage(in), tc)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestExecuteExtraTool(t *testing.T) {
	reg := NewRegistry(echoTool())
	unlock := NewUnlockSet("echo")
	exec := NewExecuteExtraTool(reg, unlock)

	// unlocked → resolves to echo and runs
	r := delegateAndRun(t, exec, `{"tool_name":"echo","params":{"msg":"hi"}}`)
	if r.IsError || r.Content[0].Text != "echo:hi" {
		t.Fatalf("expected echo:hi, got %+v", r)
	}

	// locked → rejected
	locked := NewExecuteExtraTool(reg, NewUnlockSet())
	r = delegateAndRun(t, locked, `{"tool_name":"echo","params":{"msg":"hi"}}`)
	if !r.IsError || !strings.Contains(r.Content[0].Text, "not unlocked") {
		t.Fatalf("locked tool should be rejected, got %+v", r)
	}

	// unknown tool
	r = delegateAndRun(t, exec, `{"tool_name":"nope","params":{}}`)
	if !r.IsError || !strings.Contains(r.Content[0].Text, "unknown tool") || r.ErrorCode != CodeUnknownTool {
		t.Fatalf("unknown tool should error, got %+v", r)
	}

	// invalid params (missing required msg) → schema validation error
	r = delegateAndRun(t, exec, `{"tool_name":"echo","params":{}}`)
	if !r.IsError || !strings.Contains(r.Content[0].Text, "invalid tool input") {
		t.Fatalf("invalid params should error, got %+v", r)
	}
}

// Called with no agent loop behind it (no ToolContext.RunDeferred),
// ExecuteExtraTool refuses: there must be no path that runs a deferred tool
// with no permission check.
func TestExecuteExtraToolCallFailsClosed(t *testing.T) {
	ran := false
	reg := NewRegistry(Build(Spec{
		Name:   "sideeffect",
		Schema: map[string]any{"type": "object"},
		Run: func(context.Context, json.RawMessage, *ToolContext) (Result, error) {
			ran = true
			return Text("ran"), nil
		},
	}))
	exec := NewExecuteExtraTool(reg, nil)
	for _, tc := range []*ToolContext{nil, {}} {
		r, err := exec.Call(context.Background(), json.RawMessage(`{"tool_name":"sideeffect","params":{}}`), tc)
		if err != nil || !r.IsError || ran || r.ErrorCode != CodePermissionDenied {
			t.Fatalf("Call without the loop must refuse without running the tool: ran=%v r=%+v err=%v", ran, r, err)
		}
	}
}
