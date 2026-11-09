package noaadapter

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/0xmanhnv/norma/llm"
	"github.com/0xmanhnv/norma/noa"
	"github.com/0xmanhnv/norma/tool"
)

// Every pressure decision noa makes keys off ONE number: the token count View
// hands to ProcessTurn. Which tier to nudge, whether to nudge at all, when to
// start mechanically shredding tool results — all of it reads that number and
// nothing else.
//
// These tests pin what it is allowed to measure. Getting it wrong does not fail
// loudly: the system keeps working, the requests stay valid, and the only
// symptom is that a long session compresses over and over on a context that was
// never full.
//
// Upstream hit this as issue #289 and fixed it in two layers
// (billion-context-pi src/tokens.ts):
//
//	estimateTokens(msgs, coveredIds, …)  skip messages an active block covers
//	sentViewTokenCount(…)                re-measure on the post-processTurn view,
//	                                     because prune ALSO strips uncovered
//	                                     messages (orphaned tool pairs, absorbed
//	                                     and filtered messages) every turn
//
// Norma's Session.resolveTokenCount does neither: it sums Project(msgs) — the
// whole raw history — so the number only ever grows and a compression that
// removes 40k tokens from the request leaves it untouched.

// ladderTokens is the number View actually passed to ProcessTurn this turn.
func ladderTokens(sess *Session) int { return sess.tokensBefore() }

// refOfCore finds the ref assigned to the core matching pick. Refs are handed
// out inside View, so call this after at least one View.
func refOfCore(t *testing.T, sess *Session, msgs []llm.Message, pick func(noa.CoreMessage) bool) string {
	t.Helper()
	cores, _ := Project(msgs)
	st := sess.State()
	for _, c := range cores {
		if !pick(c) {
			continue
		}
		if ref, ok := st.MessageRefs.ByRaw[c.ID]; ok && ref != noa.BlockedRef {
			return ref
		}
		t.Fatalf("the core matching the predicate (%s/%s) has no usable ref", c.ContentType, c.ToolCallID)
	}
	t.Fatal("no core matched the predicate")
	return ""
}

// compressRange drives the real Compress tool the way the model would.
func compressRange(t *testing.T, sess *Session, callID, startRef, endRef string) string {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"content": []map[string]any{{
		"startId": startRef, "endId": endRef, "topic": "work batch",
		"summary": "Summary of " + startRef + "–" + endRef + ": read and reviewed several " +
			"modules under /src. Key files: auth/jwt.go:42, auth/middleware.go:18. " +
			"Decision: keep JWT with refresh tokens. Open: clock-skew leeway hard-coded at 60s.",
	}}})
	res, err := runCompress(sess, args, &tool.ToolContext{ToolUseID: callID})
	if err != nil {
		t.Fatalf("Compress errored: %v", err)
	}
	return res.Content[0].Text
}

// ---------------------------------------------------------------------------
// Layer 1: content hidden behind a summary must leave the count.
// ---------------------------------------------------------------------------

