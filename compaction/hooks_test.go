package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/tool"
)

// AfterCompact's messages land right after the summary, ahead of the kept
// tail, in the view the next request sends. The hook is shown the summary and
// what it replaced. Messages that are not user messages are dropped: the view
// must go on alternating.
func TestAfterCompactInjectsRightAfterTheSummary(t *testing.T) {
	var seen Compacted
	cfg := smallWindow()
	cfg.AfterCompact = func(_ context.Context, c Compacted) []llm.Message {
		seen = c
		return []llm.Message{
			llm.UserText("RECOVERY NOTE: the full log is in run.log"),
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("not a user message")}},
			llm.TodoReminderMessage("1. [in_progress] write the report"),
		}
	}
	c := New(cfg, (&recSummarizer{}).fn)
	msgs := longConversation()
	out, ok := c.autoCompact(context.Background(), msgs, 9000, "auto")
	if !ok {
		t.Fatalf("autoCompact did not run: %s", c.lastReason)
	}
	view := llm.MessagesForAPI(out)
	if len(view) < 4 {
		t.Fatalf("view has %d messages", len(view))
	}
	// Anthropic-style merging aside, the view is: summary, note, reminder, tail…
	if !strings.Contains(view[0].Text(), "DIGEST") {
		t.Fatalf("view does not start with the summary: %q", view[0].Text())
	}
	if view[1].Text() != "RECOVERY NOTE: the full log is in run.log" {
		t.Fatalf("message after the summary = %q, want the recovery note", view[1].Text())
	}
	if !llm.IsTodoReminder(view[2]) {
		t.Fatalf("third message is not the injected todo list: %q", view[2].Text())
	}
	if view[3].Role != llm.RoleAssistant {
		t.Fatalf("the kept tail does not follow the injected messages: %+v", view[3])
	}
	for _, m := range view {
		if m.Text() == "not a user message" {
			t.Fatal("a non-user message was injected")
		}
	}
	if seen.Trigger != "auto" || seen.PreTokens != 9000 || !strings.Contains(seen.Summary, "DIGEST") ||
		seen.MessagesSummarized != len(seen.Summarized) || len(seen.Summarized) == 0 || len(seen.Tail) == 0 {
		t.Fatalf("hook was shown %+v", seen)
	}
	// Nothing was lost: the history before the boundary is intact.
	if len(out) != len(msgs)+4 {
		t.Fatalf("%d messages after compaction, want the %d before plus boundary, summary and two injected", len(out), len(msgs))
	}
}

// What the hook injects counts toward the new view: a summary whose view is
// not smaller than the one it replaces is thrown away, injected messages and all.
func TestAfterCompactCannotGrowTheView(t *testing.T) {
	cfg := smallWindow()
	cfg.AfterCompact = func(context.Context, Compacted) []llm.Message {
		return []llm.Message{bigMsg(llm.RoleUser, 60000)}
	}
	c := New(cfg, (&recSummarizer{}).fn)
	msgs := longConversation()
	out, ok := c.autoCompact(context.Background(), msgs, 9000, "auto")
	if ok || len(out) != len(msgs) || c.lastReason != ReasonSummaryNotSmaller {
		t.Fatalf("applied=%v reason=%q, want the pass rejected as not smaller", ok, c.lastReason)
	}
}

// The default stub keeps the way back to a cleared result: the first line,
// where a tool names what it returned, and the pointer to the full copy when
// the output had been spilled.
func TestDefaultMicroStubKeepsLocators(t *testing.T) {
	spilled := "HTTP/1.1 200 OK report-7f3a\nbody line\n" + strings.Repeat("x", 4000) +
		"\n\n... <persisted-output>[Output too large: full 90000 bytes / 1200 lines. Full output saved to  /tmp/out/7f3a.txt </persisted-output>"
	cases := []struct {
		name string
		text string
		want string
	}{
		{"first line", "record id=rec_81 stored\nsecond line\nthird",
			"[Old tool result content cleared]\nrecord id=rec_81 stored"},
		{"spill pointer", spilled,
			"[Old tool result content cleared]\nHTTP/1.1 200 OK report-7f3a\n<persisted-output>[Output too large: full 90000 bytes / 1200 lines. Full output saved to  /tmp/out/7f3a.txt </persisted-output>"},
		{"long first line is cut on a rune boundary", strings.Repeat("é", 200) + "\nrest",
			"[Old tool result content cleared]\n" + strings.Repeat("é", 60) + "…"},
		{"no text", "", "[Old tool result content cleared]"},
		{"already a stub", "[Old tool result content cleared]\nrecord id=rec_81 stored",
			"[Old tool result content cleared]\nrecord id=rec_81 stored"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DefaultMicroStub(llm.ToolResultText("id", c.text, false)); got != c.want {
				t.Fatalf("stub =\n%q\nwant\n%q", got, c.want)
			}
		})
	}
	if ptr, ok := tool.SpillPointer(spilled); !ok || !strings.Contains(ptr, "/tmp/out/7f3a.txt") {
		t.Fatalf("SpillPointer = %q, %v", ptr, ok)
	}
}

