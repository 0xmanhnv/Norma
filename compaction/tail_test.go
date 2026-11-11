package compaction

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
)

func toolUseMsg(ids ...string) llm.Message {
	m := llm.Message{Role: llm.RoleAssistant}
	for _, id := range ids {
		m.Content = append(m.Content, llm.ContentBlock{Type: llm.BlockToolUse, ID: id, Name: "Lookup", Input: []byte(`{}`)})
	}
	return m
}

func toolResultMsg(body string, ids ...string) llm.Message {
	m := llm.Message{Role: llm.RoleUser}
	for _, id := range ids {
		m.Content = append(m.Content, llm.ToolResultText(id, body+" for "+id, false))
	}
	return m
}

// toolLoopHistory is a user request followed by n tool turns of size chars each.
func toolLoopHistory(n, size int) []llm.Message {
	msgs := []llm.Message{llm.UserText("the task")}
	for i := range n {
		id := "c" + strconv.Itoa(i)
		msgs = append(msgs, toolUseMsg(id), toolResultMsg(strings.Repeat("r", size), id))
	}
	return msgs
}

// toolIDs returns the tool_use and tool_result ids found in msgs.
func toolIDs(msgs []llm.Message) (uses, results map[string]bool) {
	uses, results = map[string]bool{}, map[string]bool{}
	for _, m := range msgs {
		for _, b := range m.Content {
			switch b.Type {
			case llm.BlockToolUse:
				uses[b.ID] = true
			case llm.BlockToolResult:
				results[b.ToolUseID] = true
			}
		}
	}
	return
}

// assertExchangesWhole checks that no tool exchange was lost or split: every
// call of the history is, with its result, either in what was summarized or in
// the view kept after the boundary.
func assertExchangesWhole(t *testing.T, history, summarized, view []llm.Message) {
	t.Helper()
	all, _ := toolIDs(history)
	sumUses, sumResults := toolIDs(summarized)
	viewUses, viewResults := toolIDs(view)
	for id := range all {
		inSummary := sumUses[id] && sumResults[id]
		inView := viewUses[id] && viewResults[id]
		if inSummary == inView {
			t.Errorf("exchange %s: summarized(use=%v result=%v) kept(use=%v result=%v); want it whole on exactly one side",
				id, sumUses[id], sumResults[id], viewUses[id], viewResults[id])
		}
	}
}

func assertAlternates(t *testing.T, view []llm.Message) {
	t.Helper()
	for i := 1; i < len(view); i++ {
		if view[i].Role == view[i-1].Role {
			t.Fatalf("messages %d and %d after the boundary are both %s", i-1, i, view[i].Role)
		}
	}
	if len(view) == 0 || view[0].Role != llm.RoleUser {
		t.Fatal("the view must start with the summary as a user message")
	}
}

// Cutting the tail by message count could start it on a tool_result. The view
// then dropped that result as unpaired, and the summary input dropped its
// call: the exchange vanished from both. With every count from 1 to 9 the tail
// now starts on an assistant message and every exchange stays whole.
func TestTailNeverStartsOnAToolResult(t *testing.T) {
	history := toolLoopHistory(8, 400)
	for keep := 1; keep <= 9; keep++ {
		rs := &recSummarizer{}
		c := New(Config{ContextWindow: 200000, KeepRecent: keep}, rs.fn)
		out, ok := c.autoCompact(context.Background(), history, 180000, "auto")
		if !ok {
			t.Fatalf("keep=%d: did not compact (%s)", keep, c.lastReason)
		}
		view := llm.MessagesForAPI(out)
		if !reflect.DeepEqual(llm.PairToolBlocks(view), view) {
			t.Fatalf("keep=%d: the view has an unpaired tool block", keep)
		}
		tail := view[1:]
		if len(tail) == 0 || tail[0].Role != llm.RoleAssistant {
			t.Fatalf("keep=%d: the tail does not start on an assistant message: %+v", keep, tail)
		}
		if len(tail) > keep && keep >= 2 {
			t.Fatalf("keep=%d: %d messages kept", keep, len(tail))
		}
		// Nothing was lost between the two sides: summarized + kept is the
		// whole history.
		if len(rs.last)+len(tail) != len(history) {
			t.Fatalf("keep=%d: %d summarized + %d kept != %d messages", keep, len(rs.last), len(tail), len(history))
		}
		assertExchangesWhole(t, history, rs.last, view)
		assertAlternates(t, view)
	}
}

// Parallel calls answered across several messages are one exchange: the cut
// never falls between a call and any of its results.
func TestTailKeepsParallelCallsTogether(t *testing.T) {
	history := []llm.Message{
		bigMsg(llm.RoleUser, 600),
		toolUseMsg("a"), toolResultMsg(strings.Repeat("out ", 100), "a"),
		toolUseMsg("p", "q", "r"),
		toolResultMsg(strings.Repeat("out ", 100), "p"),
		toolResultMsg(strings.Repeat("out ", 100), "q", "r"), // the results arrive in two messages
		toolUseMsg("z"), toolResultMsg(strings.Repeat("out ", 100), "z"),
	}
	for keep := 1; keep <= 7; keep++ {
		rs := &recSummarizer{}
		c := New(Config{ContextWindow: 200000, KeepRecent: keep}, rs.fn)
		out, ok := c.autoCompact(context.Background(), history, 180000, "auto")
		if !ok {
			t.Fatalf("keep=%d: did not compact (%s)", keep, c.lastReason)
		}
		view := llm.MessagesForAPI(out)
		assertExchangesWhole(t, history, rs.last, view)
		if !reflect.DeepEqual(llm.PairToolBlocks(view), view) {
			t.Fatalf("keep=%d: unpaired block in the view", keep)
		}
	}
}

