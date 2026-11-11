package harness

import (
	"context"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// StopCheck ends a run before a model call with the host's own reason. The
// case it exists for: what must be kept no longer fits the model's window, so
// compacting again cannot help. The run ends with ReasonContextExhausted, the
// host's explanation and an error carrying llm.CodeContextOverflow, instead of
// going round until a provider rejects the request.
func TestStopCheckEndsTheRunWithATypedReason(t *testing.T) {
	m := &scriptedModel{turns: [][]llm.StreamEvent{
		toolTurn("a", "sleep", `{"seconds":0}`),
		toolTurn("b", "sleep", `{"seconds":0}`),
		textTurn("never reached"),
	}}
	var seen []RunState
	in := QueryInput{
		Messages:       []llm.Message{llm.UserText("go")},
		Tools:          tool.NewRegistry(tool.NewSleep()),
		PermissionMode: permission.ModeBypass,
		WorkingDir:     t.TempDir(),
		StopCheck: func(_ context.Context, st RunState) *StopRequest {
			seen = append(seen, st)
			if st.Turn < 3 {
				return nil
			}
			return &StopRequest{Reason: ReasonContextExhausted, Detail: "prefix 90k + kept tail 120k exceed the 200k window"}
		},
	}
	term := drain(t, in, QueryDeps{CallModel: m.call})
	if term.Reason != ReasonContextExhausted {
		t.Fatalf("reason = %v, want %v", term.Reason, ReasonContextExhausted)
	}
	if term.Detail != "prefix 90k + kept tail 120k exceed the 200k window" {
		t.Fatalf("detail = %q", term.Detail)
	}
	if !llm.IsCode(term.Err, llm.CodeContextOverflow) {
		t.Fatalf("err = %v, want code %s", term.Err, llm.CodeContextOverflow)
	}
	if m.calls != 2 {
		t.Fatalf("model calls = %d, want 2: the third was stopped before it was made", m.calls)
	}
	if len(seen) != 3 || seen[0].Turn != 1 || len(seen[0].Messages) != 1 || len(seen[2].Messages) != 5 {
		t.Fatalf("StopCheck saw %d states: %+v", len(seen), seen)
	}
	// The conversation is whole: both tool exchanges are in it.
	if len(term.Messages) != 5 {
		t.Fatalf("terminal carries %d messages, want 5", len(term.Messages))
	}
}

// A StopRequest with no reason ends the run as ReasonStopRequested, with no
// error of the loop's making.
func TestStopCheckDefaultReason(t *testing.T) {
	m := &scriptedModel{turns: [][]llm.StreamEvent{textTurn("never reached")}}
	term := drain(t, QueryInput{
		Messages:  []llm.Message{llm.UserText("go")},
		StopCheck: func(context.Context, RunState) *StopRequest { return &StopRequest{Detail: "spending limit"} },
	}, QueryDeps{CallModel: m.call})
	if term.Reason != ReasonStopRequested || term.Err != nil || term.Detail != "spending limit" || m.calls != 0 {
		t.Fatalf("terminal = %+v after %d model calls", term, m.calls)
	}
}
