package agentcore

import (
	"context"
	"iter"
	"os"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/harness"
	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
	"github.com/0xmanhnv/corvid/transcript"
)

func TestInterruptedResults(t *testing.T) {
	user := llm.UserText("q")
	call := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.TextBlock("on it"),
		{Type: llm.BlockToolUse, ID: "a", Name: "Bash"}, {Type: llm.BlockToolUse, ID: "b", Name: "Write"}}}
	res := func(ids ...string) llm.Message {
		m := llm.Message{Role: llm.RoleUser}
		for _, id := range ids {
			m.Content = append(m.Content, llm.ToolResultText(id, "ok", false))
		}
		return m
	}
	answer := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}
	cases := []struct {
		name string
		in   []llm.Message
		want []string // ids answered by the synthetic message; nil = none added
	}{
		{"calls with no results", []llm.Message{user, call}, []string{"a", "b"}},
		{"partial results", []llm.Message{user, call, res("a")}, []string{"b"}},
		{"results split over two messages", []llm.Message{user, call, res("a"), res("b")}, nil},
		{"complete exchange", []llm.Message{user, call, res("a", "b")}, nil},
		{"complete exchange, then an answer", []llm.Message{user, call, res("a", "b"), answer}, nil},
		{"plain conversation", []llm.Message{user}, nil},
		{"empty", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, ok := interruptedResults(c.in)
			if ok != (c.want != nil) {
				t.Fatalf("ok = %v, want %v (%+v)", ok, c.want != nil, m)
			}
			if !ok {
				return
			}
			if m.Role != llm.RoleUser || len(m.Content) != len(c.want) {
				t.Fatalf("message = %+v, want user results for %v", m, c.want)
			}
			for i, b := range m.Content {
				if b.Type != llm.BlockToolResult || b.ToolUseID != c.want[i] || !b.IsError ||
					b.ToolError == nil || b.ToolError.Code != tool.CodeInterrupted {
					t.Fatalf("block %d = %+v, want an interrupted result for %s", i, b, c.want[i])
				}
				for _, word := range []string{"MAY have executed", "Check"} {
					if !strings.Contains(b.Content[0].Text, word) {
						t.Fatalf("result text %q does not say %q", b.Content[0].Text, word)
					}
				}
			}
			// Paired now: nothing is left open, and the provider-boundary
			// projection keeps every block (it drops unpaired ones).
			all := append(append([]llm.Message{}, c.in...), m)
			if again, open := interruptedResults(all); open {
				t.Fatalf("still unanswered after pairing: %+v", again)
			}
			blocks := func(msgs []llm.Message) (n int) {
				for _, x := range msgs {
					n += len(x.Content)
				}
				return n
			}
			if got, want := blocks(llm.MessagesForAPI(all)), blocks(all); got != want {
				t.Fatalf("the provider projection kept %d of %d blocks", got, want)
			}
		})
	}
}

// A session killed after a tool call was recorded but before its result was
// must not come back with the call erased: the tool may have run, and a model
// that cannot see the call issues it again.
func TestResumeKeepsAnInterruptedToolCall(t *testing.T) {
	store := transcript.NewStore(t.TempDir())
	w := store.NewWriter("sess", "")
	w.RecordMessage(llm.UserText("deploy it"), llm.Usage{})
	w.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockToolUse, ID: "d1", Name: "Bash", Input: []byte(`{"command":"./deploy.sh"}`)},
	}}, llm.Usage{InputTokens: 7, OutputTokens: 3})
	if err := w.Err(); err != nil {
		t.Fatal(err)
	}

	var reqs [][]llm.Message
	callModel := func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		reqs = append(reqs, req.Messages)
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, e := range textTurn("checking first") {
				if !yield(e, nil) {
					return
				}
			}
		}
	}
	opts := Options{SystemPrompt: []string{"go"}, PermissionMode: permission.ModeBypass, Transcript: store,
		Deps: harness.QueryDeps{CallModel: callModel}}
	s := NewSession(opts)
	defer s.Close()
	if err := s.Resume("sess"); err != nil {
		t.Fatal(err)
	}
	msgs := s.Messages()
	if len(msgs) != 3 || len(msgs[1].ToolUses()) != 1 {
		t.Fatalf("resumed conversation = %+v, want the call kept and answered", msgs)
	}
	r := msgs[2].Content[0]
	if r.ToolUseID != "d1" || !r.IsError || !strings.Contains(r.Content[0].Text, "MAY have executed") {
		t.Fatalf("synthetic result = %+v", r)
	}

	drainPrompt(t, s, "carry on")
	sent := reqs[len(reqs)-1]
	if e := pairingErr(sent); e != "" {
		t.Fatalf("request after resume is invalid: %s", e)
	}
	var sawCall bool
	for _, m := range sent {
		for _, u := range m.ToolUses() {
			sawCall = sawCall || u.ID == "d1"
		}
	}
	if !sawCall {
		t.Fatal("the model was not shown the interrupted call")
	}

	// The synthetic result is in the transcript, so a second resume sees the
	// same conversation and adds nothing more.
	again := NewSession(opts)
	defer again.Close()
	if err := again.Resume("sess"); err != nil {
		t.Fatal(err)
	}
	if got := again.Messages(); len(got) != 5 || got[2].Content[0].ToolUseID != "d1" {
		t.Fatalf("second resume = %d messages, want 5 with the synthetic result third: %+v", len(got), got)
	}
}

// A transcript whose last write was cut short still resumes: the torn line is
// skipped and reported, and the session keeps appending to the same file.
func TestResumeSurvivesATornFinalLine(t *testing.T) {
	store := transcript.NewStore(t.TempDir())
	w := store.NewWriter("sess", "")
	w.RecordMessage(llm.UserText("first question"), llm.Usage{})
	w.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("first answer")}}, llm.Usage{})
	f, err := os.OpenFile(store.MainPath("sess"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"uuid":"x","type":"message","message":{"role":"user","content":[{"type":"te`)
	f.Close()

	var warnings []string
	opts := Options{SystemPrompt: []string{"go"}, PermissionMode: permission.ModeBypass, Transcript: store,
		OnWarn: func(m string) { warnings = append(warnings, m) },
		Deps:   harness.QueryDeps{CallModel: usageModel(llm.Usage{InputTokens: 1, OutputTokens: 1})}}
	s := NewSession(opts)
	defer s.Close()
	if err := s.Resume("sess"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(s.Messages()) != 2 {
		t.Fatalf("resumed %d messages, want 2", len(s.Messages()))
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "incomplete final line") {
		t.Fatalf("warnings = %q, want one about the incomplete final line", warnings)
	}
	drainPrompt(t, s, "second question")

	again := NewSession(opts)
	defer again.Close()
	if err := again.Resume("sess"); err != nil {
		t.Fatalf("second resume: %v", err)
	}
	if len(again.Messages()) != 4 {
		t.Fatalf("second resume loaded %d messages, want 4", len(again.Messages()))
	}
}