// The minimal statement of the bug. Compress a range, and the number driving
// every pressure decision must fall — that is the entire point of compressing.
func TestLadderFallsWhenAnActiveBlockHidesContent(t *testing.T) {
	window := 200_000
	sess := simSession(t, window)

	var msgs []llm.Message
	msgs = append(msgs, userText("Refactor the authentication subsystem."))
	for i := range 12 {
		call := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			assistantWith(toolUse(call, "Bash", `{"command":"go test ./..."}`)),
			toolResult(call, fmt.Sprintf("run %d\n", i)+strings.Repeat("output line of a test run. ", 400)),
		)
	}
	// Six more turns so the early ones fall out of the protected tail.
	for i := 12; i < 18; i++ {
		call := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			assistantWith(toolUse(call, "Bash", `{"command":"go build ./..."}`)),
			toolResult(call, fmt.Sprintf("run %d ok", i)),
		)
	}

	sess.View(msgs)
	before := ladderTokens(sess)

	startRef := refOfCore(t, sess, msgs, func(c noa.CoreMessage) bool { return c.ToolCallID == "call_1" })
	endRef := refOfCore(t, sess, msgs, func(c noa.CoreMessage) bool {
		return c.ToolCallID == "call_8" && c.ContentType == noa.CTToolResult
	})
	panel := compressRange(t, sess, "tc1", startRef, endRef)
	if noa.PanelBlockCount(panel) == 0 {
		t.Fatalf("the compression did not form a block, so there is nothing to measure:\n%s", panel)
	}
	msgs = append(msgs,
		assistantWith(toolUse("tc1", noa.CompressToolName, `{}`)),
		toolResult("tc1", panel),
	)

	sent := estimateMessages(sess.View(msgs))
	after := ladderTokens(sess)

	if after >= before {
		t.Fatalf("a block now hides eight turns of output, and the pressure ladder still reads "+
			"%d tokens (was %d). The request it describes weighs %d.\n"+
			"Session.resolveTokenCount sums Project(msgs) — the raw history — and prune replaces "+
			"the covered messages AFTER that, inside ProcessTurn. So the number never falls when a "+
			"compression lands. Upstream's first layer is estimateTokens(msgs, coveredIds, …), "+
			"which skips every message an active block covers.",
			after, before, sent)
	}
}

// ---------------------------------------------------------------------------
// Layer 2: content prune strips without a block covering it.
// ---------------------------------------------------------------------------

// Subtracting block coverage is not enough, which is why upstream needed a
// second layer. prune also removes messages NO block covers: a tool result
// whose call is gone is orphaned and stripped from every request, forever,
// while a coverage-based estimate keeps counting it.
//
// Compress itself can no longer produce such an orphan — the fold-integrity
// gate keeps a call and its result on the same side of a fold — so the test
// first pins that, then plants the orphan the way a host would leave one: a
// trimmed or rewritten history that kept a result and lost its call. This is
// upstream's #289 Fix A discriminator, adapted.
func TestLadderStopsCountingOrphansPruneStripsEveryTurn(t *testing.T) {
	window := 200_000
	sess := simSession(t, window)

	const fanout = noa.ToolPairMaxScan + 1
	bigCommand := `{"command":"` + strings.Repeat("x", 30_000) + `"}`

	calls := []llm.ContentBlock{toolUse("c1", "Bash", bigCommand)}
	for i := 2; i <= fanout; i++ {
		calls = append(calls, toolUse(fmt.Sprintf("c%d", i), "Bash", `{"command":"ls"}`))
	}

	msgs := []llm.Message{userText("Audit the build.")}
	msgs = append(msgs, assistantWith(calls...))
	msgs = append(msgs, toolResult("c1", strings.Repeat("z", 20_000)))
	for i := 2; i <= fanout; i++ {
		msgs = append(msgs, toolResult(fmt.Sprintf("c%d", i), "ok"))
	}
	burstEnd := len(msgs)
	// Trailing work so the burst is outside the protected recent window.
	for i := range 6 {
		call := fmt.Sprintf("t%d", i)
		msgs = append(msgs,
			assistantWith(toolUse(call, "Bash", `{"command":"go vet ./..."}`)),
			toolResult(call, "clean"),
		)
	}

	sess.View(msgs)
	c1Call := refOfCore(t, sess, msgs, func(c noa.CoreMessage) bool {
		return c.ToolCallID == "c1" && c.ContentType == noa.CTToolCall
	})
	// c1's result sits 21 positions past its call, beyond ToolPairMaxScan, so
	// boundary widening cannot pull it in. The integrity gate must refuse.
	refused := compressRange(t, sess, "tc0", c1Call, c1Call)
	if noa.PanelBlockCount(refused) != 0 || !strings.Contains(refused, "tool call/result pair") {
		t.Fatalf("compressing c1's call alone must be refused to keep the pair whole:\n%s", refused)
	}

	// Fold the whole burst so prune has a block to apply.
	lastResult := refOfCore(t, sess, msgs, func(c noa.CoreMessage) bool {
		return c.ToolCallID == fmt.Sprintf("c%d", fanout) && c.ContentType == noa.CTToolResult
	})
	panel := compressRange(t, sess, "tc1", c1Call, lastResult)
	if noa.PanelBlockCount(panel) == 0 {
		t.Fatalf("compressing the whole burst did not form a block:\n%s", panel)
	}

	// Plant the orphan: a large result whose call is nowhere in the history.
	orphan := toolResult("ghost", strings.Repeat("y", 100_000))
	tail := append([]llm.Message{orphan}, msgs[burstEnd:]...)
	msgs = append(msgs[:burstEnd:burstEnd], tail...)
	msgs = append(msgs,
		assistantWith(toolUse("tc1", noa.CompressToolName, `{}`)),
		toolResult("tc1", panel),
	)

	view := sess.View(msgs)
	sent := estimateMessages(view)
	ladder := ladderTokens(sess)

	// The premise: the orphan really is gone from the request, and really is
	// not covered by the block.
	if strings.Contains(strings.Join(textOf(view), "\n"), strings.Repeat("y", 1000)) {
		t.Fatal("the orphaned tool result is still in the request; the orphan layout did not take")
	}
	covered := noa.CoveredMessageIDs(sess.State())
	cores, _ := Project(msgs)
	var orphanID string
	for _, c := range cores {
		if c.ToolCallID == "ghost" && c.ContentType == noa.CTToolResult {
			orphanID = c.ID
		}
	}
	if orphanID == "" {
		t.Fatal("the orphaned result vanished from the projection; the fixture is wrong")
	}
	if covered[orphanID] {
		t.Fatal("the orphan is covered by a block; this discriminator does not apply")
	}

	if ladder > sent*2 {
		t.Fatalf("a 100k-char orphan is stripped from every request and covered by no block, "+
			"yet the ladder reads %d tokens for a request weighing %d.\n"+
			"Subtracting block coverage alone would not fix this: nothing covers the orphan. "+
			"Upstream's second layer (sentViewTokenCount) re-measures on the actual "+
			"post-processTurn view for exactly this reason.",
			ladder, sent)
	}
}

