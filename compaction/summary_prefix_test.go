package compaction

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
)

// usageProvider records requests and answers each with replies[i] (the last
// one repeating), reporting a fixed usage per call. A reply of "" is a
// response with no text, as when the model calls a tool instead of answering.
type usageProvider struct {
	reqs    []llm.CompletionRequest
	replies []string
	usage   llm.Usage
}

func (p *usageProvider) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	p.reqs = append(p.reqs, req)
	reply := p.replies[min(len(p.reqs), len(p.replies))-1]
	return func(yield func(llm.StreamEvent, error) bool) {
		if reply == "" {
			if !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "t", ToolName: "Read"}, nil) {
				return
			}
		} else if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: reply}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn", Usage: p.usage}, nil)
	}
}

func (p *usageProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	acc := llm.NewAccumulator()
	for ev, err := range p.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		acc.Add(ev)
	}
	return acc.Message(), acc.StopReason, acc.Usage, nil
}

func sessionPrefix() llm.CompletionRequest {
	return llm.CompletionRequest{
		System:               []string{"static system", "dynamic system"},
		DynamicBoundary:      1,
		Tools:                []llm.ToolSchema{{Name: "Read", Description: "reads", InputSchema: map[string]any{"type": "object"}}},
		CacheTTL:             "1h",
		ConversationCacheTTL: "1h",
		Thinking:             "adaptive",
		ReasoningEffort:      "low",
		Messages:             []llm.Message{llm.UserText("<system-reminder>lead</system-reminder>")},
	}
}

func longConversation() []llm.Message {
	var msgs []llm.Message
	for i := range 9 { // ends on a user message; the kept tail is [assistant, user]
		role := llm.RoleUser
		if i%2 == 1 {
			role = llm.RoleAssistant
		}
		msgs = append(msgs, bigMsg(role, 4000))
	}
	return msgs
}

// With the run's prefix known, the summary request is the run's own request
// with the instruction appended: same system, boundary, tools and session
// settings, the messages the loop leads with, then the conversation as the
// next turn would send it. Nothing about it tells the provider to skip the
// cache. Its usage is handed on.
func TestSummaryRequestIsBuiltOnTheRunsPrefix(t *testing.T) {
	p := &usageProvider{replies: []string{"<summary>S</summary>"}, usage: llm.Usage{InputTokens: 900, OutputTokens: 40, CacheReadTokens: 850}}
	c := New(smallWindow(), ProviderSummarizer(p, 0, false))
	prefix := sessionPrefix()
	c.SetRequestPrefix(prefix)
	msgs := longConversation()

	out, ok := c.autoCompact(context.Background(), msgs, 9000, "auto")
	if !ok {
		t.Fatal("autoCompact did not run")
	}
	if len(p.reqs) != 1 {
		t.Fatalf("%d summary requests, want 1", len(p.reqs))
	}
	req := p.reqs[0]
	if !reflect.DeepEqual(req.System, prefix.System) || req.DynamicBoundary != prefix.DynamicBoundary {
		t.Fatalf("system %v boundary %d: not the run's", req.System, req.DynamicBoundary)
	}
	if !reflect.DeepEqual(req.Tools, prefix.Tools) {
		t.Fatalf("tools %v: not the run's", req.Tools)
	}
	if got, want := llm.SettingsOf(req), llm.SettingsOf(prefix); got != want {
		t.Fatalf("settings %+v, want the run's %+v", got, want)
	}
	if req.DisableCache {
		t.Fatal("the shared-prefix summary must keep its cache markers")
	}
	want := append(append([]llm.Message{}, prefix.Messages...), llm.MessagesForAPI(msgs)...)
	if len(req.Messages) != len(want)+1 {
		t.Fatalf("%d messages, want lead + conversation + instruction = %d", len(req.Messages), len(want)+1)
	}
	if !reflect.DeepEqual(req.Messages[:len(want)], want) {
		t.Fatal("the summary request does not start with the run's messages")
	}
	if last := req.Messages[len(req.Messages)-1]; last.Role != llm.RoleUser || !strings.Contains(last.Text(), "Do NOT call any tools") {
		t.Fatalf("last message is not the summary instruction: %q", last.Text())
	}
	if req.MaxTokens != compactMaxOutputTokens {
		t.Fatalf("max tokens %d", req.MaxTokens)
	}
	if got := c.TakeUsage(); got != p.usage {
		t.Fatalf("usage %+v, want %+v", got, p.usage)
	}
	if got := c.TakeUsage(); got != (llm.Usage{}) {
		t.Fatalf("TakeUsage did not reset: %+v", got)
	}
	// The summary replaced the head and kept the tail, as before.
	if view := llm.MessagesForAPI(out); !strings.Contains(view[0].Text(), "Summary:\nS") || len(view) != 1+smallWindow().KeepRecent {
		t.Fatalf("unexpected view after compaction: %d messages, first %q", len(view), view[0].Text())
	}
}

