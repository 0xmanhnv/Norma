package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/0xmanhnv/corvid/compaction"
	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// These tests guard the property prompt caching depends on: a provider's cache
// is a prefix match over the request as rendered (tools, then system, then
// messages), so within a run every request must start with the exact bytes of
// the one before it. They drive the real loop against the real Anthropic
// adapter and a fake endpoint that records the request bodies, so anything
// that rewrites an earlier byte — in the loop, in compaction or in the adapter
// — shows up here.

// wireRequest is one recorded request body, split into the three parts a
// prompt cache is keyed on, as raw bytes.
type wireRequest struct {
	Tools    json.RawMessage `json:"tools"`
	System   json.RawMessage `json:"system"`
	Messages json.RawMessage `json:"messages"`
	summary  bool            // the compaction summary call, not a turn of the run
	reported int             // input tokens the endpoint reported for it
}

var cacheMarkerRe = regexp.MustCompile(`,"cache_control":\{[^{}]*\}`)

// unmarked removes the cache markers, which move from request to request and
// are not part of what a cache entry is keyed on.
func unmarked(b []byte) []byte { return cacheMarkerRe.ReplaceAll(b, nil) }

// rendered is the request in cache order with the closing bracket of the
// message array removed, so a request that only appended messages renders to
// an extension of the previous one.
func (r wireRequest) rendered() []byte {
	var out []byte
	out = append(out, unmarked(r.Tools)...)
	out = append(out, unmarked(r.System)...)
	msgs := bytes.TrimSpace(unmarked(r.Messages))
	return append(out, bytes.TrimSuffix(msgs, []byte("]"))...)
}

// fakeAnthropic is an Anthropic-format endpoint. Every turn of the run asks
// for one more call of toolName until turns have been served, then answers in
// text. A request that ends with the compaction instruction gets a summary.
// It reports an input size derived from the body, the way a real endpoint
// would, so the compactor's thresholds see realistic numbers.
type fakeAnthropic struct {
	mu       sync.Mutex
	reqs     []wireRequest
	served   int
	turns    int
	toolName string
	srv      *httptest.Server
}

const summaryInstructionMark = "Your task is to create a detailed summary of the conversation so far"

