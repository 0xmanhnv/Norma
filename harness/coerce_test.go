package harness

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// A mistyped but repairable call reaches the tool repaired, with the repairs
// on its context; a raw-input tool still gets the bytes as sent.
func TestHarnessCoercesBeforeValidating(t *testing.T) {
	var got json.RawMessage
	var notes []string
	ct := tool.Build(tool.Spec{
		Name: "add_items",
		Schema: map[string]any{"type": "object", "properties": map[string]any{
			"limit": map[string]any{"type": "integer"},
			"items": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		}},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Decision{Behavior: permission.Allow}
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
			got, notes = in, tool.InputCoercions(ctx)
			return tool.Result{Content: []llm.ContentBlock{llm.TextBlock("ran")}}, nil
		},
	})
	res := execWith(t, ct, `{"limit":"20","items":"[{\"a\":1}]"}`)
	if res.IsError || string(got) != `{"items":[{"a":1}],"limit":20}` || len(notes) != 2 {
		t.Fatalf("result %+v, tool got %s, notes %v", res, got, notes)
	}
	var raw json.RawMessage
	execWith(t, recordingSpec("Lenient", true, &raw), `{"content":"20"}`)
	if string(raw) != `{"content":"20"}` {
		t.Fatalf("raw-input tool got %s", raw)
	}
}