// After a context overflow the whole conversation cannot be sent again, so the
// summary covers only the part it replaces — still on the run's tools and
// system, which stay readable from the cache.
func TestSummaryAfterOverflowSendsOnlyTheHead(t *testing.T) {
	p := &usageProvider{replies: []string{"<summary>S</summary>"}}
	c := New(smallWindow(), ProviderSummarizer(p, 0, false))
	c.SetRequestPrefix(sessionPrefix())
	msgs := longConversation()
	if _, ok := c.Reactive(context.Background(), msgs); !ok {
		t.Fatal("reactive compaction did not run")
	}
	req := p.reqs[0]
	head := len(msgs) - smallWindow().KeepRecent
	if len(req.Messages) != 1+head+1 {
		t.Fatalf("%d messages, want lead + %d head messages + instruction", len(req.Messages), head)
	}
	if len(req.Tools) != 1 {
		t.Fatal("the run's tools were dropped")
	}
}

// A separate summarizer model shares nothing with the run: its own system
// prompt, no tools, only the part being summarized, and no cache markers,
// since no later request would read what they wrote. Its usage is still
// reported.
func TestStandaloneSummarizerSharesNothing(t *testing.T) {
	cheap := &usageProvider{replies: []string{"<summary>S</summary>"}, usage: llm.Usage{InputTokens: 500, OutputTokens: 30}}
	cfg := smallWindow()
	cfg.Summarizer = StandaloneSummarizer(cheap, 0, false)
	session := &usageProvider{replies: []string{"unused"}}
	c := New(cfg, ProviderSummarizer(session, 0, false))
	c.SetRequestPrefix(sessionPrefix())
	msgs := longConversation()
	if _, ok := c.autoCompact(context.Background(), msgs, 9000, "auto"); !ok {
		t.Fatal("autoCompact did not run")
	}
	if len(session.reqs) != 0 {
		t.Fatal("Config.Summarizer must replace the session summarizer")
	}
	req := cheap.reqs[0]
	if len(req.System) != 1 || req.System[0] != summarySystemPrompt || len(req.Tools) != 0 {
		t.Fatalf("standalone request carries the run's prefix: system=%v tools=%v", req.System, req.Tools)
	}
	if !req.DisableCache {
		t.Fatal("standalone summary must not send cache markers")
	}
	if got := llm.SettingsOf(req); got != (llm.ModelSettings{DisableCache: true}) {
		t.Fatalf("standalone request carries session settings: %+v", got)
	}
	if head := len(msgs) - cfg.KeepRecent; len(req.Messages) != head+1 {
		t.Fatalf("%d messages, want the %d being summarized + instruction", len(req.Messages), head)
	}
	if got := c.TakeUsage(); got != cheap.usage {
		t.Fatalf("usage %+v, want %+v", got, cheap.usage)
	}
}

// A compactor nobody told the prefix to (driven without the loop) still
// summarizes, with the standalone request.
func TestSummaryWithoutPrefixFallsBack(t *testing.T) {
	p := &usageProvider{replies: []string{"<summary>S</summary>"}}
	c := New(smallWindow(), ProviderSummarizer(p, 0, false))
	if _, ok := c.autoCompact(context.Background(), longConversation(), 9000, "auto"); !ok {
		t.Fatal("autoCompact did not run")
	}
	if req := p.reqs[0]; len(req.Tools) != 0 || req.System[0] != summarySystemPrompt || !req.DisableCache {
		t.Fatalf("unexpected fallback request: %+v", req)
	}
}

// The run's tools are listed in the summary request to keep the prefix
// identical. A response that calls one instead of answering has no summary in
// it: it is retried, and never used as an empty summary.
func TestSummaryWithNoTextIsRetriedNotUsed(t *testing.T) {
	p := &usageProvider{replies: []string{"", "<summary>S</summary>"}, usage: llm.Usage{InputTokens: 10}}
	c := New(smallWindow(), ProviderSummarizer(p, 0, false))
	c.SetRequestPrefix(sessionPrefix())
	if _, ok := c.autoCompact(context.Background(), longConversation(), 9000, "auto"); !ok {
		t.Fatal("a second attempt should have produced the summary")
	}
	if len(p.reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(p.reqs))
	}
	if got := c.TakeUsage().InputTokens; got != 20 {
		t.Fatalf("usage counts %d input tokens, want both attempts (20)", got)
	}

	never := &usageProvider{replies: []string{""}}
	c = New(smallWindow(), ProviderSummarizer(never, 0, false))
	msgs := longConversation()
	out, ok := c.autoCompact(context.Background(), msgs, 9000, "auto")
	if ok || len(out) != len(msgs) {
		t.Fatal("a summary with no text must leave the history alone")
	}
	if _, err := ProviderSummarizer(never, 0, false)(context.Background(), msgs); !errors.Is(err, errNoSummaryText) {
		t.Fatalf("err=%v, want errNoSummaryText", err)
	}
}
