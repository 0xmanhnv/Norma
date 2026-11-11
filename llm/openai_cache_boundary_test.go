package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// With cache control on, the OpenAI format carries a breakpoint at the end of
// the tool list and one at the system boundary. Recorded from the wire: two
// sessions that share their tools and static system prompt, and nothing else,
// send those two parts byte for byte, each ending in a marker. A gateway that
// honours explicit breakpoints can therefore serve the second session's tools
// and system prompt from what the first one wrote.
func TestOpenAICacheControlMarksToolsAndSystemBoundary(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	tools := []ToolSchema{
		{Name: "Read", Description: "reads", InputSchema: map[string]any{"type": "object"}},
		{Name: "Write", Description: "writes", InputSchema: map[string]any{"type": "object"}},
	}
	session := func(cfg Config, dynamic string, msgs ...Message) {
		cfg.Format, cfg.BaseURL, cfg.APIKey, cfg.Model = FormatOpenAI, srv.URL, "k", "m"
		p, err := NewProvider(cfg)
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, err = p.Complete(context.Background(), CompletionRequest{
			System:          []string{"static rules", dynamic},
			DynamicBoundary: 1,
			Tools:           tools,
			Messages:        msgs,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	session(Config{CacheControl: true}, "notes of session one", UserText("first task"))
	session(Config{CacheControl: true}, "notes of session two", UserText("another task"),
		asst(TextBlock("on it")), UserText("go on"))
	session(Config{}, "notes of session three", UserText("first task")) // cache control off

	type wire struct {
		Tools    []json.RawMessage `json:"tools"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	var reqs [3]wire
	for i, b := range bodies {
		if err := json.Unmarshal(b, &reqs[i]); err != nil {
			t.Fatal(err)
		}
	}
	one, two, off := reqs[0], reqs[1], reqs[2]

	// Tools: identical across the two sessions, the marker on the last tool only.
	if len(one.Tools) != 2 || len(two.Tools) != 2 {
		t.Fatalf("tools: %d and %d", len(one.Tools), len(two.Tools))
	}
	for i := range one.Tools {
		if !bytes.Equal(one.Tools[i], two.Tools[i]) {
			t.Fatalf("tool %d differs between sessions:\n%s\n%s", i, one.Tools[i], two.Tools[i])
		}
	}
	if bytes.Contains(one.Tools[0], []byte("cache_control")) {
		t.Fatalf("the first tool is marked: %s", one.Tools[0])
	}
	if !bytes.HasSuffix(one.Tools[1], []byte(`,"cache_control":{"type":"ephemeral"}}`)) {
		t.Fatalf("the last tool does not end in a breakpoint: %s", one.Tools[1])
	}

	// System: the static part is its own marked content part, identical across
	// sessions; the dynamic part follows it, unmarked.
	var parts [2][]json.RawMessage
	for i, r := range []wire{one, two} {
		if r.Messages[0].Role != "system" {
			t.Fatalf("session %d: first message is %q", i+1, r.Messages[0].Role)
		}
		if err := json.Unmarshal(r.Messages[0].Content, &parts[i]); err != nil || len(parts[i]) != 2 {
			t.Fatalf("session %d: system content is not two parts: %s", i+1, r.Messages[0].Content)
		}
	}
	const static = `{"type":"text","text":"static rules","cache_control":{"type":"ephemeral"}}`
	if string(parts[0][0]) != static || string(parts[1][0]) != static {
		t.Fatalf("static system part:\n%s\n%s\nwant %s", parts[0][0], parts[1][0], static)
	}
	if bytes.Contains(parts[0][1], []byte("cache_control")) || bytes.Equal(parts[0][1], parts[1][1]) {
		t.Fatalf("dynamic system parts: %s / %s", parts[0][1], parts[1][1])
	}

	// Off by default: a strict endpoint sees no field it does not know.
	if bytes.Contains(bodies[2], []byte("cache_control")) {
		t.Fatalf("cache control is off, yet the request carries a marker: %s", bodies[2])
	}
	if string(off.Messages[0].Content) != `"static rules\n\nnotes of session three"` {
		t.Fatalf("plain system content changed: %s", off.Messages[0].Content)
	}
}
