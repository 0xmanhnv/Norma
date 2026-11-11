package compaction

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/0xmanhnv/corvid/llm"
)

type eventLog struct{ events []Event }

func (l *eventLog) on(e Event) { l.events = append(l.events, e) }

func (l *eventLog) last(t *testing.T) Event {
	t.Helper()
	if len(l.events) == 0 {
		t.Fatal("no event was reported")
	}
	return l.events[len(l.events)-1]
}

func alternating(n, size int) []llm.Message {
	var msgs []llm.Message
	for i := range n {
		role := llm.RoleUser
		if i%2 == 1 {
			role = llm.RoleAssistant
		}
		msgs = append(msgs, bigMsg(role, size))
	}
	return msgs
}

// Pre used to accept whatever the summarizer returned. A summary no smaller
// than what it replaces leaves the context as full as before, behind a new
// boundary, and the next turn summarizes again. It is now thrown away, the
// history is left untouched, and Reactive does not pay for a second summary on
// the same turn.
func TestPreRejectsASummaryThatDoesNotShrinkTheView(t *testing.T) {
	log := &eventLog{}
	rs := &recSummarizer{reply: strings.Repeat("z", 400000)}
	c := New(Config{ContextWindow: 200000, KeepRecent: 2, OnEvent: log.on}, rs.fn)
	msgs := alternating(11, 4000)

	out := c.Pre(context.Background(), msgs, 185000)
	if boundaryCount(out) != 0 || len(out) != len(msgs) {
		t.Fatalf("a summary that does not shrink the view was accepted (%d boundaries)", boundaryCount(out))
	}
	if rs.calls != 1 {
		t.Fatalf("calls=%d", rs.calls)
	}
	ev := log.last(t)
	if ev.Applied || ev.Reason != ReasonSummaryNotSmaller || ev.Kind != KindSummary || ev.Trigger != "auto" {
		t.Fatalf("event %+v", ev)
	}
	if ev.TokensBefore != 185000 || ev.TokensAfter != 185000 {
		t.Fatalf("a rejected pass must report unchanged tokens: %+v", ev)
	}

	// Same turn: the request overflows and the loop asks for a reactive pass.
	out, ok := c.Reactive(context.Background(), msgs)
	if ok || rs.calls != 1 {
		t.Fatalf("a second summary was made on the same turn (ok=%v calls=%d)", ok, rs.calls)
	}
	if ev := log.last(t); ev.Reason != ReasonSummaryRejected || ev.Trigger != "reactive" {
		t.Fatalf("event %+v", ev)
	}
	if len(out) != len(msgs) {
		t.Fatal("history changed")
	}

	// The next turn may try again.
	rs.reply = "short"
	out = c.Pre(context.Background(), msgs, 185000)
	if boundaryCount(out) != 1 || rs.calls != 2 {
		t.Fatalf("next turn: boundaries=%d calls=%d", boundaryCount(out), rs.calls)
	}
	if ev := log.last(t); !ev.Applied || ev.TokensAfter >= ev.TokensBefore || ev.MessagesSummarized == 0 {
		t.Fatalf("applied pass must report a smaller context and what it summarized: %+v", ev)
	}
}

// Every pass is reported the same way, whether Pre decided it or Reactive was
// asked for it, with tokens before and after.
func TestEveryPassIsReported(t *testing.T) {
	log := &eventLog{}
	rs := &recSummarizer{}
	c := New(Config{ContextWindow: 100000, MaxOutputTokens: 4000, KeepRecent: 2, OnEvent: log.on}, rs.fn)

	// Below every threshold: nothing happens and nothing is reported.
	c.Pre(context.Background(), alternating(11, 4000), 1000)
	if len(log.events) != 0 {
		t.Fatalf("event for a turn with nothing to do: %+v", log.events)
	}

	// A MicroCompact pass made by Pre.
	var reads []llm.Message
	for i := range 20 {
		reads = append(reads, readPair("r"+string(rune('a'+i)), 6000)...)
	}
	c.Pre(context.Background(), reads, 60000)
	ev := log.last(t)
	if ev.Kind != KindMicro || !ev.Applied || ev.ResultsCleared != 15 || ev.TokensBefore != 60000 || ev.TokensAfter >= ev.TokensBefore {
		t.Fatalf("micro event %+v", ev)
	}

	// A summary made by Pre.
	c.Pre(context.Background(), alternating(11, 4000), 90000)
	ev = log.last(t)
	if ev.Kind != KindSummary || ev.Trigger != "auto" || !ev.Applied || ev.TokensBefore != 90000 || ev.TokensAfter >= 90000 {
		t.Fatalf("auto summary event %+v", ev)
	}

	// A summary asked for after an overflow.
	if _, ok := c.Reactive(context.Background(), alternating(11, 4000)); !ok {
		t.Fatal("reactive pass did not run")
	}
	ev = log.last(t)
	if ev.Kind != KindSummary || ev.Trigger != "reactive" || !ev.Applied || ev.TokensAfter >= ev.TokensBefore {
		t.Fatalf("reactive summary event %+v", ev)
	}
}

