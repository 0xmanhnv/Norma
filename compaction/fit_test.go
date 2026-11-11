package compaction

import (
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
)

// Fit measures what compaction cannot shrink — the fixed prefix and the most
// recent turn — against the window, so a host can end a run that no summary
// would save instead of compacting in circles.
func TestFitReportsWhenNothingCanBeFreed(t *testing.T) {
	c := New(Config{ContextWindow: 20000, MaxOutputTokens: 1000, StaticPrefixTokens: 3000}, nil)

	// A long conversation of small turns: only the last turn must be kept.
	small := toolLoopHistory(40, 2000)
	f := c.Fit(small)
	lastTurn := c.estimate(small[len(small)-2:])
	if !f.Fits || f.Window != 19000 || f.Prefix != 3000 || f.Tail != lastTurn {
		t.Fatalf("fit = %+v, want a fit with prefix 3000 and the last turn (%d) as the tail", f, lastTurn)
	}

	// The last turn alone is larger than the window: nothing to summarize helps.
	huge := append(toolLoopHistory(3, 2000), toolUseMsg("big"), toolResultMsg(strings.Repeat("r", 80000), "big"))
	f = c.Fit(huge)
	if f.Fits || f.Prefix+f.Tail < f.Window {
		t.Fatalf("fit = %+v, want no fit: the kept turn exceeds the window", f)
	}

	// A prefix that fills the window leaves no room whatever the conversation.
	full := New(Config{ContextWindow: 20000, MaxOutputTokens: 1000, StaticPrefixTokens: 19500}, nil)
	if f := full.Fit(small); f.Fits {
		t.Fatalf("fit = %+v, want no fit: the prefix fills the window", f)
	}

	// Only what follows the last boundary counts, and a parallel tool exchange
	// is kept whole.
	after := append(append([]llm.Message{}, huge...), llm.BoundaryMessage(llm.BoundaryMeta{}), llm.UserText("Summary: s"),
		toolUseMsg("p1", "p2"), toolResultMsg("x", "p1", "p2"))
	f = c.Fit(after)
	if want := c.estimate(after[len(after)-2:]); !f.Fits || f.Tail != want {
		t.Fatalf("fit after a boundary = %+v, want tail %d", f, want)
	}
}
