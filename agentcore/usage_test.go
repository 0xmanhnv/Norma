package agentcore

import (
	"context"
	"iter"
	"testing"

	"github.com/0xmanhnv/corvid/harness"
	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/transcript"
)

// usageModel answers every call with one line of text and the given usage.
func usageModel(u llm.Usage) func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, ev := range []llm.StreamEvent{
				{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: u.InputTokens}},
				{Type: llm.SETextDelta, Text: "ok"},
				{Type: llm.SEMessageDelta, StopReason: "end_turn", Usage: llm.Usage{OutputTokens: u.OutputTokens}},
				{Type: llm.SEMessageStop},
			} {
				if !yield(ev, nil) {
					return
				}
			}
		}
	}
}

// Session.Usage is the running total for the session — across prompts, and
// across a resume — while LastUsage is the most recent prompt alone.
func TestSessionUsageIsCumulative(t *testing.T) {
	per := llm.Usage{InputTokens: 10, OutputTokens: 5}
	times := func(n int) llm.Usage { return llm.Usage{InputTokens: 10 * n, OutputTokens: 5 * n} }
	cases := []struct {
		name      string
		before    int // prompts in a first session
		resume    bool
		reset     bool
		after     int // prompts after the resume or reset
		wantTotal llm.Usage
		wantLast  llm.Usage
	}{
		{"one prompt", 1, false, false, 0, times(1), per},
		{"three prompts", 3, false, false, 0, times(3), per},
		{"resume alone restores the total", 2, true, false, 0, times(2), llm.Usage{}},
		{"prompts after a resume add to it", 2, true, false, 2, times(4), per},
		{"reset starts over", 2, false, true, 1, times(1), per},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := transcript.NewStore(t.TempDir())
			opts := Options{SystemPrompt: []string{"test"}, PermissionMode: permission.ModeBypass,
				Transcript: store, Deps: harness.QueryDeps{CallModel: usageModel(per)}}
			s := NewSession(opts)
			defer s.Close()
			for i := 0; i < c.before; i++ {
				drainPrompt(t, s, "q")
			}
			if c.resume {
				resumed := NewSession(opts)
				defer resumed.Close()
				if err := resumed.Resume(s.SessionID()); err != nil {
					t.Fatal(err)
				}
				s = resumed
			}
			if c.reset {
				s.Reset()
			}
			for i := 0; i < c.after; i++ {
				drainPrompt(t, s, "q")
			}
			if got := s.Usage(); got != c.wantTotal {
				t.Errorf("Usage() = %+v, want %+v", got, c.wantTotal)
			}
			if got := s.LastUsage(); got != c.wantLast {
				t.Errorf("LastUsage() = %+v, want %+v", got, c.wantLast)
			}
		})
	}
}