// MicroCompact leaves the default stub — not the bare notice — in place of a
// cleared result, so a locator in it survives the pass; a second pass changes
// nothing.
func TestMicroCompactStubPreservesLocator(t *testing.T) {
	c := New(Config{ContextWindow: 200000}, nil)
	var msgs []llm.Message
	for i := range microKeepRecent + 2 {
		id := "r" + string(rune('a'+i))
		msgs = append(msgs, toolPair("Read", id, "locator:/data/"+id+".json\n"+strings.Repeat("y", 800))...)
	}
	out, changed := c.microCompact(msgs)
	if !changed {
		t.Fatal("nothing was cleared")
	}
	if got, want := resultText(out[1].Content[0]), "[Old tool result content cleared]\nlocator:/data/ra.json"; got != want {
		t.Fatalf("cleared result = %q, want %q", got, want)
	}
	if got := resultText(out[len(out)-1].Content[0]); !strings.Contains(got, "yyyy") {
		t.Fatalf("a recent result was cleared: %q", got)
	}
	if _, again := c.microCompact(out); again {
		t.Fatal("a second pass changed the history again")
	}
	if c.microFreeable(out) != 0 {
		t.Fatal("cleared results still count as freeable")
	}
}

// Config.MicroStub writes the stub. It is recognised as cleared even though it
// does not look like the default, an empty return falls back to the default,
// and a stub that is not shorter than the result is not applied.
func TestMicroCompactCustomStub(t *testing.T) {
	calls := 0
	c := New(Config{ContextWindow: 200000, MicroStub: func(result llm.ContentBlock) string {
		calls++
		text := resultText(result)
		switch {
		case strings.HasPrefix(text, "ref="):
			ref, _, _ := strings.Cut(text, "\n")
			return "(dropped; " + ref + ")"
		case strings.HasPrefix(text, "tiny"):
			return "a stub far longer than the result it would replace"
		}
		return ""
	}}, nil)
	msgs := append(toolPair("Read", "a", "ref=obj-1\n"+strings.Repeat("y", 500)),
		append(toolPair("Read", "b", "tiny"), toolPair("Read", "c", "plain first line\n"+strings.Repeat("y", 500))...)...)
	for i := range microKeepRecent {
		msgs = append(msgs, toolPair("Read", "k"+string(rune('0'+i)), strings.Repeat("z", 300))...)
	}
	out, changed := c.microCompact(msgs)
	if !changed {
		t.Fatal("nothing was cleared")
	}
	if got := resultText(out[1].Content[0]); got != "(dropped; ref=obj-1)" {
		t.Fatalf("custom stub = %q", got)
	}
	if got := resultText(out[3].Content[0]); got != "tiny" {
		t.Fatalf("a stub longer than the result was applied: %q", got)
	}
	if got := resultText(out[5].Content[0]); got != "[Old tool result content cleared]\nplain first line" {
		t.Fatalf("empty custom stub did not fall back to the default: %q", got)
	}
	if n := c.clearedResults(out); n != 2 {
		t.Fatalf("clearedResults = %d, want 2", n)
	}
	before := calls
	if _, again := c.microCompact(out); again {
		t.Fatal("a second pass changed the history again")
	}
	if calls-before != 1 { // only the result that was left alone is looked at again
		t.Fatalf("the stub function ran %d more times on a history already cleared, want 1", calls-before)
	}
}

