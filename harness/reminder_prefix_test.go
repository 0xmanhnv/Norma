package harness

import (
	"bytes"
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/tool"
)

// cachedPrefix is what a request asks the provider to cache of the
// conversation: the rendered request up to the end of the last block that
// carries a cache marker. The next request reads that entry only if it starts
// with exactly these bytes.
func (r wireRequest) cachedPrefix(t *testing.T) []byte {
	t.Helper()
	const marker = `,"cache_control":{`
	i := bytes.LastIndex(r.Messages, []byte(marker))
	if i < 0 {
		t.Fatalf("request marks no conversation breakpoint: %s", tailOf(r.Messages))
	}
	var out []byte
	out = append(out, unmarked(r.Tools)...)
	out = append(out, unmarked(r.System)...)
	return append(out, unmarked(r.Messages[:i])...)
}

// The system reminder changes from one prompt to the next (a new date, new
// host state). It rides at the end of the request, after the cache
// breakpoint, so a changed reminder costs nothing: across two prompts of one
// conversation with different reminders, every request still starts with the
// bytes the previous one asked to have cached. In front of the conversation,
// the second prompt's first request would share nothing with the cache.
func TestSystemReminderDoesNotBreakTheCachedPrefix(t *testing.T) {
	const turns = 4
	f := newFakeAnthropic(t, "Lookup", turns)
	tools := tool.NewRegistry(bulkTool("Lookup", 500))
	frames := 0
	in := QueryInput{
		Provider:       f.provider(t),
		Tools:          tools,
		SystemReminder: "<system-reminder>\nday one\n</system-reminder>",
		WorkingFrame: func(context.Context) string {
			frames++
			return "frame " + strings.Repeat("#", frames)
		},
	}
	first := runLoop(t, in)

	f.mu.Lock()
	f.turns += turns // the second prompt gets tool turns too (one fewer: the first prompt's answer counted)
	f.mu.Unlock()
	in.Messages = append(append([]llm.Message{}, first.Messages...), llm.UserText("and the rest"))
	in.SystemReminder = "<system-reminder>\nday two, with more to say\n</system-reminder>"
	runLoop(t, in)

	reqs := f.requests()
	if len(reqs) != 2*turns+1 {
		t.Fatalf("%d requests, want %d", len(reqs), 2*turns+1)
	}
	for i, r := range reqs {
		reminder := "day one"
		if i > turns {
			reminder = "day two"
		}
		if n := bytes.Count(r.Messages, []byte(reminder)); n != 1 {
			t.Fatalf("request %d carries the reminder %d times, want once: %s", i, n, tailOf(r.Messages))
		}
		// The reminder and then the frame close the request, in the last message.
		tail := string(r.Messages[bytes.LastIndex(r.Messages, []byte(`{"role":`)):])
		ri, fi := strings.Index(tail, reminder), strings.Index(tail, "frame #")
		if ri < 0 || fi < ri {
			t.Fatalf("request %d: reminder and frame are not the closing blocks of the last message: %s", i, tail)
		}
		if strings.Contains(string(r.cachedPrefix(t)), "system-reminder") || strings.Contains(string(r.cachedPrefix(t)), "frame #") {
			t.Fatalf("request %d: the cached prefix includes the reminder or the frame", i)
		}
		if i == 0 {
			continue
		}
		if prev := reqs[i-1].cachedPrefix(t); !bytes.HasPrefix(r.rendered(), prev) {
			t.Fatalf("request %d does not start with what request %d cached:\ncached: %s\nnext:   %s",
				i, i-1, tailOf(prev), tailOf(r.rendered()[:min(len(prev), len(r.rendered()))]))
		}
	}
}

// The reminder is a request-only block: it never enters the history.
func TestSystemReminderStaysOutOfHistory(t *testing.T) {
	m := &scriptedModel{turns: [][]llm.StreamEvent{textTurn("ok")}}
	var sent []llm.Message
	call := func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		sent = req.Messages
		if req.VolatileTail != 1 {
			t.Errorf("VolatileTail = %d, want 1 (the reminder)", req.VolatileTail)
		}
		return m.call(ctx, req)
	}
	term := drain(t, QueryInput{Messages: []llm.Message{llm.UserText("hi")}, SystemReminder: "REMINDER"},
		QueryDeps{CallModel: call})
	if len(sent) != 1 || len(sent[0].Content) != 2 || sent[0].Content[0].Text != "hi" || sent[0].Content[1].Text != "REMINDER" {
		t.Fatalf("request messages = %+v, want one user message [hi, REMINDER]", sent)
	}
	for _, msg := range term.Messages {
		for _, b := range msg.Content {
			if strings.Contains(b.Text, "REMINDER") {
				t.Fatalf("the reminder entered the history: %+v", term.Messages)
			}
		}
	}
}