// The summarizer that works on the summarized part alone (a separate model)
// gets every call together with its result; nothing is dropped as dangling.
func TestStandaloneSummaryInputKeepsExchanges(t *testing.T) {
	history := toolLoopHistory(8, 400)
	p := &usageProvider{replies: []string{"<summary>S</summary>"}}
	c := New(Config{ContextWindow: 200000, KeepRecent: 4, Summarizer: StandaloneSummarizer(p, 0, false)}, nil)
	out, ok := c.autoCompact(context.Background(), history, 180000, "auto")
	if !ok {
		t.Fatalf("did not compact (%s)", c.lastReason)
	}
	sent := p.reqs[0].Messages
	sent = sent[:len(sent)-1] // without the instruction
	assertExchangesWhole(t, history, sent, llm.MessagesForAPI(out))
}

// The tail is a token budget. Eight messages used to be kept whatever their
// size, so a handful of huge tool results after a boundary left "nothing to
// summarize" while the context overflowed.
func TestTailIsATokenBudget(t *testing.T) {
	// 3 tool turns of ~40000 tokens each: 7 messages, under the old count of 8.
	history := toolLoopHistory(3, 120000)
	rs := &recSummarizer{}
	c := New(Config{ContextWindow: 200000}, rs.fn) // tail budget: a fifth of the window
	out, ok := c.autoCompact(context.Background(), history, 180000, "auto")
	if !ok {
		t.Fatalf("large messages within the count cap must still compact (%s)", c.lastReason)
	}
	view := llm.MessagesForAPI(out)
	if got := c.estimate(view[1:]); got > c.tailHardLimit() {
		t.Fatalf("kept tail weighs %d tokens", got)
	}
	// The most recent turn is kept even though it alone is over the budget.
	if len(view) != 3 || view[1].Role != llm.RoleAssistant {
		t.Fatalf("want summary + the last tool turn, got %d messages", len(view))
	}
	assertExchangesWhole(t, history, rs.last, view)

	// Small messages: the budget keeps several turns, capped by KeepRecent.
	history = toolLoopHistory(20, 400)
	c = New(Config{ContextWindow: 200000, KeepRecent: 6}, rs.fn)
	out, _ = c.autoCompact(context.Background(), history, 180000, "auto")
	if got := len(llm.MessagesForAPI(out)) - 1; got != 6 {
		t.Fatalf("%d messages kept, want 6", got)
	}
	// An explicit token budget cuts the tail shorter than the cap.
	c = New(Config{ContextWindow: 200000, KeepRecent: 6, KeepRecentTokens: 200}, rs.fn)
	out, _ = c.autoCompact(context.Background(), history, 180000, "auto")
	if got := len(llm.MessagesForAPI(out)) - 1; got != 2 {
		t.Fatalf("%d messages kept under a 200-token budget, want one turn (2)", got)
	}
}

// When the turn that has to be kept is larger than anything a summary could
// make room for, compaction says so and makes no model call — on this turn or
// the next ones.
func TestOneMessageOverTheBudgetIsReportedNotRetried(t *testing.T) {
	history := toolLoopHistory(4, 400)
	history = append(history, toolUseMsg("big"), toolResultMsg(strings.Repeat("r", 600000), "big")) // ~200000 tokens
	rs := &recSummarizer{}
	c := New(Config{ContextWindow: 200000}, rs.fn)
	for range 3 {
		out, ok := c.autoCompact(context.Background(), history, 190000, "auto")
		if ok || len(out) != len(history) {
			t.Fatal("compacted although the kept turn alone exceeds the budget")
		}
		if c.lastReason != ReasonMessageExceedsBudget {
			t.Fatalf("reason %q, want %q", c.lastReason, ReasonMessageExceedsBudget)
		}
	}
	if rs.calls != 0 {
		t.Fatalf("%d summary calls for a compaction that cannot help", rs.calls)
	}
	if c.tripped || c.failures != 0 {
		t.Fatal("an impossible compaction must not count against the summarizer")
	}
	// Once the large result is behind more recent turns it can be summarized.
	history = append(history, toolLoopHistory(2, 400)[1:]...)
	if _, ok := c.autoCompact(context.Background(), history, 190000, "auto"); !ok || rs.calls != 1 {
		t.Fatalf("should compact once the large message is no longer the latest turn (ok=%v calls=%d reason=%s)", ok, rs.calls, c.lastReason)
	}
}

// Everything since the boundary fits in the tail: nothing to summarize.
func TestTailCoversEverything(t *testing.T) {
	rs := &recSummarizer{}
	c := New(Config{ContextWindow: 200000}, rs.fn)
	if _, ok := c.autoCompact(context.Background(), toolLoopHistory(2, 400), 180000, "auto"); ok || rs.calls != 0 {
		t.Fatal("a conversation that fits in the tail must not be summarized")
	}
	if c.lastReason != ReasonNothingToSummarize {
		t.Fatalf("reason %q", c.lastReason)
	}
}
