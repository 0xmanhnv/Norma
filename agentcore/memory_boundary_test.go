package agentcore

import (
	"context"
	"iter"
	"testing"

	"github.com/0xmanhnv/corvid/harness"
	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/memory"
)

// The memory index changes whenever a memory is saved, so it must not sit in
// the cached part of the system prompt: with no boundary of the host's, the
// boundary goes right before the index and the segments ahead of it stay
// cached. A boundary the host set is left as it is.
func TestMemoryIndexSitsAfterTheCacheBoundary(t *testing.T) {
	cases := []struct {
		name     string
		system   []string
		appendS  []string
		boundary int
		want     int
	}{
		{"no boundary: before the index", []string{"a", "b"}, []string{"c"}, 0, 3},
		{"host boundary kept", []string{"a", "b"}, nil, 1, 1},
		{"index alone: nothing to split", nil, nil, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []llm.CompletionRequest
			s := NewSession(Options{
				SystemPrompt:       c.system,
				AppendSystemPrompt: c.appendS,
				DynamicBoundary:    c.boundary,
				Memory:             &MemoryOptions{Store: memory.NewStore(t.TempDir()), IncludeIndexInPrompt: true},
				Deps: harness.QueryDeps{CallModel: func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
					got = append(got, req)
					return func(yield func(llm.StreamEvent, error) bool) {
						yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "ok"}, nil)
						yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
					}
				}},
			})
			defer s.Close()
			for _, err := range s.Prompt(context.Background(), "hi") {
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(got) != 1 {
				t.Fatalf("%d requests, want 1", len(got))
			}
			req := got[0]
			if req.DynamicBoundary != c.want {
				t.Fatalf("DynamicBoundary = %d, want %d (system %q)", req.DynamicBoundary, c.want, req.System)
			}
			if n := len(req.System); n == 0 || req.System[n-1] != memory.NewStore(t.TempDir()).SystemSegment() {
				t.Fatalf("the memory index is not the last system segment: %q", req.System)
			}
		})
	}
}
