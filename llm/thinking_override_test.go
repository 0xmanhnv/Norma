package llm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureReq runs one arbitrary request and returns the raw + parsed request body.
func captureReq(t *testing.T, cfg Config, req CompletionRequest, stub string) (string, map[string]any) {
	t.Helper()
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.Write([]byte(stub))
	}))
	defer srv.Close()
	cfg.BaseURL, cfg.APIKey, cfg.Model = srv.URL, "k", "m"
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	collect(t, p, req)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	return string(raw), m
}

func thinkingType(m map[string]any) string {
	if th, ok := m["thinking"].(map[string]any); ok {
		s, _ := th["type"].(string)
		return s
	}
	return ""
}

// A per-request Thinking="disabled" override must win over an enabled config AND
// strip replayed thinking blocks (Anthropic rejects them when thinking is off).
func TestAnthropicPerRequestThinkingDisabledStrips(t *testing.T) {
	stub := sse(`data: {"type":"message_stop"}`, ``)
	req := CompletionRequest{
		Thinking: "disabled",
		Messages: []Message{
			UserText("hi"),
			{Role: RoleAssistant, Content: []ContentBlock{
				{Type: BlockThinking, Thinking: "SECRET_THOUGHT"},
				TextBlock("answer"),
			}},
		},
	}
	raw, m := captureReq(t, Config{Format: FormatAnthropic, ThinkingType: "enabled"}, req, stub)
	if thinkingType(m) != "disabled" {
		t.Fatalf("override should force disabled, got %q", thinkingType(m))
	}
	if strings.Contains(raw, "SECRET_THOUGHT") {
		t.Fatal("thinking block must be stripped when thinking is disabled")
	}
	if !strings.Contains(raw, "answer") {
		t.Fatal("sibling text block should survive")
	}
}

func TestAnthropicPerRequestThinkingEnabledKeeps(t *testing.T) {
	stub := sse(`data: {"type":"message_stop"}`, ``)
	req := CompletionRequest{
		Thinking: "enabled",
		Messages: []Message{
			{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockThinking, Thinking: "KEPT_THOUGHT", Signature: "s"}, TextBlock("a")}},
		},
	}
	raw, m := captureReq(t, Config{Format: FormatAnthropic}, req, stub) // cfg thinking unset
	if thinkingType(m) != "enabled" {
		t.Fatalf("override should force enabled, got %q", thinkingType(m))
	}
	if !strings.Contains(raw, "KEPT_THOUGHT") {
		t.Fatal("thinking block should be replayed when enabled")
	}
}

func TestAnthropicThinkingInheritsConfigWhenUnset(t *testing.T) {
	stub := sse(`data: {"type":"message_stop"}`, ``)
	req := CompletionRequest{Messages: []Message{UserText("hi")}} // Thinking == ""
	_, m := captureReq(t, Config{Format: FormatAnthropic, ThinkingType: "enabled"}, req, stub)
	if thinkingType(m) != "enabled" {
		t.Fatalf("empty override should inherit config, got %q", thinkingType(m))
	}
	// And with config also unset, thinking is omitted entirely.
	_, m2 := captureReq(t, Config{Format: FormatAnthropic}, req, stub)
	if _, ok := m2["thinking"]; ok {
		t.Fatalf("thinking should be omitted, got %v", m2["thinking"])
	}
}

func TestOpenAIPerRequestThinkingOverride(t *testing.T) {
	// A non-empty stub: an empty completion would trip the empty-response retry
	// (this test only inspects the request body, so give it minimal content).
	stub := sse(`data: {"choices":[{"index":0,"delta":{"content":"ok"}}]}`, ``, `data: [DONE]`, ``)
	req := CompletionRequest{Thinking: "disabled", Messages: []Message{UserText("hi")}}
	_, m := captureReq(t, Config{Format: FormatOpenAI, ThinkingType: "enabled"}, req, stub)
	if thinkingType(m) != "disabled" {
		t.Fatalf("override should force disabled, got %q", thinkingType(m))
	}
	// Empty override inherits config.
	req2 := CompletionRequest{Messages: []Message{UserText("hi")}}
	_, m2 := captureReq(t, Config{Format: FormatOpenAI, ThinkingType: "enabled"}, req2, stub)
	if thinkingType(m2) != "enabled" {
		t.Fatalf("empty override should inherit config, got %q", thinkingType(m2))
	}
}

// A "disabled" the endpoint never asked for is left off the wire. The provider
// here has no ThinkingType, so nothing has said this endpoint takes a thinking
// field: several OpenAI-compatible gateways reject the unknown key, and newer
// Claude models reject "disabled" itself. Compaction's summary used to send it
// to every endpoint, and three rejections turned compaction off.
func TestThinkingDisabledIsOmittedWhenEndpointNeverAskedForIt(t *testing.T) {
	oaStub := sse(`data: {"choices":[{"index":0,"delta":{"content":"ok"}}]}`, ``, `data: [DONE]`, ``)
	anStub := sse(`event: message_stop`, `data: {"type":"message_stop"}`, ``)
	req := CompletionRequest{Thinking: "disabled", Messages: []Message{
		UserText("hi"),
		{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockThinking, Thinking: "SECRET_THOUGHT", Signature: "s"}, TextBlock("a")}},
		UserText("again"),
	}}
	for _, f := range []Format{FormatOpenAI, FormatAnthropic} {
		stub := oaStub
		if f == FormatAnthropic {
			stub = anStub
		}
		raw, m := captureReq(t, Config{Format: f}, req, stub)
		if _, ok := m["thinking"]; ok {
			t.Fatalf("%s: a thinking field was sent to an endpoint that never asked for one: %s", f, raw)
		}
		if f == FormatAnthropic && strings.Contains(raw, "SECRET_THOUGHT") {
			t.Fatalf("%s: thinking blocks must still be stripped when thinking is off: %s", f, raw)
		}
	}
	// A provider configured with a thinking type has told us the endpoint takes
	// the field, so the override is sent.
	_, m := captureReq(t, Config{Format: FormatAnthropic, ThinkingType: "enabled"}, req, anStub)
	if thinkingType(m) != "disabled" {
		t.Fatalf("configured endpoint: thinking=%q, want disabled", thinkingType(m))
	}
}

// Any other mode is sent as asked, and counts as thinking on: its blocks are
// replayed. ("adaptive" used to be treated as off and had its history stripped.)
func TestThinkingAdaptiveIsSentAndKeepsBlocks(t *testing.T) {
	stub := sse(`event: message_stop`, `data: {"type":"message_stop"}`, ``)
	req := CompletionRequest{Thinking: "adaptive", Messages: []Message{
		UserText("hi"),
		{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockThinking, Thinking: "KEPT_THOUGHT", Signature: "s"}, TextBlock("a")}},
		UserText("again"),
	}}
	raw, m := captureReq(t, Config{Format: FormatAnthropic}, req, stub)
	if thinkingType(m) != "adaptive" {
		t.Fatalf("thinking=%q, want adaptive", thinkingType(m))
	}
	if !strings.Contains(raw, "KEPT_THOUGHT") {
		t.Fatalf("thinking blocks dropped with thinking on: %s", raw)
	}
}
