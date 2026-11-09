package noa

// Regression tests for the fixes synced from acp-kernel v0.0.97 (MIT,
// Copyright © 2026 ranxianglei). Each scenario is adapted from the upstream
// test named in its comment; see docs/guides/noa-upstream-sync.md.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// zoneConfig is a config whose soft protected zone is exactly the last n
// messages, with the size floors out of the way so tiny fixtures can fold.
func zoneConfig(n int) Config {
	c := DefaultConfig(200000)
	c.PreserveRecentMessages = n
	c.PreserveRecentTokens = 0
	c.Compress.MinCompressRange = 1
	c.Compress.MinSummaryLength = 1
	return c
}

func withRefs(msgs []CoreMessage) CompressionState {
	st := CreateInitialState("s", "/a")
	st.MessageRefs = AssignRefs(msgs, AssignRefsOptions{Existing: st.MessageRefs, NextIndex: 1}).Map
	return st
}

func applyWith(msgs []CoreMessage, st CompressionState, cfg Config, rs ...CompressRange) ApplyResult {
	return ApplyCompression(ApplyInput{
		Ranges: rs, Messages: msgs, State: st, Config: cfg,
		CallID: "toolu_sync", Archiver: newMemArchiver(),
	})
}

func covers(b CompressionBlock, id string) bool {
	for _, x := range b.EffectiveMessageIDs {
		if x == id {
			return true
		}
	}
	return false
}

func editCall(id, callID string) CoreMessage {
	c := call(id, callID, "Edit")
	c.Text = `{"path":"a.go"}`
	return c
}

func editResult(id, callID string) CoreMessage {
	r := result(id, callID, "Edit")
	r.Text = strings.Repeat("r", 400)
	return r
}

// unansweredToolCalls returns the calls a strict provider would reject: every
// tool call must be answered before any other message follows.
func unansweredToolCalls(view []CoreMessage) []string {
	var bad, calls []string
	answered := map[string]bool{}
	flush := func() {
		for _, c := range calls {
			if !answered[c] {
				bad = append(bad, c)
			}
		}
		calls, answered = nil, map[string]bool{}
	}
	for _, m := range view {
		switch {
		case m.ContentType == CTReasoning:
		case m.ContentType == CTToolCall:
			calls = append(calls, m.ToolCallID)
		case m.ContentType == CTToolResult:
			answered[m.ToolCallID] = true
		default:
			flush()
		}
	}
	flush()
	return bad
}

// --- fold integrity (upstream tests/tool-pair-integrity.test.ts) -----------

// The protected-zone carve can leave the call inside the range and its result
// outside it. Folding the call alone would strip the visible result as an
// orphan; the pair must stay visible instead.
func TestApplyCompressionWithdrawsSplitToolPair(t *testing.T) {
	msgs := []CoreMessage{
		msg("u0", RoleUser, CTText, "the task"),
		msg("a0", RoleAssistant, CTText, strings.Repeat("x", 400)),
		editCall("c1", "x1"),
		editResult("r1", "x1"),
		msg("u1", RoleUser, CTText, "next"),
		msg("a1", RoleAssistant, CTText, "ok"),
		msg("u2", RoleUser, CTText, "more"),
		msg("a2", RoleAssistant, CTText, "done"),
	}
	st := withRefs(msgs)
	// The last five messages (r1..a2) are protected; the range asks for a0..c1.
	res := applyWith(msgs, st, zoneConfig(5), CompressRange{StartRef: "m00002", EndRef: "m00003", Summary: longSummary})
	if len(res.Errors) != 0 {
		t.Fatalf("errors = %v", res.Errors)
	}
	if len(res.BlocksCreated) != 1 {
		t.Fatalf("blocks = %d, want 1", len(res.BlocksCreated))
	}
	b := res.BlocksCreated[0]
	if covers(b, "c1") || covers(b, "r1") {
		t.Fatalf("block covers %v — the call/result pair was split across the fold", b.EffectiveMessageIDs)
	}
	if !covers(b, "a0") {
		t.Fatalf("block covers %v, want a0 to fold", b.EffectiveMessageIDs)
	}
	warned := false
	for _, w := range res.Warnings {
		warned = warned || strings.Contains(w, "tool call/result pair")
	}
	if !warned {
		t.Fatalf("warnings = %v, want the withdrawal reported", res.Warnings)
	}

	view := ProcessTurn(ProcessTurnInput{Messages: msgs, State: res.State, Config: zoneConfig(5)}).Messages
	if !containsID(view, "c1") || !containsID(view, "r1") {
		t.Fatalf("view = %v, want the pair visible", ids(view))
	}
	if bad := unansweredToolCalls(view); len(bad) != 0 {
		t.Fatalf("unanswered calls %v in %v", bad, ids(view))
	}
}