func newFakeAnthropic(t *testing.T, toolName string, turns int) *fakeAnthropic {
	f := &fakeAnthropic{toolName: toolName, turns: turns}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAnthropic) provider(t *testing.T) llm.Provider {
	p, err := llm.NewProvider(llm.Config{Format: llm.FormatAnthropic, BaseURL: f.srv.URL, APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *fakeAnthropic) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req wireRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The instruction is the request's last message; a later turn that merely
	// has it in its history is an ordinary turn.
	tail := req.Messages
	if i := bytes.LastIndex(tail, []byte(`{"role":`)); i >= 0 {
		tail = tail[i:]
	}
	req.summary = bytes.Contains(tail, []byte(summaryInstructionMark))
	in := len(body) / 4
	req.reported = in
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	var n int
	if !req.summary {
		f.served++
		n = f.served
	}
	f.mu.Unlock()

	frames := []string{fmt.Sprintf(`{"type":"message_start","message":{"usage":{"input_tokens":%d,"output_tokens":1}}}`, in)}
	switch {
	case req.summary:
		frames = append(frames,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"<summary>what happened so far</summary>"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`)
	case n <= f.turns:
		frames = append(frames,
			fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_%d","name":%q,"input":{}}}`, n, f.toolName),
			fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"key\":\"k%d\"}"}}`, n),
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`)
	default:
		frames = append(frames,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`)
	}
	frames = append(frames, `{"type":"message_stop"}`)
	w.Header().Set("content-type", "text/event-stream")
	for _, fr := range frames {
		fmt.Fprintf(w, "data: %s\n\n", fr)
	}
}

func (f *fakeAnthropic) requests() []wireRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wireRequest(nil), f.reqs...)
}

// bulkTool returns size characters of output that depend on the input, like a
// file read: large, and different on every call.
func bulkTool(name string, size int) tool.CoreTool {
	return tool.Build(tool.Spec{
		Name:        name,
		Description: "Returns the content stored under a key.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"key": map[string]any{"type": "string"}},
			"required":   []any{"key"},
		},
		ReadOnly: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
			line := "content for " + string(input) + "\n"
			return tool.Text(strings.Repeat(line, size/len(line)+1)[:size]), nil
		},
	})
}

func runLoop(t *testing.T, in QueryInput) *Terminal {
	t.Helper()
	in.Messages = []llm.Message{llm.UserText("work through the keys")}
	in.System = []string{"You are a careful agent.", "Session notes: none."}
	in.DynamicBoundary = 1
	in.PermissionMode = permission.ModeBypass
	in.WorkingDir = t.TempDir()
	var term *Terminal
	for ev, err := range Query(context.Background(), in, QueryDeps{}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == KindResult {
			term = ev.Terminal
		}
	}
	if term == nil || term.Reason != ReasonCompleted {
		t.Fatalf("run did not complete: %+v", term)
	}
	return term
}

// prefixBreaks returns the indexes i for which request i does not start with
// the rendered bytes of request i-1.
func prefixBreaks(reqs []wireRequest) []int {
	var breaks []int
	for i := 1; i < len(reqs); i++ {
		if !bytes.HasPrefix(reqs[i].rendered(), reqs[i-1].rendered()) {
			breaks = append(breaks, i)
		}
	}
	return breaks
}

// Across the turns of a tool loop with no compaction pass, every request is a
// byte-for-byte extension of the one before it. One changed byte anywhere in
// tools, system or an earlier message would make every later turn pay for the
// whole history again.
func TestLoopRequestsAreBytePrefixesOfEachOther(t *testing.T) {
	const turns = 12
	f := newFakeAnthropic(t, "Lookup", turns)
	runLoop(t, QueryInput{
		Provider: f.provider(t),
		Tools:    tool.NewRegistry(bulkTool("Lookup", 2000), bulkTool("Other", 10)),
		// A compactor is present but far from any threshold: it must leave the
		// history alone.
		Compactor: compaction.New(compaction.Config{ContextWindow: 1_000_000}, nil),
	})
	reqs := f.requests()
	if len(reqs) != turns+1 {
		t.Fatalf("%d requests, want %d", len(reqs), turns+1)
	}
	if breaks := prefixBreaks(reqs); len(breaks) != 0 {
		i := breaks[0]
		t.Fatalf("request %d does not extend request %d (breaks at %v):\nprev: %s\nnext: %s",
			i, i-1, breaks, tailOf(reqs[i-1].rendered()), tailOf(reqs[i].rendered()))
	}
	// And the cache is actually asked for: each request marks the tools, the
	// static system prefix and the conversation.
	for i, r := range reqs {
		for part, raw := range map[string]json.RawMessage{"tools": r.Tools, "system": r.System, "messages": r.Messages} {
			if !bytes.Contains(raw, []byte(`"cache_control"`)) {
				t.Fatalf("request %d: %s carries no cache marker", i, part)
			}
		}
	}
}

// The compaction summary is made over the run's own prefix: its tools and
// system prompt are the session's bytes, and its messages are the previous
// request's with the instruction appended, so the call reads the cache the
// run just wrote instead of paying for the whole history as fresh input. Its
// usage lands in the run's totals.
func TestSummaryCallSharesTheRunsPrefix(t *testing.T) {
	const turns = 30
	f := newFakeAnthropic(t, "Lookup", turns) // not a tool MicroCompact clears
	p := f.provider(t)
	term := runLoop(t, QueryInput{
		Provider: p,
		Tools:    tool.NewRegistry(bulkTool("Lookup", 4000), bulkTool("Other", 10)),
		Compactor: compaction.New(compaction.Config{ContextWindow: 30_000, MaxOutputTokens: 2000},
			compaction.ProviderSummarizer(p, 2000, false)),
	})
	reqs := f.requests()
	summaries := 0
	var turnInput, allInput int
	for i, r := range reqs {
		in := r.reported
		allInput += in
		if !r.summary {
			turnInput += in
			continue
		}
		summaries++
		prev := reqs[i-1]
		if prev.summary {
			t.Fatalf("request %d: two summary calls in a row", i)
		}
		if !bytes.Equal(r.Tools, prev.Tools) {
			t.Fatalf("summary tools differ from the session's:\n%s\n%s", r.Tools, prev.Tools)
		}
		if !bytes.Equal(r.System, prev.System) {
			t.Fatalf("summary system differs from the session's:\n%s\n%s", r.System, prev.System)
		}
		if !bytes.HasPrefix(r.rendered(), prev.rendered()) {
			t.Fatalf("summary request does not extend the run's previous request:\nprev: %s\nsummary: %s",
				tailOf(prev.rendered()), tailOf(r.rendered()))
		}
		if !bytes.Contains(r.Messages, []byte(`"cache_control"`)) {
			t.Fatal("summary request asks for no cache read on the conversation")
		}
	}
	if summaries == 0 {
		t.Fatal("the run never compacted; the test exercises nothing")
	}
	// Each summary rewrites the history once, and only once.
	if got := len(prefixBreaks(turnsOnly(reqs))); got != summaries {
		t.Fatalf("%d prefix breaks between turns for %d summaries", got, summaries)
	}
	if term.Usage.InputTokens != allInput {
		t.Fatalf("run usage counts %d input tokens; turns sent %d and summaries %d more",
			term.Usage.InputTokens, turnInput, allInput-turnInput)
	}
}

func turnsOnly(reqs []wireRequest) []wireRequest {
	var out []wireRequest
	for _, r := range reqs {
		if !r.summary {
			out = append(out, r)
		}
	}
	return out
}

func tailOf(b []byte) string {
	if len(b) > 300 {
		b = b[len(b)-300:]
	}
	return string(b)
}
