package compaction

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
)

// readPair is one Read call and its result of n characters.
func readPair(id string, n int) []llm.Message {
	return toolPair("Read", id, strings.Repeat("y", n))
}

// batchConfig: effective window 96000, micro threshold 57600, summary due at
// 77000 (predictive), a MicroCompact pass must free 9600 tokens.
func batchConfig() Config {
	return Config{ContextWindow: 100000, MaxOutputTokens: 4000, KeepRecent: 2}
}

// MicroCompact used to clear one more old tool result on every turn above the
// threshold, and the loop wrote that into the history: the request prefix
// changed on every turn, so no turn could read the conversation from the
// prompt cache. A pass now waits until it frees MicroMinFree, clears
// everything old at once, and the history is the same bytes until the next.
func TestMicroCompactRunsInBatches(t *testing.T) {
	c := New(batchConfig(), nil)
	if got := c.microMinFree(); got != 9600 {
		t.Fatalf("default MicroMinFree=%d, want a tenth of the effective window (9600)", got)
	}
	var msgs []llm.Message
	passes, cleared := 0, 0
	const resultChars = 6000 // ~2000 tokens each
	for turn := range 40 {
		msgs = append(msgs, readPair("r"+strconv.Itoa(turn), resultChars)...)
		before := msgs
		msgs = c.Pre(context.Background(), msgs, 60000) // above the micro threshold, no summary due
		now := countCleared(msgs)
		if now == cleared {
			// No pass: not one message may have been touched.
			if !reflect.DeepEqual(before, msgs) {
				t.Fatalf("turn %d: history changed without a pass", turn)
			}
			continue
		}
		passes++
		if freed := (now - cleared) * resultChars / 3; freed < c.microMinFree() {
			t.Fatalf("turn %d: a pass freed ~%d tokens, below the minimum %d", turn, freed, c.microMinFree())
		}
		cleared = now
		// A pass clears everything but the most recent results.
		if want := turn + 1 - microKeepRecent; now != want {
			t.Fatalf("turn %d: %d results cleared, want all but the last %d (%d)", turn, now, microKeepRecent, want)
		}
	}
	// 40 turns of ~2000 tokens with a 9600-token minimum: a pass every fifth
	// turn, not every turn.
	if passes < 5 || passes > 8 {
		t.Fatalf("%d passes over 40 turns; want one every five turns or so", passes)
	}
}

// Clearing replaces a result's body and nothing else: every tool_use keeps its
// tool_result, in the same place, so the request stays valid.
func TestMicroCompactKeepsToolPairsIntact(t *testing.T) {
	c := New(batchConfig(), nil)
	var msgs []llm.Message
	for i := range 20 {
		msgs = append(msgs, readPair("r"+strconv.Itoa(i), 6000)...)
	}
	out := c.Pre(context.Background(), msgs, 60000)
	if countCleared(out) == 0 {
		t.Fatal("expected a pass")
	}
	if len(out) != len(msgs) {
		t.Fatalf("message count changed: %d → %d", len(msgs), len(out))
	}
	for i := range msgs {
		if out[i].Role != msgs[i].Role || len(out[i].Content) != len(msgs[i].Content) {
			t.Fatalf("message %d changed shape", i)
		}
		for j, b := range msgs[i].Content {
			o := out[i].Content[j]
			if o.Type != b.Type || o.ID != b.ID || o.ToolUseID != b.ToolUseID || o.Name != b.Name {
				t.Fatalf("message %d block %d: pairing fields changed: %+v → %+v", i, j, b, o)
			}
		}
	}
	if !reflect.DeepEqual(llm.PairToolBlocks(llm.MessagesForAPI(out)), llm.MessagesForAPI(out)) {
		t.Fatal("a pass left an unpaired tool block")
	}
	// The caller's slice is not edited in place.
	if countCleared(msgs) != 0 {
		t.Fatal("Pre mutated its input")
	}
}

// With a summary due, clearing is done instead only when it is a real batch
// and gets the context back under the threshold. Otherwise the history is left
// unedited and summarized as it stands, so the summary reads the cache.
func TestMicroCompactOrSummary(t *testing.T) {
	old := func(n, chars int) []llm.Message {
		var msgs []llm.Message
		for i := range n {
			msgs = append(msgs, readPair("r"+strconv.Itoa(i), chars)...)
		}
		return msgs
	}

	// 15 clearable results of ~2000 tokens: clearing frees ~30000, which takes
	// 80000 back under the 77000 line. No summary.
	rs := &recSummarizer{}
	c := New(batchConfig(), rs.fn)
	out := c.Pre(context.Background(), old(15+microKeepRecent, 6000), 80000)
	if rs.calls != 0 || boundaryCount(out) != 0 || countCleared(out) != 15 {
		t.Fatalf("clearing was enough: calls=%d boundaries=%d cleared=%d", rs.calls, boundaryCount(out), countCleared(out))
	}

	// The same history at 120000 tokens: clearing cannot avert the summary, so
	// nothing is cleared first and the summarizer sees the results intact.
	rs = &recSummarizer{}
	c = New(batchConfig(), rs.fn)
	out = c.Pre(context.Background(), old(15+microKeepRecent, 6000), 120000)
	if rs.calls != 1 || boundaryCount(out) != 1 {
		t.Fatalf("summary expected: calls=%d boundaries=%d", rs.calls, boundaryCount(out))
	}
	if countCleared(out) != 0 || countCleared(rs.last) != 0 {
		t.Fatal("results were cleared ahead of a summary that was coming anyway")
	}

	// Too little to clear (one small result) with a summary due: summary, and
	// again no edit before it.
	rs = &recSummarizer{}
	c = New(batchConfig(), rs.fn)
	out = c.Pre(context.Background(), old(1+microKeepRecent, 300), 80000)
	if rs.calls != 1 || countCleared(out) != 0 {
		t.Fatalf("small clear + summary due: calls=%d cleared=%d", rs.calls, countCleared(out))
	}

	// No summarizer (or its breaker open): clearing is the only relief, so it
	// runs whatever it frees.
	c = New(batchConfig(), nil)
	out = c.Pre(context.Background(), old(1+microKeepRecent, 300), 80000)
	if countCleared(out) != 1 {
		t.Fatalf("no summarizer: cleared=%d, want 1", countCleared(out))
	}
	rs = &recSummarizer{}
	c = New(batchConfig(), rs.fn)
	c.trip()
	out = c.Pre(context.Background(), old(1+microKeepRecent, 300), 80000)
	if countCleared(out) != 1 || rs.calls != 0 {
		t.Fatalf("open breaker: cleared=%d calls=%d", countCleared(out), rs.calls)
	}
}
