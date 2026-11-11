package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// A redacted_thinking block survives the whole trip on the Anthropic format:
// parsed from a non-streaming response and from a stream, kept in the message,
// and sent back byte for byte in the next request. The provider checks the
// turn's reasoning blocks against what it issued, so losing or reordering one
// fails the request.
func TestAnthropicRedactedThinkingRoundTrip(t *testing.T) {
	want := `{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"sig"},` +
		`{"type":"redacted_thinking","data":"b3BhcXVl"},{"type":"tool_use","id":"t1","name":"Read","input":{"path":"a"}}]}`

	// Non-streaming.
	msg, stop, _, err := parseAnthropicResponse([]byte(`{"type":"message","content":[
		{"type":"thinking","thinking":"plan","signature":"sig"},
		{"type":"redacted_thinking","data":"b3BhcXVl"},
		{"type":"tool_use","id":"t1","name":"Read","input":{"path":"a"}}],"stop_reason":"tool_use"}`))
	if err != nil || stop != "tool_use" {
		t.Fatalf("parse: %v (stop %q)", err, stop)
	}
	if got, _ := json.Marshal(msg); string(got) != want {
		t.Fatalf("non-streaming message\n got %s\nwant %s", got, want)
	}

	// Streaming: the block arrives whole in its content_block_start frame.
	acc := NewAccumulator()
	for _, frame := range []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"b3BhcXVl"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t1","name":"Read","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a\"}"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`{"type":"message_stop"}`,
	} {
		ev, ok, err := parseAnthropicFrame(frame)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(frame, `"redacted_thinking"`) && (!ok || ev.Type != SERedactedThinking || ev.Text != "b3BhcXVl") {
			t.Fatalf("redacted_thinking start frame parsed to %+v (ok=%v), want an SERedactedThinking event with the data", ev, ok)
		}
		if ok {
			acc.Add(ev)
		}
	}
	streamed := acc.Message()
	if got, _ := json.Marshal(streamed); string(got) != want {
		t.Fatalf("streamed message\n got %s\nwant %s", got, want)
	}

	// Replay with thinking on: the assistant turn goes back out unchanged, and
	// neither reasoning block carries a cache marker.
	history := []Message{
		UserText("go"),
		streamed,
		{Role: RoleUser, Content: []ContentBlock{ToolResultText("t1", "ok", false)}},
	}
	p := &anthropicProvider{cfg: Config{Model: "m", ThinkingType: "enabled"}}
	body, err := p.buildBody(CompletionRequest{Messages: history}, true)
	if err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatal(err)
	}
	if string(sent.Messages[1]) != want {
		t.Fatalf("replayed assistant turn\n got %s\nwant %s", sent.Messages[1], want)
	}

	// With thinking off, both kinds of reasoning block are stripped together:
	// the endpoint rejects either without the thinking parameter.
	off := &anthropicProvider{cfg: Config{Model: "m"}}
	body, err = off.buildBody(CompletionRequest{Messages: history}, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "redacted_thinking") || strings.Contains(string(body), `"thinking"`) {
		t.Fatalf("reasoning blocks were sent with thinking off: %s", body)
	}
	if !strings.Contains(string(body), `"tool_use"`) {
		t.Fatalf("the tool call was lost with the reasoning: %s", body)
	}
}

// A cache breakpoint never lands on a redacted_thinking block: like a thinking
// block it cannot carry one, so the mark moves to the block before it.
func TestAnthropicCacheSkipsRedactedThinking(t *testing.T) {
	msgs := []Message{
		UserText("go"),
		{Role: RoleAssistant, Content: []ContentBlock{
			TextBlock("answer"),
			{Type: BlockRedactedThinking, Data: "b3BhcXVl"},
		}},
	}
	out, err := anthropicMessages(msgs, true, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	last := string(out[1])
	if !strings.Contains(last, `{"type":"text","text":"answer","cache_control":{"type":"ephemeral"}}`) {
		t.Fatalf("the mark is not on the text block: %s", last)
	}
	if !strings.Contains(last, `{"type":"redacted_thinking","data":"b3BhcXVl"}`) {
		t.Fatalf("the redacted block was changed: %s", last)
	}
}
