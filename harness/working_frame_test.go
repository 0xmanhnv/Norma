package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/0xmanhnv/norma/llm"
	"github.com/0xmanhnv/norma/permission"
	"github.com/0xmanhnv/norma/tool"
)

// The working frame is rebuilt for every model call, rides at the end of that
// request only, and never enters the conversation history.
func TestWorkingFrameIsFreshPerCallAndNeverStored(t *testing.T) {
	m := &scriptedModel{turns: [][]llm.StreamEvent{
		toolTurn("a", "Bash", `{"command":"echo 1"}`),
		toolTurn("b", "Bash", `{"command":"echo 2"}`),
		textTurn("done"),
	}}
	var seen []string
	record := func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		last := req.Messages[len(req.Messages)-1]
		b := last.Content[len(last.Content)-1]
		seen = append(seen, b.Text)
		if req.VolatileTail != 1 {
			t.Errorf("the frame is volatile: VolatileTail = %d, want 1", req.VolatileTail)
		}
		if last.Role != llm.RoleUser {
			t.Errorf("frame must ride on a user message, got %s", last.Role)
		}
		return m.call(ctx, req)
	}
	calls := 0
	in := QueryInput{
		Messages: []llm.Message{llm.UserText("go")}, Tools: tool.NewRegistry(tool.NewBash()),
		PermissionMode: permission.ModeBypass, WorkingDir: t.TempDir(),
		WorkingFrame: func(context.Context) string {
			calls++
			return fmt.Sprintf("<frame>tried=%d</frame>", calls)
		},
	}
	term := drain(t, in, QueryDeps{CallModel: record})
	want := []string{"<frame>tried=1</frame>", "<frame>tried=2</frame>", "<frame>tried=3</frame>"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("frames seen by the model = %v, want %v", seen, want)
	}
	for _, msg := range term.Messages {
		b, _ := json.Marshal(msg)
		if strings.Contains(string(b), "<frame>") {
			t.Fatalf("the frame leaked into history: %s", b)
		}
	}
}

func TestWithWorkingFrameDoesNotTouchItsInput(t *testing.T) {
	orig := []llm.Message{llm.UserText("go")}
	out := withWorkingFrame(orig, "F")
	if len(orig[0].Content) != 1 || len(out[0].Content) != 2 || out[0].Content[1].Text != "F" {
		t.Fatalf("orig=%+v out=%+v", orig, out)
	}
	asst := []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("x")}}}
	if got := withWorkingFrame(asst, "F"); len(got) != 2 || got[1].Role != llm.RoleUser {
		t.Fatalf("no trailing user message: %+v", got)
	}
	if got := withWorkingFrame(orig, ""); len(got[0].Content) != 1 {
		t.Fatalf("an empty frame must add nothing: %+v", got)
	}
}