func TestApplyCompressionRefusesRangeThatOnlySplitsAPair(t *testing.T) {
	msgs := []CoreMessage{
		msg("u0", RoleUser, CTText, "the task"),
		editCall("c1", "x1"),
		editResult("r1", "x1"),
		msg("u1", RoleUser, CTText, "next"),
		msg("a1", RoleAssistant, CTText, "ok"),
		msg("u2", RoleUser, CTText, "more"),
		msg("a2", RoleAssistant, CTText, "done"),
	}
	st := withRefs(msgs)
	res := applyWith(msgs, st, zoneConfig(5), CompressRange{StartRef: "m00002", EndRef: "m00002", Summary: longSummary})
	if len(res.BlocksCreated) != 0 {
		t.Fatalf("created %d block(s) from a range whose only content is half a pair", len(res.BlocksCreated))
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "split 1 tool call/result pair") {
		t.Fatalf("errors = %v, want the split-pair refusal", res.Errors)
	}
}

// The two invariants feed each other: withdrawing a call for the pair rule
// leaves its turn with folded reasoning and a visible call.
func TestComputeIntegrityWithdrawalsReachesFixedPoint(t *testing.T) {
	msgs := []CoreMessage{
		think("t1"),
		editCall("c1", "x1"),
		editResult("r1", "x1"),
	}
	// The reasoning and the call fold; the result does not.
	iw := ComputeIntegrityWithdrawals(msgs, map[string]bool{"t1": true, "c1": true})
	if !iw.Withdrawn["t1"] || !iw.Withdrawn["c1"] {
		t.Fatalf("withdrawn = %v, want both the call (pair rule) and the reasoning (turn rule)", iw.Withdrawn)
	}
	if iw.Withdrawn["r1"] {
		t.Fatal("r1 was never folded, so it cannot be withdrawn")
	}
	if iw.SplitPairCount != 1 || iw.SplitTurnCount != 1 {
		t.Fatalf("counts = %d pair(s), %d turn(s), want 1 and 1", iw.SplitPairCount, iw.SplitTurnCount)
	}
}

// The reverse split stays legal: reasoning and text visible, the whole pair
// folded.
func TestComputeIntegrityWithdrawalsAllowsFoldingWholePair(t *testing.T) {
	msgs := []CoreMessage{
		think("t1"),
		msg("a1", RoleAssistant, CTText, "looking"),
		editCall("c1", "x1"),
		editResult("r1", "x1"),
	}
	iw := ComputeIntegrityWithdrawals(msgs, map[string]bool{"c1": true, "r1": true})
	if len(iw.Withdrawn) != 0 {
		t.Fatalf("withdrawn = %v, want none", iw.Withdrawn)
	}
}

// --- summary placement (upstream tests/tool-pair-integrity.test.ts) --------

func TestPruneKeepsSummaryOutOfParallelBurst(t *testing.T) {
	msgs := []CoreMessage{
		msg("u1", RoleUser, CTText, "go"),
		call("c-grep", "g", "Grep"),
		call("c-read", "r", "Read"),
		result("r-read", "r", "Read"),
		result("r-grep", "g", "Grep"),
		msg("u2", RoleUser, CTText, "next"),
	}
	st := CreateInitialState("s", "/a")
	st.Blocks = []CompressionBlock{block("b1", 1, "c-read", "r-read")}
	view := pruneWith(msgs, st)

	pos := map[string]int{}
	for i, m := range view {
		pos[m.ID] = i
	}
	s, ok := pos[SummaryMessageID("b1")]
	if !ok {
		t.Fatalf("summary missing from %v", ids(view))
	}
	if s > pos["c-grep"] && s < pos["r-grep"] {
		t.Fatalf("summary landed inside the grep call/result span: %v", ids(view))
	}
	if bad := unansweredToolCalls(view); len(bad) != 0 {
		t.Fatalf("unanswered calls %v in %v", bad, ids(view))
	}
}

