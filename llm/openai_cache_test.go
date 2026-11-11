package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func oaBody(t *testing.T, cfg Config, req CompletionRequest) (string, []map[string]any) {
	t.Helper()
	cfg.Model = "m"
	p := &openaiProvider{cfg: cfg}
	b, err := p.buildBody(req, true)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}
	return string(b), parsed.Messages
}

func toolLoop() CompletionRequest {
	return CompletionRequest{
		System:          []string{"static rules", "dynamic notes"},
		DynamicBoundary: 1,
		Tools:           []ToolSchema{{Name: "Read", InputSchema: map[string]any{"type": "object"}}},
		Messages: []Message{
			usr(TextBlock("task")),
			asst(use("t1")),
			usr(ToolResultText("t1", "first", false)),
			asst(use("t2")),
			usr(ToolResultText("t2", "second", false)),
		},
	}
}

// markedPart returns the cache_control of a message whose content is an array
// of parts ending in a marked one, and whether it is.
func markedPart(m map[string]any) (map[string]any, bool) {
	parts, ok := m["content"].([]any)
	if !ok || len(parts) == 0 {
		return nil, false
	}
	for _, p := range parts {
		if cc, ok := p.(map[string]any)["cache_control"].(map[string]any); ok {
			return cc, true
		}
	}
	return nil, false
}

// By default the OpenAI format sends plain string content and no marker: a
// strict endpoint rejects fields it does not know.
func TestOpenAINoCacheControlByDefault(t *testing.T) {
	body, msgs := oaBody(t, Config{}, toolLoop())
	if strings.Contains(body, "cache_control") {
		t.Fatalf("cache_control sent without the option: %s", body)
	}
	for i, m := range msgs {
		if c, ok := m["content"]; ok {
			if _, isString := c.(string); !isString {
				t.Fatalf("message %d content is not a plain string: %v", i, c)
			}
		}
	}
}

// With Config.CacheControl the markers sit where the Anthropic adapter puts
// them: the static system prefix, the previous tail and the tail.
func TestOpenAICacheControlMarksSameBoundaries(t *testing.T) {
	body, msgs := oaBody(t, Config{CacheControl: true}, toolLoop())
	if n := strings.Count(body, `"cache_control"`); n != 3 {
		t.Fatalf("%d markers, want 3 (system prefix, previous tail, tail): %s", n, body)
	}
	// system: two parts, the static one marked, the dynamic one not.
	sys := msgs[0]["content"].([]any)
	if len(sys) != 2 || sys[0].(map[string]any)["text"] != "static rules" || sys[1].(map[string]any)["text"] != "dynamic notes" {
		t.Fatalf("system parts: %v", sys)
	}
	if _, ok := sys[0].(map[string]any)["cache_control"]; !ok {
		t.Fatal("static system part is not marked")
	}
	if _, ok := sys[1].(map[string]any)["cache_control"]; ok {
		t.Fatal("dynamic system part is marked")
	}
	// messages: system, user(task), assistant(t1), tool(t1), assistant(t2), tool(t2)
	if _, ok := markedPart(msgs[5]); !ok || msgs[5]["role"] != "tool" {
		t.Fatalf("tail (last tool result) is not marked: %v", msgs[5])
	}
	if _, ok := markedPart(msgs[3]); !ok {
		t.Fatalf("previous tail (the tool result before the last assistant turn) is not marked: %v", msgs[3])
	}
	for _, i := range []int{1, 2, 4} {
		if _, ok := markedPart(msgs[i]); ok {
			t.Fatalf("message %d is marked", i)
		}
	}
	// The marked messages keep their text and their other fields.
	if msgs[5]["tool_call_id"] != "t2" || msgs[5]["content"].([]any)[0].(map[string]any)["text"] != "second" {
		t.Fatalf("marked tool message lost data: %v", msgs[5])
	}
}

// The working frame at the end of a request is never sent again, so the tail
// marker goes on the message before it.
func TestOpenAICacheControlSkipsVolatileTail(t *testing.T) {
	req := toolLoop()
	req.Messages = append(req.Messages, usr(TextBlock("working frame")))
	req.VolatileTail = 1
	_, msgs := oaBody(t, Config{CacheControl: true}, req)
	last := msgs[len(msgs)-1]
	if last["content"] != "working frame" {
		t.Fatalf("volatile tail was marked or altered: %v", last)
	}
	if _, ok := markedPart(msgs[len(msgs)-2]); !ok {
		t.Fatal("the message before the volatile tail is not marked")
	}
}

