package llm

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func asst(blocks ...ContentBlock) Message { return Message{Role: RoleAssistant, Content: blocks} }
func usr(blocks ...ContentBlock) Message  { return Message{Role: RoleUser, Content: blocks} }
func use(id string) ContentBlock {
	return ContentBlock{Type: BlockToolUse, ID: id, Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}
}
func result(id, text string) ContentBlock { return ToolResultText(id, text, false).Content[0] }

func markedBlocks(t *testing.T, body []byte) []string {
	t.Helper()
	var req struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	var out []string
	for i, m := range req.Messages {
		for j, b := range m.Content {
			if _, ok := b["cache_control"]; ok {
				out = append(out, strings.TrimSpace(string(rune('0'+i))+":"+string(rune('0'+j))))
			}
		}
	}
	return out
}

func buildFor(t *testing.T, req CompletionRequest) []byte {
	t.Helper()
	p := &anthropicProvider{cfg: Config{Model: "m"}}
	b, err := p.buildBody(req, false)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// With caching off the messages encode exactly as before.
func TestAnthropicNoCacheLeavesMessagesAlone(t *testing.T) {
	msgs := []Message{usr(TextBlock("task")), asst(use("t1")), usr(result("t1", "out"))}
	body := buildFor(t, CompletionRequest{Messages: msgs})
	plain, _ := json.Marshal(msgs)
	var got struct {
		Messages json.RawMessage `json:"messages"`
	}
	json.Unmarshal(body, &got)
	var a, b bytes.Buffer
	json.Compact(&a, got.Messages)
	json.Compact(&b, plain)
	if a.String() != b.String() {
		t.Fatalf("messages changed with caching off:\n%s\n%s", a.String(), b.String())
	}
}

// A tool loop with a working frame: the tail mark sits on the last tool result,
// not on the frame, and the previous request's tail is marked again.
func TestAnthropicCacheMarksStableTailAndPreviousTail(t *testing.T) {
	msgs := []Message{
		usr(TextBlock("task")),
		asst(use("t1")),
		usr(result("t1", "first")),
		asst(use("t2")),
		usr(result("t2", "second"), TextBlock("FRAME: changes every turn")),
	}
	body := buildFor(t, CompletionRequest{System: []string{"sys"}, DynamicBoundary: 1, VolatileTail: 1, Messages: msgs})
	got := markedBlocks(t, body)
	if len(got) != 2 || got[0] != "2:0" || got[1] != "4:0" {
		t.Fatalf("marks %v, want [2:0 4:0] (previous tail, tail)", got)
	}
	if n := strings.Count(string(body), `"cache_control"`); n > 4 {
		t.Fatalf("%d breakpoints, Anthropic allows 4", n)
	}
}

// The prefix the next request reads must be byte-identical to what this request
// wrote: request N's tail mark is request N+1's previous-tail mark.
func TestAnthropicCachePrefixMatchesAcrossTurns(t *testing.T) {
	hist := []Message{usr(TextBlock("task")), asst(use("t1")), usr(result("t1", "first"))}
	frame := TextBlock("FRAME 1")
	req1 := append(append([]Message{}, hist[:2]...), usr(result("t1", "first"), frame))
	b1, err := anthropicMessages(req1, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	req2 := append(append([]Message{}, hist...), asst(use("t2")), usr(result("t2", "second"), TextBlock("FRAME 2")))
	b2, err := anthropicMessages(req2, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	// In request 1, message 2's first block (the tool result) is the tail mark;
	// in request 2 the same block is the previous-tail mark.
	blk := func(raw json.RawMessage, i int) string {
		var m struct {
			Content []json.RawMessage `json:"content"`
		}
		json.Unmarshal(raw, &m)
		return string(m.Content[i])
	}
	if blk(b1[2], 0) != blk(b2[2], 0) || !strings.Contains(blk(b2[2], 0), "cache_control") {
		t.Fatalf("cached prefix differs between turns:\n%s\n%s", blk(b1[2], 0), blk(b2[2], 0))
	}
	// Earlier messages carry the same content; only where breakpoints sit moves.
	unmark := func(raw json.RawMessage) string {
		return strings.ReplaceAll(string(raw), `,"cache_control":{"type":"ephemeral"}`, "")
	}
	for i := 0; i < 2; i++ {
		if unmark(b1[i]) != unmark(b2[i]) {
			t.Fatalf("message %d content differs between turns", i)
		}
	}
}

// Thinking blocks can't carry a breakpoint: the mark moves to the block before.
func TestAnthropicCacheSkipsThinking(t *testing.T) {
	msgs := []Message{usr(TextBlock("a"), ContentBlock{Type: BlockThinking, Thinking: "hmm", Signature: "s"})}
	b, err := anthropicMessages(msgs, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b[0]), `"text":"a","cache_control"`) || strings.Count(string(b[0]), "cache_control") != 1 {
		t.Fatalf("got %s", b[0])
	}
}

// With caching on, the last tool carries a breakpoint and the request as a whole
// never exceeds four; with caching off no tool is marked.
func TestAnthropicCacheMarksEndOfTools(t *testing.T) {
	tools := []ToolSchema{{Name: "Bash", InputSchema: map[string]any{"type": "object"}}, {Name: "Read", InputSchema: map[string]any{"type": "object"}}}
	msgs := []Message{
		usr(TextBlock("task")),
		asst(use("t1")),
		usr(result("t1", "first")),
		asst(use("t2")),
		usr(result("t2", "second")),
	}
	marked := func(body []byte) []string {
		var req struct {
			Tools []map[string]any `json:"tools"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, tl := range req.Tools {
			if _, ok := tl["cache_control"]; ok {
				out = append(out, tl["name"].(string))
			}
		}
		return out
	}
	body := buildFor(t, CompletionRequest{System: []string{"sys", "dynamic"}, DynamicBoundary: 1, Messages: msgs, Tools: tools})
	if got := marked(body); len(got) != 1 || got[0] != "Read" {
		t.Fatalf("marked tools %v, want [Read]", got)
	}
	if n := strings.Count(string(body), `"cache_control"`); n != 4 {
		t.Fatalf("%d breakpoints, want 4 (tools, system, previous tail, tail)", n)
	}
	if got := marked(buildFor(t, CompletionRequest{System: []string{"sys"}, Messages: msgs, Tools: tools})); len(got) != 0 {
		t.Fatalf("caching off marked tools %v", got)
	}
}

// CacheTTL sets the lifetime of the tools and system breakpoints only; the
// conversation breakpoints keep the default, so every longer lifetime comes
// before every shorter one, as Anthropic requires.
func TestAnthropicCacheTTLOnStableBreakpoints(t *testing.T) {
	tools := []ToolSchema{{Name: "Bash", InputSchema: map[string]any{"type": "object"}}, {Name: "Read", InputSchema: map[string]any{"type": "object"}}}
	msgs := []Message{usr(TextBlock("task")), asst(use("t1")), usr(result("t1", "first")), asst(use("t2")), usr(result("t2", "second"))}
	req := CompletionRequest{System: []string{"sys", "dynamic"}, DynamicBoundary: 1, Messages: msgs, Tools: tools}

	body := string(buildFor(t, req))
	if strings.Contains(body, `"ttl"`) {
		t.Fatalf("default request carries a ttl: %s", body)
	}
	req.CacheTTL = "1h"
	body = string(buildFor(t, req))
	if n := strings.Count(body, `"cache_control"`); n != 4 {
		t.Fatalf("%d breakpoints, want 4", n)
	}
	if n := strings.Count(body, `"ttl":"1h"`); n != 2 {
		t.Fatalf("%d one-hour breakpoints, want 2 (tools, system): %s", n, body)
	}
	// Prompt order is tools, system, messages: the one-hour markers are on
	// the first two, the conversation keeps the default.
	var parsed struct {
		Tools    []json.RawMessage `json:"tools"`
		System   []json.RawMessage `json:"system"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(parsed.Tools[len(parsed.Tools)-1]), `"ttl":"1h"`) || !strings.Contains(string(parsed.System[0]), `"ttl":"1h"`) {
		t.Fatal("tools or system breakpoint lacks the one-hour ttl")
	}
	for _, m := range parsed.Messages {
		if strings.Contains(string(m), `"ttl"`) {
			t.Fatalf("a conversation breakpoint carries a ttl: %s", m)
		}
	}
	// The whole-static system prompt gets the lifetime too, and an unknown
	// value is ignored rather than sent.
	req.DynamicBoundary, req.System = 5, []string{"sys"}
	if n := strings.Count(string(buildFor(t, req)), `"ttl":"1h"`); n != 2 {
		t.Fatalf("all-static system: %d one-hour breakpoints", n)
	}
	req.CacheTTL = "forever"
	if strings.Contains(string(buildFor(t, req)), `"ttl"`) {
		t.Fatal("an unknown ttl was sent")
	}
	// Without caching (no boundary) nothing is marked whatever the ttl.
	req.DynamicBoundary, req.CacheTTL = 0, "1h"
	if strings.Contains(string(buildFor(t, req)), "cache_control") {
		t.Fatal("ttl turned caching on")
	}
}
