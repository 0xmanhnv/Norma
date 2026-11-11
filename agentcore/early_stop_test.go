package agentcore

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/harness"
	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// pairingErr reports a request whose tool calls and results don't pair up, which
// providers reject.
func pairingErr(msgs []llm.Message) string {
	for i, m := range msgs {
		uses := m.ToolUses()
		if m.Role != llm.RoleAssistant || len(uses) == 0 {
			continue
		}
		if i+1 >= len(msgs) {
			return "tool call with no result message"
		}
		got := map[string]bool{}
		for _, b := range msgs[i+1].Content {
			if b.Type == llm.BlockToolResult {
				got[b.ToolUseID] = true
			}
		}
		for _, u := range uses {
			if !got[u.ID] {
				return "tool call " + u.ID + " has no result"
			}
		}
	}
	return ""
}

// A caller that stops reading mid-prompt must not leave the session behind. Here
// a tool call completes (it ran, and a transcript would have it), then the caller
// walks away while the model is answering. The session must keep the finished
// exchange (or the next prompt would let the model run the side effect again),
// drop nothing that pairs up, and send a valid conversation next time.
func TestSessionKeepsUpWhenTheCallerStopsEarly(t *testing.T) {
	turns := [][]llm.StreamEvent{
		toolTurn("b1", "Bash", `{"command":"echo one"}`),
		textTurn("partial answer the caller never reads to the end"),
	}
	var reqs [][]llm.Message
	callModel := func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		reqs = append(reqs, req.Messages)
		evs := textTurn("second answer")
		if len(turns) > 0 {
			evs, turns = turns[0], turns[1:]
		}
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, e := range evs {
				if !yield(e, nil) {
					return
				}
			}
		}
	}
	sess := NewSession(Options{SystemPrompt: []string{"go"}, Tools: []tool.CoreTool{tool.NewBash()},
		PermissionMode: permission.ModeBypass, WorkingDir: t.TempDir(), Deps: harness.QueryDeps{CallModel: callModel}})
	defer sess.Close()

	for ev, err := range sess.Prompt(context.Background(), "first question") {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == harness.KindText {
			break // the caller walks away while the second model call streams
		}
	}
	msgs := sess.Messages()
	if len(msgs) == 0 || !strings.Contains(msgs[0].Content[0].Text, "first question") {
		t.Fatalf("the prompt the caller sent was lost: %+v", msgs)
	}
	if e := pairingErr(msgs); e != "" {
		t.Fatalf("session kept an unfinished tool exchange: %s", e)
	}
	ran := false
	for _, m := range msgs {
		for _, u := range m.ToolUses() {
			ran = ran || u.ID == "b1"
		}
	}
	if !ran {
		t.Fatal("the session forgot a tool call that already ran")
	}

	for _, err := range sess.Prompt(context.Background(), "second question") {
		if err != nil {
			t.Fatal(err)
		}
	}
	last := reqs[len(reqs)-1]
	var text strings.Builder
	for _, m := range last {
		for _, b := range m.Content {
			text.WriteString(b.Text + "\n")
		}
	}
	if !strings.Contains(text.String(), "first question") || !strings.Contains(text.String(), "second question") {
		t.Fatalf("next request lost history: %s", text.String())
	}
	if e := pairingErr(last); e != "" {
		t.Fatalf("next request is invalid: %s", e)
	}
}
