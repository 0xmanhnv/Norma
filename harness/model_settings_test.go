package harness

import (
	"context"
	"iter"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
)

// QueryInput.ModelSettings reach every model request of the run, unchanged
// from one turn to the next: they are part of what a prompt cache is keyed on.
func TestModelSettingsReachEveryRequest(t *testing.T) {
	want := llm.ModelSettings{CacheTTL: "1h", ConversationCacheTTL: "1h", Thinking: "adaptive", ReasoningEffort: "medium"}
	var reqs []llm.CompletionRequest
	turn := 0
	call := func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		reqs = append(reqs, req)
		turn++
		n := turn
		return func(yield func(llm.StreamEvent, error) bool) {
			if n < 3 {
				yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "working"}, nil)
				yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "max_tokens"}, nil)
				return
			}
			yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "done"}, nil)
			yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
		}
	}
	in := QueryInput{Messages: []llm.Message{llm.UserText("go")}, ModelSettings: want}
	for _, err := range Query(context.Background(), in, QueryDeps{CallModel: call}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(reqs) < 2 {
		t.Fatalf("want several requests, got %d", len(reqs))
	}
	for i, r := range reqs {
		if got := llm.SettingsOf(r); got != want {
			t.Fatalf("request %d settings %+v, want %+v", i, got, want)
		}
	}
	// The zero settings leave a request as the loop built it.
	reqs, turn = nil, 5
	for _, err := range Query(context.Background(), QueryInput{Messages: []llm.Message{llm.UserText("go")}}, QueryDeps{CallModel: call}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := llm.SettingsOf(reqs[0]); got != (llm.ModelSettings{}) {
		t.Fatalf("zero settings changed the request: %+v", got)
	}
}