// A summary inside an assistant run splits one host message in two, and the
// tool calls then reach the provider without their reasoning.
func TestPruneKeepsSummaryOutOfAssistantRun(t *testing.T) {
	msgs := []CoreMessage{
		msg("u1", RoleUser, CTText, "go"),
		think("t1"),
		msg("a1", RoleAssistant, CTText, "plan"),
		call("c1", "x1", "Read"),
		call("c2", "x2", "Grep"),
		result("r1", "x1", "Read"),
		result("r2", "x2", "Grep"),
		msg("u2", RoleUser, CTText, "next"),
	}
	st := CreateInitialState("s", "/a")
	st.Blocks = []CompressionBlock{block("b1", 1, "c1", "r1")}
	view := pruneWith(msgs, st)

	// Between the first and last surviving assistant core there must be no
	// non-assistant message.
	first, last := -1, -1
	for i, m := range view {
		if m.Role == RoleAssistant {
			if first < 0 {
				first = i
			}
			last = i
		}
		if m.ContentType == CTToolResult {
			break
		}
	}
	for i := first; i <= last; i++ {
		if view[i].Role != RoleAssistant {
			t.Fatalf("message %q splits the assistant run: %v", view[i].ID, ids(view))
		}
	}
	if bad := unansweredToolCalls(view); len(bad) != 0 {
		t.Fatalf("unanswered calls %v in %v", bad, ids(view))
	}
}

// --- recommendation (upstream tests/recommend-range-integrity.test.ts) -----

func TestRecommendationNeverAdvertisesUnfoldableTurn(t *testing.T) {
	msgs := []CoreMessage{
		msg("u0", RoleUser, CTText, strings.Repeat("t", 1200)),
		msg("a0", RoleAssistant, CTText, strings.Repeat("a", 1200)),
		think("r1"),
		editCall("c1", "x1"),
		editResult("t1", "x1"),
		msg("a1", RoleAssistant, CTText, "summarized the edit"),
		msg("u1", RoleUser, CTText, "next"),
		msg("a2", RoleAssistant, CTText, "reply"),
	}
	st := withRefs(msgs)
	cfg := zoneConfig(4) // protects t1, a1, u1, a2

	rec := BuildRecommendation(msgs, st, cfg, nil)
	for _, r := range rec.CompressibleRanges {
		if r.EndRef >= "m00003" {
			t.Fatalf("advertised %s–%s, which reaches into a turn whose result is protected", r.StartRef, r.EndRef)
		}
		res := applyWith(msgs, st, cfg, CompressRange{StartRef: r.StartRef, EndRef: r.EndRef, Summary: longSummary})
		if len(res.Errors) != 0 {
			t.Fatalf("advertised range %s–%s does not fold: %v", r.StartRef, r.EndRef, res.Errors)
		}
	}
	if len(rec.CompressibleRanges) != 1 {
		t.Fatalf("ranges = %+v, want just the foldable head", rec.CompressibleRanges)
	}
}

func TestRecommendationStillAdvertisesWholeTurn(t *testing.T) {
	msgs := []CoreMessage{
		msg("u0", RoleUser, CTText, strings.Repeat("t", 1200)),
		think("r1"),
		editCall("c1", "x1"),
		editResult("t1", "x1"),
		msg("a1", RoleAssistant, CTText, strings.Repeat("a", 1200)),
		msg("u1", RoleUser, CTText, "next"),
		msg("a2", RoleAssistant, CTText, "reply"),
	}
	st := withRefs(msgs)
	rec := BuildRecommendation(msgs, st, zoneConfig(2), nil)
	if len(rec.CompressibleRanges) != 1 || rec.CompressibleRanges[0].EndRef != "m00005" {
		t.Fatalf("ranges = %+v, want one range through the whole turn (m00005)", rec.CompressibleRanges)
	}
}

// --- batching (upstream #309 / #498) ---------------------------------------

func positioned(start, end, chars int) RangeInfo {
	return RangeInfo{
		StartRef: IndexToRef(start + 1), EndRef: IndexToRef(end + 1),
		Count: end - start + 1, Tokens: chars / 4, Chars: chars,
		StartIndex: start, EndIndex: end, Positioned: true,
	}
}