// Config.SummaryPrompt replaces the instruction of the summary request, and
// Config.References adds the request to list the required identifiers.
func TestSummaryPromptOverrideAndReferencesRequest(t *testing.T) {
	p := &usageProvider{replies: []string{"<summary>kept obj-1 and run.log</summary>"}}
	cfg := smallWindow()
	cfg.SummaryPrompt = "Summarize for a hand-over. Plain text, no tool calls."
	var shown []llm.Message
	cfg.References = func(summarized []llm.Message) []string {
		shown = summarized
		return []string{"obj-1", " run.log ", "", "obj-1"}
	}
	c := New(cfg, ProviderSummarizer(p, 0, false))
	c.SetRequestPrefix(llm.CompletionRequest{System: []string{"sys"}})
	if _, ok := c.autoCompact(context.Background(), longConversation(), 9000, "auto"); !ok {
		t.Fatalf("autoCompact did not run: %s", c.lastReason)
	}
	last := p.reqs[0].Messages[len(p.reqs[0].Messages)-1].Text()
	if !strings.HasPrefix(last, "Summarize for a hand-over. Plain text, no tool calls.") {
		t.Fatalf("instruction does not start with the override: %q", last)
	}
	if strings.Contains(last, "Your task is to create a detailed summary") {
		t.Fatal("the built-in instruction was sent as well")
	}
	if !strings.HasSuffix(last, "character for character:\n- obj-1\n- run.log") || !strings.Contains(last, `titled "References"`) {
		t.Fatalf("instruction does not ask for the references: %q", last)
	}
	if len(shown) == 0 {
		t.Fatal("References was not shown the messages being summarized")
	}

	// Without an override the built-in instruction is sent, unchanged.
	p2 := &usageProvider{replies: []string{"<summary>S</summary>"}}
	c2 := New(smallWindow(), ProviderSummarizer(p2, 0, false))
	if _, ok := c2.autoCompact(context.Background(), longConversation(), 9000, "auto"); !ok {
		t.Fatal("autoCompact did not run")
	}
	if got := p2.reqs[0].Messages[len(p2.reqs[0].Messages)-1].Text(); got != DefaultSummaryPrompt() {
		t.Fatal("the default instruction changed")
	}
}

// Whatever the model wrote, every required identifier is in the summary that
// is inserted: the compactor appends the ones it left out, in the order given,
// and reports how many.
func TestRequiredReferencesAreVerifiedAndAppended(t *testing.T) {
	refs := func([]llm.Message) []string { return []string{"obj-1", "/var/log/run.log", "ticket#42"} }
	cases := []struct {
		name      string
		reply     string
		wantTail  string
		wantAdded int
	}{
		{"none mentioned", "The work so far.",
			"The work so far.\n\nReferences:\n- obj-1\n- /var/log/run.log\n- ticket#42", 3},
		{"some mentioned, with a section", "Did things.\n\n## References\n- obj-1",
			"Did things.\n\n## References\n- obj-1\n\nReferences (continued):\n- /var/log/run.log\n- ticket#42", 2},
		{"all mentioned", "Touched obj-1, read /var/log/run.log for ticket#42.",
			"Touched obj-1, read /var/log/run.log for ticket#42.", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &eventLog{}
			cfg := smallWindow()
			cfg.References, cfg.OnEvent = refs, log.on
			c := New(cfg, (&recSummarizer{reply: tc.reply}).fn)
			out, ok := c.summaryPass(context.Background(), longConversation(), 9000, "auto")
			if !ok {
				t.Fatalf("no summary: %s", c.lastReason)
			}
			summary := llm.MessagesForAPI(out)[0].Text()
			if !strings.HasSuffix(summary, tc.wantTail) {
				t.Fatalf("summary =\n%s\nwant it to end with\n%s", summary, tc.wantTail)
			}
			for _, r := range refs(nil) {
				if !strings.Contains(summary, r) {
					t.Fatalf("%q is not in the summary", r)
				}
			}
			if ev := log.last(t); ev.ReferencesAdded != tc.wantAdded {
				t.Fatalf("ReferencesAdded = %d, want %d", ev.ReferencesAdded, tc.wantAdded)
			}
		})
	}
	// Deterministic: the same input gives the same bytes.
	a, _ := ensureReferences("s", []string{"x", "y"})
	b, _ := ensureReferences("s", []string{"x", "y"})
	if a != b || a != "s\n\nReferences:\n- x\n- y" {
		t.Fatalf("ensureReferences = %q / %q", a, b)
	}
}
