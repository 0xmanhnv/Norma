package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func namedTool(name string) CoreTool {
	return Build(Spec{Name: name, Description: name, Schema: map[string]any{"type": "object"},
		Run: func(context.Context, json.RawMessage, *ToolContext) (Result, error) { return Text(name), nil }})
}

// A call to a missing tool names the one it most likely meant, and never a
// guess between two.
func TestUnknownToolText(t *testing.T) {
	reg := NewRegistry(namedTool("Bash"), namedTool("list_facts"), namedTool("list_findings"), namedTool("WebFetch"), namedTool("Read"), namedTool("get_item1"), namedTool("get_item2"))
	for name, want := range map[string]string{
		"bash":         `Did you mean "Bash"?`,
		"list-facts":   `Did you mean "list_facts"?`,
		"list_fact":    `Did you mean "list_facts"?`,
		"web_fetch":    `Did you mean "WebFetch"?`,
		"web_search":   "not available to this agent",
		"list_finding": `Did you mean "list_findings"?`,
		"rea":          "not available to this agent", // too short to guess
		"get_item3":    "not available to this agent", // as near to two tools
	} {
		got := UnknownToolText(reg, name)
		if !strings.HasPrefix(got, `Error: unknown tool "`+name+`"`) || !strings.Contains(got, want) {
			t.Errorf("UnknownToolText(%q) = %q, want %q", name, got, want)
		}
	}
}

// ExecuteExtraTool called for a tool that is loaded, not deferred, says to
// call it directly; a deferred tool still needs unlocking, and a gated one
// still says so.
func TestExecuteExtraToolDirectTool(t *testing.T) {
	reg := NewRegistry(echoTool(), namedTool("add_note"), namedTool("gated"))
	exec := NewExecuteExtraToolFor(reg, NewUnlockSet("echo"), []string{"echo", "gated"})
	reg.Add(exec)
	reg.Add(NewSearchExtraTools(reg, []string{"echo", "gated"}))
	call := func(in string) Result { return delegateAndRun(t, exec, in) }
	if r := call(`{"tool_name":"add_note","params":{}}`); !r.IsError || !strings.Contains(r.Content[0].Text, "call add_note directly") {
		t.Fatalf("direct tool: %+v", r)
	}
	if r := call(`{"tool_name":"SearchExtraTools","params":{"query":"x"}}`); !r.IsError || !strings.Contains(r.Content[0].Text, "call SearchExtraTools directly") {
		t.Fatalf("access tool: %+v", r)
	}
	if r := call(`{"tool_name":"gated","params":{}}`); !r.IsError || !strings.Contains(r.Content[0].Text, "not unlocked") {
		t.Fatalf("gated deferred tool: %+v", r)
	}
	if r := call(`{"tool_name":"echo","params":{"msg":"hi"}}`); r.IsError || r.Content[0].Text != "echo:hi" {
		t.Fatalf("unlocked deferred tool: %+v", r)
	}
	if r := call(`{"tool_name":"echoo","params":{}}`); !r.IsError || !strings.Contains(r.Content[0].Text, `Did you mean "echo"?`) {
		t.Fatalf("typo: %+v", r)
	}
}