func TestMergeRangesFoldsSubThresholdTailIntoPrevious(t *testing.T) {
	got := MergeRangesToThreshold([]RangeInfo{positioned(0, 2, 6000), positioned(3, 4, 1000)}, 5000)
	if len(got) != 1 || got[0].Chars != 7000 || got[0].EndIndex != 4 {
		t.Fatalf("got %+v, want one 7000-char batch through index 4", got)
	}
}

func TestMergeRangesDropsLoneSubThresholdRemainder(t *testing.T) {
	if got := MergeRangesToThreshold([]RangeInfo{positioned(0, 2, 1000)}, 5000); len(got) != 0 {
		t.Fatalf("got %+v — a batch below the gate is rejected the moment it is submitted", got)
	}
}

// An active block sits in the gap; a batch stretched across it covers content
// the model cannot compress, and picking a sub-range of it hits "already
// compressed".
func TestMergeRangesNeverBridgesABlock(t *testing.T) {
	afterBlock := func(r RangeInfo) RangeInfo { r.BlockBefore = true; return r }
	if got := MergeRangesToThreshold([]RangeInfo{positioned(0, 2, 3000), afterBlock(positioned(6, 8, 3000))}, 5000); len(got) != 0 {
		t.Fatalf("got %+v, want nothing: neither side clears the gate alone", got)
	}
	got := MergeRangesToThreshold([]RangeInfo{
		positioned(0, 2, 6000), positioned(3, 4, 1000), afterBlock(positioned(9, 10, 6000)), afterBlock(positioned(14, 15, 1000)),
	}, 5000)
	if len(got) != 2 {
		t.Fatalf("got %+v, want two batches", got)
	}
	// A sub-threshold segment closed by a block is dropped, not folded backward;
	// only a trailing remainder with no block before it joins the last batch.
	if got[0].EndIndex != 2 || got[1].StartIndex != 9 || got[1].EndIndex != 10 {
		t.Fatalf("got %+v, want [0..2] and [9..10]; block-bounded fragments are dropped", got)
	}
	for _, r := range got {
		if r.Chars < 5000 {
			t.Fatalf("batch %+v is below the gate", r)
		}
	}
}

// Soft-protected messages are bridged (a noa deviation): on a read-heavy
// session the short reasoning between reads all sits in the recent zone's token
// budget, and closing batches there would leave every read an island too small
// to advertise.
func TestRecommendationBridgesSoftProtectedBetweenReads(t *testing.T) {
	msgs := []CoreMessage{msg("u0", RoleUser, CTText, "task")}
	for i := 1; i <= 12; i++ {
		id := itoa(i)
		r := result("r"+id, "k"+id, "Read")
		r.Text = strings.Repeat("s", 4000)
		msgs = append(msgs, think("t"+id), msg("x"+id, RoleAssistant, CTText, "Reading"), call("c"+id, "k"+id, "Read"), r)
	}
	cfg := DefaultConfig(40000)
	rec := BuildRecommendation(msgs, withRefs(msgs), cfg, nil)
	if len(rec.CompressibleRanges) == 0 {
		t.Fatal("a read-heavy session advertised nothing to compress")
	}
	for _, r := range rec.CompressibleRanges {
		if r.Chars < cfg.Compress.MinCompressRange {
			t.Fatalf("advertised %+v below the gate", r)
		}
	}
}

// After a fold, a new batch must not stretch back over the block.
func TestRecommendationDoesNotBatchAcrossActiveBlock(t *testing.T) {
	msgs := []CoreMessage{msg("u0", RoleUser, CTText, "task")}
	for i := range 6 {
		msgs = append(msgs, msg("a"+itoa(i), RoleUser, CTText, strings.Repeat("a", 2000)))
	}
	msgs = append(msgs, msg("u1", RoleUser, CTText, "next"))
	for i := range 6 {
		msgs = append(msgs, msg("b"+itoa(i), RoleUser, CTText, strings.Repeat("b", 2000)))
	}
	cfg := zoneConfig(1)
	cfg.Compress.MinCompressRange = 5000
	st := withRefs(msgs)
	st.Blocks = []CompressionBlock{block("b1", 1, "a2", "a3")}
	view := pruneWith(msgs, st)
	rec := BuildRecommendation(view, st, cfg, nil)
	a3 := st.MessageRefs.ByRaw["a3"]
	a2 := st.MessageRefs.ByRaw["a2"]
	for _, r := range rec.CompressibleRanges {
		if r.StartRef < a2 && r.EndRef > a3 {
			t.Fatalf("advertised %s–%s spans active block b1 (%s–%s)", r.StartRef, r.EndRef, a2, a3)
		}
	}
	if len(rec.CompressibleRanges) == 0 {
		t.Fatal("nothing advertised; the fixture is not exercising batching")
	}
}

