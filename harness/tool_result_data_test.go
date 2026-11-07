package harness

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

// A tool's host-only Data reaches OnToolResult and never enters the conversation.
func TestToolResultDataReachesHostNotModel(t *testing.T) {
	type record struct {
		Findings int    `json:"findings"`
		RawRef   string `json:"raw_ref"`
	}
	scan := tool.Build(tool.Spec{
		Name: "scan", Description: "scan", Schema: map[string]any{"type": "object", "properties": map[string]any{}},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			r := tool.Text("3 findings (summary)")
			r.Data = record{Findings: 3, RawRef: "sha256:secretrawpointer"}
			return r, nil
		},
	})
	m := &scriptedModel{turns: [][]llm.StreamEvent{toolTurn("c1", "scan", `{}`), textTurn("done")}}
	var mu sync.Mutex
	var got []any
	var ids []string
	in := QueryInput{
		Messages: []llm.Message{llm.UserText("go")}, Tools: tool.NewRegistry(scan),
		PermissionMode: permission.ModeBypass, WorkingDir: t.TempDir(),
		OnToolResult: func(_ context.Context, id, name string, _ json.RawMessage, res tool.Result) {
			mu.Lock()
			defer mu.Unlock()
			got, ids = append(got, res.Data), append(ids, id+":"+name)
		},
	}
	term := drain(t, in, QueryDeps{CallModel: m.call})
	if len(got) != 1 || ids[0] != "c1:scan" {
		t.Fatalf("OnToolResult calls = %v %v", ids, got)
	}
	if rec, ok := got[0].(record); !ok || rec.Findings != 3 {
		t.Fatalf("Data = %#v", got[0])
	}
	for _, msg := range term.Messages {
		b, _ := json.Marshal(msg)
		if strings.Contains(string(b), "secretrawpointer") {
			t.Fatalf("host-only Data leaked into the conversation: %s", b)
		}
	}
	if res := term.Messages[2].Content[0]; !strings.Contains(res.Content[0].Text, "summary") {
		t.Fatalf("the model should still see the summary: %+v", res)
	}
}
