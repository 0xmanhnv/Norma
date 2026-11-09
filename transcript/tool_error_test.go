package transcript

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/0xmanhnv/norma/llm"
)

// A failed call's classification is part of the durable conversation: it is
// written, read back and reconstructed with the message, so code that resumes
// a session can still branch on it.
func TestToolErrorRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())
	w := s.NewWriter("sess", "")
	w.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockToolUse, ID: "c1", Name: "Write", Input: json.RawMessage(`{}`)},
	}}, llm.Usage{})
	res := llm.ToolResultText("c1", "Tool call was not permitted: denied", true)
	res.ToolError = &llm.ToolError{Name: "Write", Code: "permission_denied"}
	w.RecordMessage(llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{res}}, llm.Usage{})
	if err := w.Err(); err != nil {
		t.Fatal(err)
	}

	recs, err := s.Load(s.MainPath("sess"))
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := Messages(recs)
	got := msgs[1].Content[0]
	if got.ToolError == nil || *got.ToolError != (llm.ToolError{Name: "Write", Code: "permission_denied"}) {
		t.Fatalf("tool error after reload = %+v", got.ToolError)
	}
	if got.Content[0].Text != "Tool call was not permitted: denied" || !got.IsError {
		t.Fatalf("result after reload = %+v", got)
	}
}

// ErrNotLive adopted the coded base error without changing how callers match it.
func TestErrNotLiveIsCoded(t *testing.T) {
	s := NewStore(t.TempDir())
	s.NewWriter("sess", "").RecordMessage(llm.UserText("hi"), llm.Usage{})
	err := s.Rewind("sess", "no-such-uuid")
	if !errors.Is(err, ErrNotLive) {
		t.Fatalf("Rewind = %v, want ErrNotLive", err)
	}
	if !llm.IsCode(err, llm.CodeInvalidArgument) {
		t.Fatalf("code = %q", llm.CodeOf(err))
	}
}
