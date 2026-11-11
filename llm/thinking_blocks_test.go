package llm

import (
	"encoding/json"
	"reflect"
	"testing"
)

// A thinking block is kept with its signature even when it has no text, and a
// redacted_thinking block is kept as it came.
func TestAccumulatorKeepsReasoningBlocks(t *testing.T) {
	cases := []struct {
		name   string
		events []StreamEvent
		want   []ContentBlock
	}{
		{"signature with no text",
			[]StreamEvent{{Type: SEThinkingSignature, Text: "sig"}, {Type: SETextDelta, Text: "hi"}, {Type: SEMessageStop}},
			[]ContentBlock{{Type: BlockThinking, Signature: "sig"}, TextBlock("hi")}},
		{"empty delta, then signature",
			[]StreamEvent{{Type: SEThinkingDelta, Text: ""}, {Type: SEThinkingSignature, Text: "sig"}, {Type: SEMessageStop}},
			[]ContentBlock{{Type: BlockThinking, Signature: "sig"}}},
		{"text, then signature",
			[]StreamEvent{{Type: SEThinkingDelta, Text: "hmm"}, {Type: SEThinkingSignature, Text: "sig"}, {Type: SEMessageStop}},
			[]ContentBlock{{Type: BlockThinking, Thinking: "hmm", Signature: "sig"}}},
		{"empty signed block before a tool call",
			[]StreamEvent{{Type: SEThinkingSignature, Text: "sig"}, {Type: SEToolUseStart, ToolID: "c1", ToolName: "Read"},
				{Type: SEToolInputJSON, Text: `{}`}, {Type: SEMessageStop}},
			[]ContentBlock{{Type: BlockThinking, Signature: "sig"}, {Type: BlockToolUse, ID: "c1", Name: "Read", Input: json.RawMessage(`{}`)}}},
		{"a signature is not carried to a later block",
			[]StreamEvent{{Type: SEThinkingSignature, Text: "one"}, {Type: SETextDelta, Text: "hi"},
				{Type: SEThinkingDelta, Text: "more"}, {Type: SEMessageStop}},
			[]ContentBlock{{Type: BlockThinking, Signature: "one"}, TextBlock("hi"), {Type: BlockThinking, Thinking: "more"}}},
		{"redacted thinking between blocks",
			[]StreamEvent{{Type: SEThinkingDelta, Text: "hmm"}, {Type: SEThinkingSignature, Text: "sig"},
				{Type: SERedactedThinking, Text: "b64"}, {Type: SETextDelta, Text: "hi"}, {Type: SEMessageStop}},
			[]ContentBlock{{Type: BlockThinking, Thinking: "hmm", Signature: "sig"}, {Type: BlockRedactedThinking, Data: "b64"}, TextBlock("hi")}},
		{"no thinking at all",
			[]StreamEvent{{Type: SETextDelta, Text: "hi"}, {Type: SEMessageStop}},
			[]ContentBlock{TextBlock("hi")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			acc := NewAccumulator()
			for _, ev := range c.events {
				acc.Add(ev)
			}
			if got := acc.Message().Content; !reflect.DeepEqual(got, c.want) {
				t.Fatalf("blocks = %+v\nwant     %+v", got, c.want)
			}
		})
	}
}

// What goes back on the wire: a thinking block always has its "thinking" key,
// a redacted block is replayed byte for byte, and no other block grows a key.
func TestContentBlockEncoding(t *testing.T) {
	cases := []struct {
		name  string
		block ContentBlock
		want  string
	}{
		{"thinking, empty text, signed", ContentBlock{Type: BlockThinking, Signature: "sig"}, `{"type":"thinking","thinking":"","signature":"sig"}`},
		{"thinking, empty text, unsigned", ContentBlock{Type: BlockThinking}, `{"type":"thinking","thinking":""}`},
		{"thinking with text", ContentBlock{Type: BlockThinking, Thinking: "hmm", Signature: "sig"}, `{"type":"thinking","thinking":"hmm","signature":"sig"}`},
		{"redacted thinking", ContentBlock{Type: BlockRedactedThinking, Data: "b64"}, `{"type":"redacted_thinking","data":"b64"}`},
		{"text", TextBlock("hi"), `{"type":"text","text":"hi"}`},
		{"tool use", ContentBlock{Type: BlockToolUse, ID: "c1", Name: "Read", Input: json.RawMessage(`{"a":1}`)}, `{"type":"tool_use","id":"c1","name":"Read","input":{"a":1}}`},
		{"tool result", ToolResultText("c1", "ok", false), `{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"ok"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := json.Marshal(c.block)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Fatalf("encoded %s\nwant    %s", got, c.want)
			}
			// Inside a message (how every adapter and the transcript encode it).
			msg, err := json.Marshal(Message{Role: RoleAssistant, Content: []ContentBlock{c.block}})
			if err != nil {
				t.Fatal(err)
			}
			if want := `{"role":"assistant","content":[` + c.want + `]}`; string(msg) != want {
				t.Fatalf("in a message %s\nwant         %s", msg, want)
			}
			var back ContentBlock
			if err := json.Unmarshal(got, &back); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back, c.block) {
				t.Fatalf("round trip = %+v, want %+v", back, c.block)
			}
		})
	}
}

// The non-streaming Anthropic path: a signed thinking block with empty text is
// parsed and goes back out with its "thinking" key.
func TestAnthropicEmptyThinkingRoundTrip(t *testing.T) {
	msg, _, _, err := parseAnthropicResponse([]byte(`{"type":"message","content":[
		{"type":"thinking","thinking":"","signature":"sig"},{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) != 2 || msg.Content[0].Type != BlockThinking || msg.Content[0].Signature != "sig" {
		t.Fatalf("parsed = %+v, want the signed thinking block kept", msg.Content)
	}
	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig"},{"type":"text","text":"hi"}]}`
	if string(out) != want {
		t.Fatalf("replayed %s\nwant     %s", out, want)
	}
}
