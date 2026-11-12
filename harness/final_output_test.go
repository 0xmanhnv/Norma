package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/tool"
)

// todoSchema is the final answer of a to-do assistant: a title, a done flag
// and an optional priority.
func todoSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []any{"title", "done"},
		"properties": map[string]any{
			"title":    map[string]any{"type": "string"},
			"done":     map[string]any{"type": "boolean"},
			"priority": map[string]any{"enum": []any{"low", "high"}},
			"tags":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}
}

func finalOutputRun(t *testing.T, fo *FinalOutput, turns ...[]llm.StreamEvent) (*Terminal, *scriptedModel) {
	t.Helper()
	m := &scriptedModel{turns: turns}
	in := QueryInput{Messages: []llm.Message{llm.UserText("add a to-do")}, FinalOutput: fo}
	return drain(t, in, QueryDeps{CallModel: m.call}), m
}

// userTexts are the texts of the conversation's user messages, in order.
func userTexts(msgs []llm.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role == llm.RoleUser {
			out = append(out, m.Text())
		}
	}
	return out
}

func TestFinalOutputValidFirstTry(t *testing.T) {
	const doc = `{"title":"buy milk","done":false}`
	term, m := finalOutputRun(t, &FinalOutput{Schema: todoSchema()}, textTurn(doc))
	if term.Reason != ReasonCompleted || term.Err != nil {
		t.Fatalf("terminal = %s, %v", term.Reason, term.Err)
	}
	if string(term.Output) != doc || term.Text != doc {
		t.Fatalf("Output = %s, Text = %s", term.Output, term.Text)
	}
	if m.calls != 1 {
		t.Fatalf("model calls = %d, want 1", m.calls)
	}
}

// A fenced document is accepted, and Output holds it without the fence.
func TestFinalOutputAcceptsOneFence(t *testing.T) {
	text := "```json\n{\"title\":\"buy milk\",\"done\":true}\n```"
	term, _ := finalOutputRun(t, &FinalOutput{Schema: todoSchema()}, textTurn(text))
	if term.Reason != ReasonCompleted || string(term.Output) != `{"title":"buy milk","done":true}` {
		t.Fatalf("terminal = %s, Output = %s", term.Reason, term.Output)
	}
	if term.Text != text {
		t.Fatalf("Text = %q, want the answer as written", term.Text)
	}
}

// An invalid answer is sent back once with where it is wrong; the corrected
// answer completes the run. The repair turn names the property and the rule,
// never what the model wrote.
func TestFinalOutputValidAfterOneRepair(t *testing.T) {
	term, m := finalOutputRun(t, &FinalOutput{Schema: todoSchema()},
		textTurn(`{"title":"call the plumber","done":"not yet"}`),
		textTurn(`{"title":"call the plumber","done":false}`),
	)
	if term.Reason != ReasonCompleted || term.Err != nil {
		t.Fatalf("terminal = %s, %v", term.Reason, term.Err)
	}
	if string(term.Output) != `{"title":"call the plumber","done":false}` {
		t.Fatalf("Output = %s", term.Output)
	}
	if m.calls != 2 {
		t.Fatalf("model calls = %d, want 2", m.calls)
	}
	users := userTexts(term.Messages)
	if len(users) != 2 {
		t.Fatalf("user turns = %q, want the prompt and one repair", users)
	}
	repair := users[1]
	if !strings.Contains(repair, "done: must be of type boolean") {
		t.Fatalf("repair turn = %q, want the location and the rule", repair)
	}
	for _, leak := range []string{"plumber", "not yet"} {
		if strings.Contains(repair, leak) {
			t.Fatalf("repair turn repeats the answer (%q): %q", leak, repair)
		}
	}
}

func TestFinalOutputInvalidTwiceEnds(t *testing.T) {
	term, m := finalOutputRun(t, &FinalOutput{Schema: todoSchema()},
		textTurn(`{"title":"water the plants"}`),
		textTurn(`{"title":"water the plants","done":true,"priority":"urgent"}`),
		textTurn(`{"title":"water the plants","done":true}`), // never asked for
	)
	if term.Reason != ReasonInvalidOutput {
		t.Fatalf("reason = %s, want %s", term.Reason, ReasonInvalidOutput)
	}
	if m.calls != 2 {
		t.Fatalf("model calls = %d, want 2 (one answer, one repair)", m.calls)
	}
	if !llm.IsCode(term.Err, tool.CodeInvalidOutput) {
		t.Fatalf("Err code = %q, want %q", llm.CodeOf(term.Err), tool.CodeInvalidOutput)
	}
	var oe *OutputError
	if !errors.As(term.Err, &oe) {
		t.Fatalf("Err %v is not an *OutputError", term.Err)
	}
	if oe.Repairs != 1 || oe.Problem != `priority: must be one of "low", "high"` {
		t.Fatalf("OutputError = %+v", oe)
	}
	if strings.Contains(term.Err.Error(), "urgent") {
		t.Fatalf("error repeats the answer: %v", term.Err)
	}
	if term.Output != nil {
		t.Fatalf("Output = %s on an invalid answer", term.Output)
	}
	if users := userTexts(term.Messages); len(users) != 2 || !strings.Contains(users[1], `missing required property "done"`) {
		t.Fatalf("user turns = %q", users)
	}
}

