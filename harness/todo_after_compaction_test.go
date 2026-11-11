package harness

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/tool"
)

// boundaryOnce is a Compactor that summarizes everything behind a boundary on
// its second call, the way the built-in one does.
type boundaryOnce struct{ calls int }

func (c *boundaryOnce) Pre(_ context.Context, msgs []llm.Message, _ int) []llm.Message {
	c.calls++
	if c.calls != 2 {
		return msgs
	}
	out := append([]llm.Message{}, msgs...)
	return append(out, llm.BoundaryMessage(llm.BoundaryMeta{Trigger: "auto"}), llm.UserText("Summary: earlier work"))
}
func (c *boundaryOnce) Reactive(_ context.Context, msgs []llm.Message) ([]llm.Message, bool) {
	return msgs, false
}
func (c *boundaryOnce) IsOverflow(error) bool { return false }

// A compaction summarizes the turns in which the todo list was written. The
// list is put back in front of the model on the very next request, instead of
// after the ten quiet turns the reminder throttle waits for.
func TestTodoListIsRemindedRightAfterACompaction(t *testing.T) {
	store := tool.NewTodoStore()
	todo := store.Tool()
	_, err := todo.Call(context.Background(), []byte(`{"todos":[{"content":"write the report","status":"in_progress"}]}`), &tool.ToolContext{})
	if err != nil {
		t.Fatal(err)
	}
	m := &scriptedModel{turns: [][]llm.StreamEvent{
		toolTurn("a", "sleep", `{"seconds":0}`),
		textTurn("done"),
	}}
	var requests [][]llm.Message
	call := func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		requests = append(requests, req.Messages)
		return m.call(ctx, req)
	}
	rec := &capRecorder{}
	term := drain(t, QueryInput{
		Messages:  []llm.Message{llm.UserText("go")},
		Tools:     tool.NewRegistry(tool.NewSleep(), todo),
		Todos:     store,
		Compactor: &boundaryOnce{},
		Recorder:  rec,
	}, QueryDeps{CallModel: call})
	if term.Reason != ReasonCompleted || len(requests) != 2 {
		t.Fatalf("terminal %+v after %d requests", term, len(requests))
	}
	for _, msg := range requests[0] {
		if llm.IsTodoReminder(msg) {
			t.Fatal("a reminder was sent before any compaction")
		}
	}
	last := requests[1][len(requests[1])-1]
	if !llm.IsTodoReminder(last) {
		t.Fatalf("the request after the compaction does not end with the todo reminder: %+v", requests[1])
	}
	if got := last.Content[0].Text; !strings.Contains(got, "write the report") {
		t.Fatalf("the reminder does not carry the list: %s", got)
	}
	// It is part of the history and of the transcript, once.
	n := 0
	for _, msg := range rec.msgs {
		if llm.IsTodoReminder(msg) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the reminder was recorded %d times, want once", n)
	}
}