// A pass that was due and did not happen says why.
func TestNoOpReasonsAreReported(t *testing.T) {
	msgs := alternating(11, 4000)
	boom := errors.New("boom")
	cases := []struct {
		name  string
		setup func(c *Compactor)
		rs    *recSummarizer
		want  Reason
	}{
		{"no summarizer", nil, nil, ReasonNoSummarizer},
		{"summarizer error", nil, &recSummarizer{err: boom}, ReasonSummaryFailed},
		{"breaker open", func(c *Compactor) { c.viewTokens = c.estimate(msgs); c.trip() }, &recSummarizer{}, ReasonBreakerOpen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &eventLog{}
			var s Summarizer
			if tc.rs != nil {
				s = tc.rs.fn
			}
			c := New(Config{ContextWindow: 200000, KeepRecent: 2, OnEvent: log.on}, s)
			if tc.setup != nil {
				tc.setup(c)
			}
			c.Pre(context.Background(), msgs, 185000)
			ev := log.last(t)
			if ev.Applied || ev.Reason != tc.want {
				t.Fatalf("event %+v, want reason %q", ev, tc.want)
			}
			if tc.want == ReasonSummaryFailed && !errors.Is(ev.Err, boom) {
				t.Fatalf("the summarizer's error is not on the event: %v", ev.Err)
			}
		})
	}

	// Nothing to summarize, and one message over the budget.
	log := &eventLog{}
	c := New(Config{ContextWindow: 200000, OnEvent: log.on}, (&recSummarizer{}).fn)
	c.Pre(context.Background(), toolLoopHistory(2, 400), 185000)
	if ev := log.last(t); ev.Reason != ReasonNothingToSummarize {
		t.Fatalf("event %+v", ev)
	}
	huge := append(toolLoopHistory(2, 400), toolUseMsg("big"), toolResultMsg(strings.Repeat("r", 600000), "big"))
	c.Pre(context.Background(), huge, 185000)
	if ev := log.last(t); ev.Reason != ReasonMessageExceedsBudget {
		t.Fatalf("event %+v", ev)
	}
}

// A tripped breaker also reopens when the conversation has grown by
// BreakerRegrowth since it tripped, without waiting for the cool-down: the
// summary that failed was over a different, shorter conversation.
func TestBreakerReopensAfterGrowth(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	rs := &recSummarizer{err: errors.New("boom")}
	c := New(Config{ContextWindow: 200000, KeepRecent: 2, BreakerRegrowth: 5000}, rs.fn)
	c.now = func() time.Time { return clock }
	msgs := alternating(11, 4000)
	for range maxAutoCompactFails {
		c.autoCompact(context.Background(), msgs, 185000, "auto")
	}
	if !c.tripped {
		t.Fatal("breaker should be open")
	}
	// A little growth: still open.
	msgs = append(msgs, alternating(2, 4000)...)
	c.autoCompact(context.Background(), msgs, 185000, "auto")
	if rs.calls != maxAutoCompactFails || c.lastReason != ReasonBreakerOpen {
		t.Fatalf("reopened after too little growth (calls=%d reason=%s)", rs.calls, c.lastReason)
	}
	// 5000 tokens more than when it tripped: one probe, with no time passed.
	msgs = append(msgs, alternating(4, 4000)...)
	rs.err = nil
	if _, ok := c.autoCompact(context.Background(), msgs, 185000, "auto"); !ok {
		t.Fatalf("growth should have reopened the breaker (%s)", c.lastReason)
	}
	if c.tripped {
		t.Fatal("a successful probe must close the breaker")
	}
}