// textOf flattens a request for substring checks.
func textOf(msgs []llm.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Text())
		for _, b := range m.Content {
			for _, inner := range b.Content {
				out = append(out, inner.Text)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// What the mis-measurement does, observed end to end.
// ---------------------------------------------------------------------------

// The reported symptom. A cooperative model on a large window is asked to stop
// and write summaries every few turns, indefinitely, on a context that is
// nowhere near full.
//
// The mechanism is the pressure band. Past MaxContextLimitPct DecideNudge
// deliberately drops the growth gate — "waiting for it to grow further is not a
// strategy" — leaving only minPressureBenefit (5000 tokens by default). With a
// count that never falls, the session enters that band once and never leaves,
// so every time ~5000 tokens of fresh compressible content accumulate it fires
// again. At ~1k tokens a turn that is one nudge per five turns, forever.
func TestPressureBandDoesNotRenudgeEveryFewTurns(t *testing.T) {
	window := 128_000
	sess := simSession(t, window)
	a := newSimAgent(t, sess, 1)

	const turns = 120
	var nudgeTurns []int
	prev := 0
	for i := range turns {
		a.work(4000)
		a.observe()
		if a.nudgesSeen > prev {
			nudgeTurns = append(nudgeTurns, i)
			prev = a.nudgesSeen
		}
	}
	if a.compressions == 0 {
		t.Fatal("nothing was compressed; the fixture is not exercising the ladder")
	}

	// The total is not the tell — the tail is. A ladder stuck in the pressure
	// band re-fires as soon as the benefit floor is cleared again, so it is the
	// SMALLEST gap that exposes it.
	minGap := turns
	for i := 1; i < len(nudgeTurns); i++ {
		minGap = min(minGap, nudgeTurns[i]-nudgeTurns[i-1])
	}
	if len(nudgeTurns) > 1 && minGap < 10 {
		t.Fatalf("nudges at turns %v over %d turns: two are only %d turns apart, and the gaps "+
			"tighten as the session goes on. The model is being told to stop and write summaries "+
			"on a context that is not full.", nudgeTurns, turns, minGap)
	}
	t.Logf("nudges at turns %v (smallest gap %d)", nudgeTurns, minGap)
}

// Emergency truncation destroys content in the request: it replaces a tool
// result's body with head+tail and a marker. It must therefore fire on the size
// of the request. A session whose view comfortably fits keeps its tool results
// whole, however long the underlying history has grown.
func TestTruncationDoesNotFireOnARequestThatFits(t *testing.T) {
	window := 128_000
	sess := simSession(t, window)
	a := newSimAgent(t, sess, 1)

	for range 120 {
		a.work(4000)
		a.observe()
		sent := estimateMessages(sess.View(a.history))
		if sess.lastTruncatedCount > 0 && sent < window/2 {
			t.Fatalf("turn %d: %d tool result(s) were cut down while the request weighed %d of a "+
				"%d window (%.0f%%). emergency-truncate reads the same count the nudge does, so a "+
				"count pinned above the 95%% threshold shreds output out of a request that fits.",
				a.turn, sess.lastTruncatedCount, sent, window, float64(sent)*100/float64(window))
		}
	}
}

// ---------------------------------------------------------------------------
// The provider tells us the truth and we throw it away.
// ---------------------------------------------------------------------------

// Compactor.Pre feeds the provider's real reported input size into the session.
// It is the one authoritative number available — it describes the array the
// provider actually received.
//
// resolveTokenCount compares it against the local estimate and discards it when
// they diverge by more than max(1000, est/10). Because the estimate is the raw
// history, the divergence is enormous in exactly the sessions where the
// provider figure matters most, so the truth is discarded every turn.
//
// Upstream treats the host's reported usage as a raise-only FLOOR
// (applyFloors in src/index.ts), never as something that can be outvoted by an
// estimate that reads high.
func TestProviderReportedSizeIsNotDiscardedAsDrift(t *testing.T) {
	window := 128_000
	sess := simSession(t, window)
	a := newSimAgent(t, sess, 1)

	for range 80 {
		a.work(4000)
		a.observe()
	}

	// What the provider would have reported for the last request we built.
	real := sess.sentTokens()
	if real == 0 {
		t.Fatal("no request was built; the fixture is wrong")
	}
	c := &Compactor{sess: sess}
	c.Pre(t.Context(), a.history, real)

	sess.View(a.history)
	ladder := ladderTokens(sess)

	if ladder > real*2 {
		t.Fatalf("the provider reported %d input tokens and the ladder still reads %d.\n"+
			"resolveTokenCount's drift test compares the reported figure against the raw-history "+
			"estimate; the gap is far past max(1000, est/10), so the honest number is rejected as "+
			"drift and the inflated one wins.", real, ladder)
	}
}

// ---------------------------------------------------------------------------
// The same number is what the user is shown.
// ---------------------------------------------------------------------------

// Upstream fixed the reporting surfaces alongside the transform (#289 Fix B:
// acp_status must read the sent view, not the raw estimate). Norma's equivalent
// is Session.tokensBefore, which the Compress panel prints as its headline
// "before" figure — so a wrong count is not only acted on, it is reported to
// the model and to the user as fact.
func TestPanelHeadlineReportsTheRequestSize(t *testing.T) {
	window := 128_000
	sess := simSession(t, window)
	a := newSimAgent(t, sess, 1)

	for range 80 {
		a.work(4000)
		a.observe()
	}
	sent := estimateMessages(sess.View(a.history))
	shown := sess.tokensBefore()

	if shown > sent*2 {
		t.Fatalf("the panel headline would report %d tokens for a request that weighs %d "+
			"(%.0f%% vs %.0f%% of the %d window). Whatever the ladder decides on, the number "+
			"shown has to describe the request.",
			shown, sent, float64(shown)*100/float64(window), float64(sent)*100/float64(window), window)
	}
}
