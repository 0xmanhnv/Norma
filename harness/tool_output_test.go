package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/tool"
)

// record is a declared-output value with a field the renderer never shows.
type record struct {
	Visible string `json:"visible"`
	Hidden  string `json:"hidden"`
}

var recordOutput = tool.DeclareOutput(tool.OutputSpec[record]{
	Schema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"visible": map[string]any{"type": "string"}, "hidden": map[string]any{"type": "string"}},
		"required":   []any{"visible"},
	},
	Render: func(_ json.RawMessage, r record) ([]llm.ContentBlock, error) {
		if r.Visible == "FAIL" {
			return nil, fmt.Errorf("cannot render")
		}
		return []llm.ContentBlock{llm.TextBlock("visible=" + r.Visible)}, nil
	},
})

func recordTool(v any) tool.CoreTool {
	return tool.Build(tool.Spec{
		Name:     "Record",
		Schema:   map[string]any{"type": "object"},
		ReadOnly: func(json.RawMessage) bool { return true },
		Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			return tool.Result{Data: v}, nil
		},
		Output: recordOutput,
	})
}

// seen collects what OnToolResult was handed.
type seen struct {
	mu  sync.Mutex
	res []tool.Result
}

func (s *seen) on(_ context.Context, _, _ string, _ json.RawMessage, res tool.Result) {
	s.mu.Lock()
	s.res = append(s.res, res)
	s.mu.Unlock()
}

// Only the rendered content enters the conversation and the transcript; the
// value stays with the call and reaches the host through OnToolResult.
func TestTranscriptPersistsRenderedContentOnly(t *testing.T) {
	obs := &seen{}
	in := QueryInput{Tools: tool.NewRegistry(recordTool(record{Visible: "v1", Hidden: "VALUE_ONLY"})), OnToolResult: obs.on}
	h, ev, rec := runOneCall(t, in, "Record", `{}`)
	for where, b := range map[string]llm.ContentBlock{"history": h, "event": ev, "recorder": rec} {
		raw, _ := json.Marshal(b)
		if strings.Contains(string(raw), "VALUE_ONLY") || !strings.Contains(string(raw), "visible=v1") {
			t.Fatalf("%s carries %s", where, raw)
		}
	}
	if got := obs.res[0].Data.(record); got.Hidden != "VALUE_ONLY" {
		t.Fatalf("host value = %+v", got)
	}
}

// A value that fails validation or rendering is an ordinary tool error with a
// code; the run goes on.
func TestOutputFailuresAreToolErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		value any
		code  llm.Code
	}{
		"invalid value":  {map[string]any{"visible": 1}, tool.CodeInvalidOutput},
		"render failure": {record{Visible: "FAIL"}, tool.CodeRenderFailed},
	} {
		t.Run(name, func(t *testing.T) {
			h, _, rec := runOneCall(t, QueryInput{Tools: tool.NewRegistry(recordTool(tc.value))}, "Record", `{}`)
			wantToolError(t, "history", h, "Record", tc.code)
			wantToolError(t, "recorder", rec, "Record", tc.code)
		})
	}
}

func TestRewriteToolResult(t *testing.T) {
	value := record{Visible: "v1", Hidden: "h1"}
	run := func(t *testing.T, rw tool.Rewrite, tools ...tool.CoreTool) (llm.ContentBlock, *seen) {
		t.Helper()
		obs := &seen{}
		in := QueryInput{
			Tools:        tool.NewRegistry(tools...),
			OnToolResult: obs.on,
			RewriteToolResult: func(context.Context, string, string, json.RawMessage, tool.Result) tool.Rewrite {
				return rw
			},
		}
		h, _, _ := runOneCall(t, in, tools[0].Name(), `{}`)
		return h, obs
	}

	t.Run("presentation only", func(t *testing.T) {
		h, obs := run(t, tool.ReplaceContent([]llm.ContentBlock{llm.TextBlock("shown instead")}), recordTool(value))
		if h.Content[0].Text != "shown instead" || h.IsError {
			t.Fatalf("model reads %+v", h)
		}
		// The value is untouched: content replacement hides nothing from the host.
		if got := obs.res[0].Data.(record); got != value {
			t.Fatalf("value changed to %+v", got)
		}
	})

	t.Run("value", func(t *testing.T) {
		h, obs := run(t, tool.ReplaceValue(record{Visible: "v2"}), recordTool(value))
		if h.Content[0].Text != "visible=v2" {
			t.Fatalf("model reads %+v", h)
		}
		if got := obs.res[0].Data.(record); got != (record{Visible: "v2"}) {
			t.Fatalf("value = %+v", got)
		}
	})

	// Trap: a value replacement that cannot be honoured must not let the
	// original through.
	t.Run("invalid value fails closed", func(t *testing.T) {
		h, obs := run(t, tool.ReplaceValue("not a record"), recordTool(value))
		wantToolError(t, "history", h, "Record", tool.CodeInvalidOutput)
		if strings.Contains(h.Content[0].Text, "v1") || obs.res[0].Data != nil {
			t.Fatalf("original leaked: %+v / %+v", h, obs.res[0].Data)
		}
	})
	t.Run("no declared output fails closed", func(t *testing.T) {
		plain := fnTool("Plain", func() (tool.Result, error) { return tool.Text("secret text"), nil })
		h, _ := run(t, tool.ReplaceValue(record{}), plain)
		wantToolError(t, "history", h, "Plain", tool.CodeInvalidOutput)
		if strings.Contains(h.Content[0].Text, "secret") {
			t.Fatalf("original leaked: %+v", h)
		}
	})
}

// memStore is a SpillStore keeping items in memory.
type memStore struct {
	mu    sync.Mutex
	items [][]byte
}

func (s *memStore) Store(_ context.Context, item tool.SpillItem) (tool.Spilled, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, item.Data)
	return tool.Spilled{Locator: "mem", Guidance: "Ask the host for item mem"}, nil
}

// A host store set on the run is where the output cap spills to.
func TestSpillStoreReachesTheOutputCap(t *testing.T) {
	store := &memStore{}
	big := strings.Repeat("w", 400)
	in := QueryInput{
		Tools:              tool.NewRegistry(fnTool("Big", func() (tool.Result, error) { return tool.Text(big), nil })),
		SpillStore:         store,
		MaxToolOutputChars: 100,
	}
	h, _, _ := runOneCall(t, in, "Big", `{}`)
	if len(store.items) != 1 || string(store.items[0]) != big {
		t.Fatalf("store got %d items", len(store.items))
	}
	if !strings.Contains(h.Content[0].Text, "Ask the host for item mem") {
		t.Fatalf("model reads %q", h.Content[0].Text)
	}
}