// TTLs follow the request the same way as on the Anthropic format, and
// DisableCache turns the markers off for one request.
func TestOpenAICacheControlTTLAndDisable(t *testing.T) {
	req := toolLoop()
	req.ConversationCacheTTL = "1h"
	body, _ := oaBody(t, Config{CacheControl: true}, req)
	if n := strings.Count(body, `"ttl":"1h"`); n != 3 {
		t.Fatalf("%d one-hour markers, want 3: %s", n, body)
	}
	req.ConversationCacheTTL, req.CacheTTL = "", "1h"
	body, _ = oaBody(t, Config{CacheControl: true}, req)
	if n := strings.Count(body, `"ttl":"1h"`); n != 1 {
		t.Fatalf("%d one-hour markers, want 1 (system only): %s", n, body)
	}
	req.DisableCache = true
	if body, _ = oaBody(t, Config{CacheControl: true}, req); strings.Contains(body, "cache_control") {
		t.Fatalf("DisableCache left a marker: %s", body)
	}
}

// Apart from its markers, a request is the same with the option on or off for
// every message that is not marked, and the marked ones carry the same text.
func TestOpenAICacheControlKeepsUnmarkedMessagesByteIdentical(t *testing.T) {
	_, off := oaBody(t, Config{}, toolLoop())
	_, on := oaBody(t, Config{CacheControl: true}, toolLoop())
	if len(off) != len(on) {
		t.Fatalf("message count differs: %d vs %d", len(off), len(on))
	}
	for i := range off {
		if _, marked := markedPart(on[i]); marked {
			continue
		}
		a, _ := json.Marshal(off[i])
		b, _ := json.Marshal(on[i])
		if string(a) != string(b) {
			t.Fatalf("unmarked message %d changed:\n%s\n%s", i, a, b)
		}
	}
}

// Cache usage is read whatever the endpoint calls it, and never counted twice.
func TestOpenAIUsageShapes(t *testing.T) {
	cases := []struct {
		name  string
		usage string
		want  Usage
	}{
		{"plain", `{"prompt_tokens":100,"completion_tokens":7}`,
			Usage{InputTokens: 100, OutputTokens: 7}},
		{"openai cached_tokens", `{"prompt_tokens":1000,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":800}}`,
			Usage{InputTokens: 1000, OutputTokens: 7, CacheReadTokens: 800}},
		{"router: read and write in details", `{"prompt_tokens":1000,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":600,"cache_write_tokens":300}}`,
			Usage{InputTokens: 1000, OutputTokens: 7, CacheReadTokens: 600, CacheWriteTokens: 300}},
		{"anthropic names, inclusive total", `{"prompt_tokens":1000,"completion_tokens":7,"cache_read_input_tokens":600,"cache_creation_input_tokens":300}`,
			Usage{InputTokens: 1000, OutputTokens: 7, CacheReadTokens: 600, CacheWriteTokens: 300}},
		{"both namings echo the same read: counted once", `{"prompt_tokens":1000,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":600},"cache_read_input_tokens":600,"cache_creation_input_tokens":300}`,
			Usage{InputTokens: 1000, OutputTokens: 7, CacheReadTokens: 600, CacheWriteTokens: 300}},
		{"anthropic names, prompt_tokens is the uncached part only", `{"prompt_tokens":100,"completion_tokens":7,"cache_read_input_tokens":600,"cache_creation_input_tokens":300}`,
			Usage{InputTokens: 1000, OutputTokens: 7, CacheReadTokens: 600, CacheWriteTokens: 300}},
		{"cache_creation_tokens in details", `{"prompt_tokens":1000,"completion_tokens":7,"prompt_tokens_details":{"cache_creation_tokens":300}}`,
			Usage{InputTokens: 1000, OutputTokens: 7, CacheWriteTokens: 300}},
		{"deepseek hit tokens", `{"prompt_tokens":1000,"completion_tokens":7,"prompt_cache_hit_tokens":900,"prompt_cache_miss_tokens":100}`,
			Usage{InputTokens: 1000, OutputTokens: 7, CacheReadTokens: 900}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Non-streaming body.
			_, _, got, err := parseOpenAIResponse([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":` + c.usage + `}`))
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("non-streaming: %+v, want %+v", got, c.want)
			}
			// Streaming: the usage arrives on a trailing frame with no choices.
			_, _, got, ok := parseOpenAIFrame(`{"choices":[],"usage":`+c.usage+`}`, map[int]bool{})
			if !ok || got != c.want {
				t.Fatalf("streaming: %+v (ok=%v), want %+v", got, ok, c.want)
			}
			if got.CacheReadTokens+got.CacheWriteTokens > got.InputTokens {
				t.Fatalf("cache counts exceed the input total: %+v", got)
			}
		})
	}
}
