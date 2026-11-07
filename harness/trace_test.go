package harness

import (
	"context"
	"sync"
	"testing"

	"github.com/0xmanhnv/norma/llm"
	"github.com/0xmanhnv/norma/permission"
	"github.com/0xmanhnv/norma/tool"
)

type span struct {
	name       string
	start, end map[string]any
	err        error
	ended      int
}

type fakeTracer struct {
	mu    sync.Mutex
	spans []*span
}

func (f *fakeTracer) Start(ctx context.Context, name string, attrs map[string]any) (context.Context, func(error, map[string]any)) {
	s := &span{name: name, start: attrs}
	f.mu.Lock()
	f.spans = append(f.spans, s)
	f.mu.Unlock()
	return ctx, func(err error, attrs map[string]any) {
		f.mu.Lock()
		defer f.mu.Unlock()
		s.err, s.end, s.ended = err, attrs, s.ended+1
	}
}

func TestTracerSpansModelAndToolCalls(t *testing.T) {
	turn := toolTurn("c1", "Bash", `{"command":"echo hi"}`)
	turn = append([]llm.StreamEvent{{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 120}}}, turn...)
	m := &scriptedModel{turns: [][]llm.StreamEvent{turn, textTurn("done")}}
	tr := &fakeTracer{}
	in := QueryInput{
		Messages: []llm.Message{llm.UserText("go")}, Tools: tool.NewRegistry(tool.NewBash()),
		PermissionMode: permission.ModeBypass, WorkingDir: t.TempDir(), Tracer: tr,
	}
	drain(t, in, QueryDeps{CallModel: m.call})
	var names []string
	for _, s := range tr.spans {
		names = append(names, s.name)
		if s.ended != 1 {
			t.Errorf("span %s ended %d times, want 1", s.name, s.ended)
		}
	}
	if len(names) != 3 || names[0] != "model.call" || names[1] != "tool.call" || names[2] != "model.call" {
		t.Fatalf("spans = %v, want model.call, tool.call, model.call", names)
	}
	if got := tr.spans[1].start["tool"]; got != "Bash" {
		t.Errorf("tool span attrs = %v", tr.spans[1].start)
	}
	if tr.spans[1].end["is_error"] != false {
		t.Errorf("tool span end = %v", tr.spans[1].end)
	}
	if tr.spans[0].end["input_tokens"] != 120 || tr.spans[0].end["stop_reason"] != "tool_use" {
		t.Errorf("model span end = %v", tr.spans[0].end)
	}
}
