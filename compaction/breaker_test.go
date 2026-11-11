package compaction

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/0xmanhnv/corvid/llm"
)

// The breaker used to stay open for the rest of the session: three failed
// summaries (an endpoint that rejected one request field, a short outage) and a
// long run lost compaction for good, ending in a context overflow hours later.
// It now lets one summary through after a cool-down and closes when it works.
func TestBreakerRecoversAfterCooldown(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	rs := &recSummarizer{err: errors.New("boom")}
	c := New(smallWindow(), rs.fn)
	c.now = func() time.Time { return clock }
	mk := func() []llm.Message {
		var m []llm.Message
		for range 6 {
			m = append(m, bigMsg(llm.RoleUser, 4000))
		}
		return m
	}
	try := func() bool {
		_, ok := c.autoCompact(context.Background(), mk(), 1000, "auto")
		return ok
	}

	for range maxAutoCompactFails {
		try()
	}
	if !c.tripped || rs.calls != maxAutoCompactFails {
		t.Fatalf("after %d failures: tripped=%v calls=%d", maxAutoCompactFails, c.tripped, rs.calls)
	}

	// Open: no summary is attempted while the cool-down runs.
	clock = clock.Add(defaultBreakerCooldown - time.Second)
	try()
	if rs.calls != maxAutoCompactFails {
		t.Fatalf("open breaker called the summarizer (calls=%d)", rs.calls)
	}

	// Half-open: exactly one probe. It fails, so the breaker re-opens at once
	// (one failure, not three) with a fresh cool-down.
	clock = clock.Add(2 * time.Second)
	try()
	try()
	if rs.calls != maxAutoCompactFails+1 {
		t.Fatalf("half-open breaker made %d probes, want 1", rs.calls-maxAutoCompactFails)
	}
	if !c.tripped {
		t.Fatal("a failed probe must re-open the breaker")
	}

	// The next probe succeeds: the breaker closes and compaction works again
	// without waiting.
	rs.err = nil
	clock = clock.Add(defaultBreakerCooldown)
	if !try() {
		t.Fatal("probe after the cool-down should have compacted")
	}
	if c.tripped || c.failures != 0 {
		t.Fatalf("breaker not closed after a successful probe: tripped=%v failures=%d", c.tripped, c.failures)
	}
	if !try() {
		t.Fatal("closed breaker should compact straight away")
	}
}

// A negative cool-down keeps the old behaviour: open until the session ends.
func TestBreakerCooldownDisabled(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	rs := &recSummarizer{}
	cfg := smallWindow()
	cfg.BreakerCooldown = -1
	c := New(cfg, rs.fn)
	c.now = func() time.Time { return clock }
	c.trip()
	clock = clock.Add(24 * time.Hour)
	msgs := []llm.Message{bigMsg(llm.RoleUser, 4000), bigMsg(llm.RoleUser, 4000), bigMsg(llm.RoleUser, 4000)}
	if _, ok := c.autoCompact(context.Background(), msgs, 100, "auto"); ok || rs.calls != 0 {
		t.Fatalf("breaker with no cool-down recovered (calls=%d)", rs.calls)
	}
}

// A cancelled run must not count against the summarizer.
func TestBreakerIgnoresCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := New(smallWindow(), func(ctx context.Context, _ []llm.Message) (string, error) { return "", ctx.Err() })
	msgs := []llm.Message{bigMsg(llm.RoleUser, 4000), bigMsg(llm.RoleUser, 4000), bigMsg(llm.RoleUser, 4000)}
	for range maxAutoCompactFails + 1 {
		c.autoCompact(ctx, msgs, 100, "auto")
	}
	if c.tripped || c.failures != 0 {
		t.Fatalf("cancellation tripped the breaker: tripped=%v failures=%d", c.tripped, c.failures)
	}
}