// --- range display (upstream tests/range-coverage.test.ts) -----------------

// Adjacent ref numbers do not make adjacent spans: what lies between the two
// positions is not compressible.
func TestFormatRangesMergesByPositionNotRefNumber(t *testing.T) {
	a := RangeInfo{StartRef: "m00001", EndRef: "m00005", Count: 5, Tokens: 3000,
		StartIndex: 0, EndIndex: 4, Positioned: true}
	b := RangeInfo{StartRef: "m00006", EndRef: "m00008", Count: 3, Tokens: 3000,
		StartIndex: 10, EndIndex: 12, Positioned: true}
	got := FormatRanges([]RangeInfo{a, b}, nil)
	if !strings.Contains(got, "Compressible ranges (2,") {
		t.Fatalf("positionally separate spans were merged:\n%s", got)
	}
	// Positionally adjacent spans still merge, whatever their numbers.
	c := RangeInfo{StartRef: "m00040", EndRef: "m00042", Count: 3, Tokens: 3000,
		StartIndex: 5, EndIndex: 7, Positioned: true}
	got = FormatRanges([]RangeInfo{c, a}, nil)
	if !strings.Contains(got, "Compressible ranges (1,") || !strings.Contains(got, "m00001–m00042") {
		t.Fatalf("positionally adjacent spans were not merged in view order:\n%s", got)
	}
}

// --- gate messages (upstream tests/regression-bili-470.test.ts) ------------

func TestGateNamesCoveringBlock(t *testing.T) {
	msgs, st := fixture(t, 6, 2000)
	first := applyOne(t, msgs, st, newMemArchiver(), CompressRange{StartRef: "m00002", EndRef: "m00005", Summary: longSummary})
	if len(first.BlocksCreated) != 1 {
		t.Fatalf("setup: %v", first.Errors)
	}
	view := ProcessTurn(ProcessTurnInput{Messages: msgs, State: first.State, Config: DefaultConfig(200000)})
	retry := applyOne(t, view.Messages, view.State, newMemArchiver(), CompressRange{StartRef: "m00003", EndRef: "m00005", Summary: longSummary})
	if len(retry.Errors) != 1 {
		t.Fatalf("errors = %v, want one gate rejection", retry.Errors)
	}
	e := retry.Errors[0]
	if !strings.Contains(e, "already summarized in active block(s) b1") {
		t.Fatalf("gate error does not name the covering block: %s", e)
	}
	if strings.Contains(e, "Active blocks:") {
		t.Fatalf("gate error still carries the blanket block-id hint: %s", e)
	}
	// One T1 block is far below the tier-2 trigger: no distillation advice.
	if strings.Contains(e, "Tier distillation is actionable") {
		t.Fatalf("tier advice offered before it can fire: %s", e)
	}
}

func TestGateOffersTierDistillationOnlyWhenReady(t *testing.T) {
	msgs, st := fixture(t, 6, 2000)
	first := applyOne(t, msgs, st, newMemArchiver(), CompressRange{StartRef: "m00002", EndRef: "m00005", Summary: longSummary})
	view := ProcessTurn(ProcessTurnInput{Messages: msgs, State: first.State, Config: DefaultConfig(200000)})
	cfg := DefaultConfig(200000)
	cfg.Tiers.Tier2Trigger = 1
	retry := ApplyCompression(ApplyInput{
		Ranges:   []CompressRange{{StartRef: "m00003", EndRef: "m00005", Summary: longSummary}},
		Messages: view.Messages, State: view.State, Config: cfg, CallID: "toolu_2", Archiver: newMemArchiver(),
	})
	if len(retry.Errors) != 1 || !strings.Contains(retry.Errors[0], `Tier distillation is actionable now: Compress({ content: [{ startId: "b1", endId: "b1"`) {
		t.Fatalf("errors = %v, want the actionable tier hint", retry.Errors)
	}
}

