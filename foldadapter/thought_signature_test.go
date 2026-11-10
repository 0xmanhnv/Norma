package foldadapter

import (
	"encoding/json"
	"testing"

	"github.com/0xmanhnv/corvid/fold"
	"github.com/0xmanhnv/corvid/llm"
)

// Compression may rewrite a tool call's arguments. Gemini's thought_signature
// authenticates the functionCall part as the model emitted it, so replaying the
// old signature over new arguments is the same mismatch the BlockThinking case
// refuses to create. Dropping it yields a loud, documented rejection instead of
// a signature that claims to cover arguments it never saw.
func TestRebuildBlockDropsSignatureOnRewrite(t *testing.T) {
	orig := llm.ContentBlock{
		Type:             llm.BlockToolUse,
		ID:               "t1",
		Name:             "Read",
		Input:            json.RawMessage(`{"path":"/very/long/original"}`),
		ThoughtSignature: "sig-abc",
	}

	rewritten := rebuildBlock(orig, fold.CoreMessage{
		Role: fold.RoleAssistant,
		Text: `{"path":"/short"}`,
	}, &Sidecar{}, ReassembleOptions{})

	if string(rewritten.Input) != `{"path":"/short"}` {
		t.Fatalf("input not rewritten: %s", rewritten.Input)
	}
	if rewritten.ThoughtSignature != "" {
		t.Errorf("stale signature replayed over rewritten arguments: %q", rewritten.ThoughtSignature)
	}
}

// An untouched tool call must keep its signature — that is the whole point of
// recording it, and a no-op projection must not break the next Gemini turn.
func TestRebuildBlockKeepsSignatureWhenUnchanged(t *testing.T) {
	args := `{"path":"/a"}`
	orig := llm.ContentBlock{
		Type:             llm.BlockToolUse,
		ID:               "t1",
		Name:             "Read",
		Input:            json.RawMessage(args),
		ThoughtSignature: "sig-abc",
	}

	same := rebuildBlock(orig, fold.CoreMessage{Role: fold.RoleAssistant, Text: args}, &Sidecar{}, ReassembleOptions{})
	if same.ThoughtSignature != "sig-abc" {
		t.Errorf("signature lost on an unchanged call: %q", same.ThoughtSignature)
	}

	// A malformed rewrite is already refused; the signature must survive with
	// the arguments it belongs to.
	bad := rebuildBlock(orig, fold.CoreMessage{Role: fold.RoleAssistant, Text: `{"path":`}, &Sidecar{}, ReassembleOptions{})
	if string(bad.Input) != args {
		t.Fatalf("malformed rewrite was applied: %s", bad.Input)
	}
	if bad.ThoughtSignature != "sig-abc" {
		t.Errorf("signature dropped although arguments were kept: %q", bad.ThoughtSignature)
	}
}
