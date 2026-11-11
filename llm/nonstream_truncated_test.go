package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// A non-streaming response the output limit ended inside a tool call reports
// it: the cut call is marked Truncated, so the loop can answer it instead of
// running it. A stream shows the cut (the arguments stop mid-JSON); a whole
// response hides it on the Anthropic format, where the input arrives as a
// well-formed object holding only what was complete.
func TestNonStreamingResponsesReportTruncatedToolCalls(t *testing.T) {
	type parse func([]byte) (Message, string, Usage, error)
	cases := []struct {
		name      string
		parse     parse
		body      string
		wantStop  string
		wantTrunc []bool // per tool call
		wantInput []string
	}{
		{"anthropic: cut in the last call", parseAnthropicResponse,
			`{"type":"message","stop_reason":"max_tokens","content":[
				{"type":"tool_use","id":"a","name":"Write","input":{"path":"one"}},
				{"type":"tool_use","id":"b","name":"Write","input":{"path":"two"}}]}`,
			"max_tokens", []bool{false, true}, []string{`{"path":"one"}`, `{"path":"two"}`}},
		{"anthropic: limit reached in text after a call", parseAnthropicResponse,
			`{"type":"message","stop_reason":"max_tokens","content":[
				{"type":"tool_use","id":"a","name":"Write","input":{"path":"one"}},
				{"type":"text","text":"and then"}]}`,
			"max_tokens", []bool{false}, []string{`{"path":"one"}`}},
		{"anthropic: ordinary tool turn", parseAnthropicResponse,
			`{"type":"message","stop_reason":"tool_use","content":[
				{"type":"tool_use","id":"a","name":"Write","input":{"path":"one"}}]}`,
			"tool_use", []bool{false}, []string{`{"path":"one"}`}},
		{"openai: arguments cut mid-JSON", parseOpenAIResponse,
			`{"choices":[{"finish_reason":"length","message":{"tool_calls":[
				{"id":"a","type":"function","function":{"name":"Write","arguments":"{\"path\":\"one\"}"}},
				{"id":"b","type":"function","function":{"name":"Write","arguments":"{\"path\":\"two\",\"content\":\"very lo"}}]}}]}`,
			"max_tokens", []bool{false, true}, []string{`{"path":"one"}`, `{}`}},
		{"openai: complete call at the limit", parseOpenAIResponse,
			`{"choices":[{"finish_reason":"length","message":{"tool_calls":[
				{"id":"a","type":"function","function":{"name":"Write","arguments":"{\"path\":\"one\"}"}}]}}]}`,
			"max_tokens", []bool{false}, []string{`{"path":"one"}`}},
		{"openai: ordinary tool turn", parseOpenAIResponse,
			`{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[
				{"id":"a","type":"function","function":{"name":"Write","arguments":""}}]}}]}`,
			"tool_use", []bool{false}, []string{`{}`}},
		{"responses: arguments cut mid-JSON", parseResponsesResponse,
			`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[
				{"type":"function_call","call_id":"a","name":"Write","arguments":"{\"path\":\"tw"}]}`,
			"max_tokens", []bool{true}, []string{`{}`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, stop, _, err := c.parse([]byte(c.body))
			if err != nil {
				t.Fatal(err)
			}
			if stop != c.wantStop {
				t.Fatalf("stop = %q, want %q", stop, c.wantStop)
			}
			uses := msg.ToolUses()
			if len(uses) != len(c.wantTrunc) {
				t.Fatalf("%d tool calls, want %d", len(uses), len(c.wantTrunc))
			}
			for i, u := range uses {
				if u.Truncated != c.wantTrunc[i] {
					t.Errorf("call %d: Truncated = %v, want %v", i, u.Truncated, c.wantTrunc[i])
				}
				if string(u.Input) != c.wantInput[i] {
					t.Errorf("call %d: input = %s, want %s", i, u.Input, c.wantInput[i])
				}
			}
			// The mark is local to the turn, and a cut call still encodes: the
			// message goes to the transcript and back to the provider.
			b, err := json.Marshal(msg)
			if err != nil {
				t.Fatalf("the message does not encode: %v", err)
			}
			if strings.Contains(string(b), "runcated") {
				t.Fatalf("the mark was encoded: %s", b)
			}
		})
	}
}