func TestProtectedExclusionWarningSaysNotToRetarget(t *testing.T) {
	msgs := []CoreMessage{
		msg("u0", RoleUser, CTText, "the task"),
		msg("a0", RoleAssistant, CTText, strings.Repeat("x", 400)),
		msg("a1", RoleAssistant, CTText, strings.Repeat("y", 400)),
		msg("u1", RoleUser, CTText, "next"),
		msg("a2", RoleAssistant, CTText, "done"),
	}
	st := withRefs(msgs)
	res := applyWith(msgs, st, zoneConfig(2), CompressRange{StartRef: "m00002", EndRef: "m00005", Summary: longSummary})
	found := false
	for _, w := range res.Warnings {
		found = found || strings.Contains(w, "do not target them in another Compress call")
	}
	if !found {
		t.Fatalf("warnings = %v", res.Warnings)
	}
}

// --- reversed refs (upstream #310 / #847) ----------------------------------

func TestResolveBoundariesReportsReversal(t *testing.T) {
	msgs := []CoreMessage{
		msg("a", RoleUser, CTText, "1"), msg("b", RoleAssistant, CTText, "2"), msg("c", RoleUser, CTText, "3"),
	}
	st := withRefs(msgs)
	rr, err := ResolveBoundaries("m00003", "m00001", msgs, st)
	if err != nil {
		t.Fatal(err)
	}
	if rr.StartIndex != 0 || rr.EndIndex != 2 {
		t.Fatalf("range = %d..%d, want the swap to 0..2", rr.StartIndex, rr.EndIndex)
	}
	if !strings.Contains(rr.ReversedNote, "reversed") || len(rr.Warnings) == 0 {
		t.Fatalf("the swap was silent: note=%q warnings=%v", rr.ReversedNote, rr.Warnings)
	}
}

// --- block map (upstream #251) ---------------------------------------------

func TestActiveBlockSpansNeverRenderBlockIDsAsSpan(t *testing.T) {
	msgs := []CoreMessage{msg("a", RoleUser, CTText, "1"), msg("b", RoleUser, CTText, "2")}
	st := withRefs(msgs)
	b := block("b3", 2, "a", "b")
	b.StartRef, b.EndRef = "b1", "b2"
	st.Blocks = []CompressionBlock{b}
	spans := ActiveBlockSpans(st)
	if len(spans) != 1 || spans[0].StartRef != "m00001" || spans[0].EndRef != "m00002" {
		t.Fatalf("spans = %+v, want m00001–m00002 derived from coverage", spans)
	}
}

// --- prompts (upstream #442, #1198) ----------------------------------------

func TestOpenObjectivesReachEveryLayer(t *testing.T) {
	for name, text := range map[string]string{
		"HowToCompressRules": HowToCompressRules,
		"Tier2Rules":         Tier2Rules,
		"Tier3Rules":         Tier3Rules,
	} {
		if !strings.Contains(text, "Open objectives") {
			t.Errorf("%s lacks the open-objectives carry-forward rule", name)
		}
	}
	// The resident guardrail must not tell the model to discard the very line
	// the rules now require.
	if !strings.Contains(BuildSystemPrompt(Sections{}), `"Open objectives:" line`) {
		t.Error("the summaries-in-context guardrail lacks the open-objectives exception")
	}
}

func TestGentleNudgeTipDoesNotDemandEveryRange(t *testing.T) {
	_, text := RenderNudgeText(sampleDecision(1, false), NudgeSections{})
	if strings.Contains(text, "Compress all ranges") {
		t.Fatal("the tail tip contradicts the efficiency note by demanding every range")
	}
	if !strings.Contains(text, "they reappear in later nudges") {
		t.Fatal("the tail tip must say unneeded-now ranges can wait")
	}
}

// --- diagnostics -----------------------------------------------------------

func TestRawPrefixIsValidUTF8(t *testing.T) {
	raw := strings.Repeat("é", rawPrefixLen+50)
	d := ParseCompressArgs([]byte(raw)).Diagnostics
	if !utf8.ValidString(d.RawPrefix) {
		t.Fatal("RawPrefix split a multi-byte character")
	}
	if utf8.RuneCountInString(d.RawPrefix) != rawPrefixLen {
		t.Fatalf("RawPrefix has %d characters, want %d", utf8.RuneCountInString(d.RawPrefix), rawPrefixLen)
	}
}