func TestFinalOutputRepairBudget(t *testing.T) {
	bad := textTurn(`not json at all`)
	good := textTurn(`{"title":"t","done":true}`)

	// Negative: no repair, the first invalid answer ends the run.
	term, m := finalOutputRun(t, &FinalOutput{Schema: todoSchema(), MaxRepairs: -1}, bad, good)
	if term.Reason != ReasonInvalidOutput || m.calls != 1 {
		t.Fatalf("MaxRepairs -1: reason = %s after %d calls", term.Reason, m.calls)
	}
	var oe *OutputError
	if !errors.As(term.Err, &oe) || oe.Repairs != 0 || !strings.HasPrefix(oe.Problem, "the answer is not valid JSON (syntax error at byte") {
		t.Fatalf("MaxRepairs -1: Err = %v", term.Err)
	}

	// Two repairs: a third answer is asked for, and accepted.
	term, m = finalOutputRun(t, &FinalOutput{Schema: todoSchema(), MaxRepairs: 2}, bad, bad, good)
	if term.Reason != ReasonCompleted || m.calls != 3 {
		t.Fatalf("MaxRepairs 2: reason = %s after %d calls", term.Reason, m.calls)
	}
}

// Without a FinalOutput any final text completes the run, as before.
func TestFinalOutputOffByDefault(t *testing.T) {
	term, m := finalOutputRun(t, nil, textTurn("not json at all"))
	if term.Reason != ReasonCompleted || term.Output != nil || m.calls != 1 {
		t.Fatalf("terminal = %s, Output = %s, calls = %d", term.Reason, term.Output, m.calls)
	}
}

// Tool turns before the answer do not use up repairs, and every attempt is
// streamed to the host as text.
func TestFinalOutputAfterToolTurnStreamsEveryAttempt(t *testing.T) {
	m := &scriptedModel{turns: [][]llm.StreamEvent{
		toolTurn("c1", "lookup", `{}`),
		textTurn(`{"title":7,"done":true}`),
		textTurn(`{"title":"seven","done":true}`),
	}}
	lookup := tool.Build(tool.Spec{
		Name: "lookup", Schema: map[string]any{"type": "object"},
		ReadOnly: func(json.RawMessage) bool { return true },
		Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			return tool.Text("nothing"), nil
		},
	})
	in := QueryInput{
		Messages:    []llm.Message{llm.UserText("add a to-do")},
		Tools:       tool.NewRegistry(lookup),
		Allowed:     []string{"lookup"},
		FinalOutput: &FinalOutput{Schema: todoSchema()},
	}
	var texts []string
	var term *Terminal
	for ev, err := range Query(context.Background(), in, QueryDeps{CallModel: m.call}) {
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Kind {
		case KindText:
			texts = append(texts, ev.Text)
		case KindResult:
			term = ev.Terminal
		}
	}
	if term == nil || term.Reason != ReasonCompleted || string(term.Output) != `{"title":"seven","done":true}` {
		t.Fatalf("terminal = %+v", term)
	}
	if len(texts) != 2 {
		t.Fatalf("text events = %q, want both attempts", texts)
	}
}

func TestCheckFinalOutputProblems(t *testing.T) {
	cases := []struct{ text, problem string }{
		{"", "the answer is empty"},
		{"  \n ", "the answer is empty"},
		{"```json\n```", "the answer is empty"},
		{`{"title":"a","done":true} trailing words`, "the answer is not valid JSON (syntax error at byte 27)"},
		{`["a"]`, "the document: must be of type object"},
		{`{"done":true}`, `the document: missing required property "title"`},
		{`{"title":"a","done":true,"tags":["x",2]}`, "tags[1]: must be of type string"},
		{`{"title":"a","done":true,"extra":"kept"}`, ""},
		{"```\n{\"title\":\"a\",\"done\":true}\n```", ""},
		{"```{\"title\":\"a\",\"done\":true}```", ""},
		{"Here it is:\n```json\n{\"title\":\"a\",\"done\":true}\n```", "the answer is not valid JSON (syntax error at byte 1)"},
	}
	for _, c := range cases {
		_, got := checkFinalOutput(todoSchema(), c.text)
		if got != c.problem {
			t.Errorf("checkFinalOutput(%q) = %q, want %q", c.text, got, c.problem)
		}
	}
}
