package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
)

// The local estimate counted messages only, at a fixed four-thirds of
// characters/4. A request also carries the system prompt and the tool schemas,
// and dense content (hex, base64, JSON) takes far more tokens per character:
// both made the estimate come out short, which is the unsafe direction — the
// compaction that should have run did not.

// The static prefix is part of the count, whether the host measured it or the
// loop announced the prefix.
func TestEstimateIncludesTheStaticPrefix(t *testing.T) {
	msgs := alternating(4, 4000) // 4000 raw tokens → 5333 at 4/3
	bare := New(Config{ContextWindow: 200000}, nil).count(msgs, 0)
	if bare != 5333 {
		t.Fatalf("messages-only estimate = %d", bare)
	}
	measured := New(Config{ContextWindow: 200000, StaticPrefixTokens: 30000}, nil)
	if got := measured.count(msgs, 0); got != bare+30000 {
		t.Fatalf("with a measured prefix: %d, want %d", got, bare+30000)
	}
	announced := New(Config{ContextWindow: 200000}, nil)
	announced.SetRequestPrefix(llm.CompletionRequest{
		System: []string{strings.Repeat("s", 8000)},
		Tools:  []llm.ToolSchema{{Name: "Read", Description: strings.Repeat("d", 3990), InputSchema: map[string]any{"type": "object"}}},
	})
	if got := announced.count(msgs, 0); got < bare+3900 || got > bare+4100 {
		t.Fatalf("with an announced prefix of ~12000 chars: %d, want about %d", got, bare+4000)
	}
	// The provider's own count still wins when there is one.
	if got := measured.count(msgs, 77777); got != 77777 {
		t.Fatalf("reported usage must be preferred: %d", got)
	}
}

// With the prefix counted, a compaction that the messages-only estimate would
// have skipped runs.
func TestStaticPrefixTriggersCompactionEarlier(t *testing.T) {
	msgs := alternating(31, 16000) // ~165000 tokens estimated: under the 179000 threshold…
	run := func(cfg Config) int {
		rs := &recSummarizer{}
		cfg.ContextWindow, cfg.MaxOutputTokens = 200000, 1000 // predictive check out of the way
		c := New(cfg, rs.fn)
		c.Pre(context.Background(), msgs, 0)
		return rs.calls
	}
	if run(Config{}) != 0 {
		t.Fatal("setup: the messages alone should be under the threshold")
	}
	if run(Config{StaticPrefixTokens: 30000}) != 1 { // …and over it with the prefix.
		t.Fatal("a 30000-token prefix puts the request over the threshold; compaction must run")
	}
}

// A calibration factor scales every local estimate.
func TestTokenCalibration(t *testing.T) {
	msgs := alternating(4, 4000)
	c := New(Config{ContextWindow: 200000, TokenCalibration: 2}, nil)
	if got := c.count(msgs, 0); got != 8000 {
		t.Fatalf("calibration 2: %d, want 8000", got)
	}
	if got := c.estimate(msgs[:1]); got != 2000 {
		t.Fatalf("estimate of one message: %d, want 2000", got)
	}
}

// With no factor configured it is learned from what the provider reports: a
// session whose content takes twice the tokens the default assumes is
// estimated accordingly from then on.
func TestCalibrationIsLearnedFromReportedUsage(t *testing.T) {
	msgs := alternating(8, 4000) // 8000 raw tokens
	c := New(Config{ContextWindow: 1_000_000}, nil)
	if got := c.count(msgs, 0); got != 10666 {
		t.Fatalf("default estimate = %d", got)
	}
	c.Pre(context.Background(), msgs, 16000) // the provider counted 16000 for this content
	if got := c.count(msgs, 0); got != 16000 {
		t.Fatalf("after learning: %d, want 16000", got)
	}
	// With a measured prefix, only the rest is attributed to the messages.
	c = New(Config{ContextWindow: 1_000_000, StaticPrefixTokens: 4000}, nil)
	c.Pre(context.Background(), msgs, 20000)
	if got := c.count(msgs, 0); got != 20000 {
		t.Fatalf("after learning with a prefix: %d, want 20000", got)
	}
	// A reported count that cannot be explained by tokenization (a prefix
	// nobody told us about, a conversation too short to measure) is not
	// learned from.
	c = New(Config{ContextWindow: 1_000_000}, nil)
	c.Pre(context.Background(), msgs, 80000)
	c.Pre(context.Background(), alternating(2, 40), 5000)
	if got := c.count(msgs, 0); got != 10666 {
		t.Fatalf("an implausible ratio was learned: %d", got)
	}
	// An explicit factor is never overridden.
	c = New(Config{ContextWindow: 1_000_000, TokenCalibration: 1.5}, nil)
	c.Pre(context.Background(), msgs, 16000)
	if got := c.count(msgs, 0); got != 12000 {
		t.Fatalf("explicit calibration overridden: %d", got)
	}
}
