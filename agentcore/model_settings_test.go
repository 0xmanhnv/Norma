package agentcore

import (
	"context"
	"iter"
	"testing"

	"github.com/0xmanhnv/corvid/harness"
	"github.com/0xmanhnv/corvid/llm"
)

// Options.ModelSettings is how a host sets the cache lifetime, thinking mode
// and effort of a session: it must reach the requests the session sends.
func TestOptionsModelSettingsReachRequests(t *testing.T) {
	want := llm.ModelSettings{ConversationCacheTTL: "1h", Thinking: "adaptive", ReasoningEffort: "low"}
	var got []llm.ModelSettings
	s := NewSession(Options{
		SystemPrompt:  []string{"sys"},
		ModelSettings: want,
		Deps: harness.QueryDeps{CallModel: func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
			got = append(got, llm.SettingsOf(req))
			return func(yield func(llm.StreamEvent, error) bool) {
				yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "ok"}, nil)
				yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
			}
		}},
	})
	defer s.Close()
	for range 2 {
		for _, err := range s.Prompt(context.Background(), "hi") {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(got) != 2 || got[0] != want || got[1] != want {
		t.Fatalf("request settings %+v, want %+v on both prompts", got, want)
	}
}
