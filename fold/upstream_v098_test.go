package fold

// Regression tests for fixes ported from acp-kernel v0.0.98 and later, adapted
// from tests/nudge.test.ts (#478), tests/protect-recent.test.ts (#1725),
// tests/parse-compress-input.test.ts (#470) and tests/refs.test.ts (#483).
// acp-kernel is MIT plus an Additional Term from v0.0.98 (Copyright (c) 2026
// ranxianglei); see THIRD-PARTY-LICENSES.md.

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// --- Nudge cadence re-anchors on the shown reference (a587c0c, #478) -------

// A nudge stamped at an overestimated count must not dead-zone the cadence once
// the real count settles far below it but still above baseline-g.
func TestStampNudgeReanchorsBelowShownReference(t *testing.T) {
	cfg := DefaultConfig(200000) // growth unit 50000, cadence floor 22500
	st := CreateInitialState("s", "/a")

	stampNudge(&st, cfg, 60000, NudgeDecision{})                             // baseline 60000
	stampNudge(&st, cfg, 150000, NudgeDecision{ShouldInject: true, Tier: 1}) // shown 150000
	if st.Nudge.LastNudgeShownTokens != 150000 {
		t.Fatalf("shown = %d, want 150000 stamped at inject", st.Nudge.LastNudgeShownTokens)
	}

	// 80000 is above baseline-g (10000), so the baseline clause alone never
	// fires, yet it is a full step below the shown reference (100000).
	stampNudge(&st, cfg, 80000, NudgeDecision{})
	if st.Nudge.LastPerMessageNudgeTokens != 80000 {
		t.Fatalf("baseline = %d, want it re-anchored at the settled 80000", st.Nudge.LastPerMessageNudgeTokens)
	}
	if st.Nudge.LastNudgeShownTokens != 0 || len(st.Nudge.LastShownByTier) != 0 {
		t.Fatalf("nudge stamps = %+v, want the stale shown reference and tier stamps cleared", st.Nudge)
	}

	// The cadence is alive again: 105000 is 25000 past the fresh baseline.
	d := nudgeAt(t, 105000, 60000, func(s *CompressionState, _ *Config) { s.Nudge = st.Nudge })
	if !d.ShouldInject || d.Tier != 1 {
		t.Fatalf("re-anchored cadence must fire after regrowth: inject=%v tier=%d (%s)", d.ShouldInject, d.Tier, d.Reason)
	}
}

// Before the fix the same sequence stayed silent: growth was measured from the
// phantom 150000. Pins that the test above exercises the new clause.
func TestNudgeStaleShownReferenceSuppressesGrowth(t *testing.T) {
	d := nudgeAt(t, 105000, 60000, func(s *CompressionState, _ *Config) {
		s.Nudge.LastPerMessageNudgeTokens = 60000
		s.Nudge.LastNudgeShownTokens = 150000
	})
	if d.ShouldInject {
		t.Fatalf("growth from a stale shown reference must read negative: %s", d.Reason)
	}
}

// Trap: a drop smaller than one growth step below the shown reference is
// ordinary noise and must not reset the cadence.
func TestStampNudgeKeepsShownReferenceOnSmallDrop(t *testing.T) {
	cfg := DefaultConfig(200000)
	st := CreateInitialState("s", "/a")
	st.Nudge.LastPerMessageNudgeTokens = 60000
	st.Nudge.LastNudgeShownTokens = 150000
	st.Nudge.LastShownByTier = map[Tier]int{1: 150000}

	stampNudge(&st, cfg, 120000, NudgeDecision{}) // 30000 below shown: < one step

	if st.Nudge.LastPerMessageNudgeTokens != 60000 || st.Nudge.LastNudgeShownTokens != 150000 ||
		st.Nudge.LastShownByTier[1] != 150000 {
		t.Fatalf("nudge stamps = %+v, want them untouched", st.Nudge)
	}
}

// --- Case-insensitive tool names (2cc21fa, billion-context#1725) ----------
//
// fold's MatchToolPattern already lowercased both sides; these pin it for every
// knob built on it, so a host whose tools are named "read" or "READ" gets the
// same protection as one using Corvid's "Read".

func TestMatchToolPatternIgnoresCase(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"read", "read", true},
		{"read", "Read", true},
		{"READ", "read", true},
		{"bash", "BASH", true},
		{"read", "Grep", false},
		{"read*", "ReadFile", true},
		{"read*", "READ_FILE", true},
		{"SEARCH*", "search_context", true},
		{"read*", "write_file", false},
	}
	for _, c := range cases {
		if got := MatchToolPattern(c.pattern, c.name); got != c.want {
			t.Errorf("MatchToolPattern(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestRecentZoneKnobsIgnoreCase(t *testing.T) {
	cfg := DefaultConfig(1000)
	for _, name := range []string{"read", "BASH", "glob"} {
		if !IsNeverPreserveRecent(CoreMessage{ToolName: name}, cfg) {
			t.Errorf("%s must match the built-in list regardless of casing", name)
		}
	}
	if IsNeverPreserveRecent(CoreMessage{ToolName: "edit"}, cfg) {
		t.Error("a tool off the list must stay off it")
	}
	cfg.PreserveRecentTools = []string{"READ"}
	if IsNeverPreserveRecent(CoreMessage{ToolName: "read"}, cfg) {
		t.Error(`PreserveRecentTools ["READ"] must re-protect a "read" result`)
	}
}

func TestProtectedToolKnobsIgnoreCase(t *testing.T) {
	cfg := DefaultConfig(1000)
	cfg.ProtectedTools = []string{"todowrite"}
	if !IsMessageProtected(CoreMessage{ToolName: "TodoWrite"}, cfg) {
		t.Error("ProtectedTools must match regardless of casing")
	}

	msgs := latestFixture() // TodoWrite exchanges
	cfg = DefaultConfig(1000)
	cfg.ProtectedLatestTools = []string{"TODOWRITE"}
	if ids := latestProtectedCallIDs(msgs, cfg); !ids["todo3"] || len(ids) != 1 {
		t.Fatalf("latest-protected ids = %v, want only the newest instance", ids)
	}
}

// Corvid's own Compress tool stays an exact match, as upstream's always-protected
// list does: a differently-cased "compress" is not the tool fold registered.
func TestAlwaysProtectedToolsStayExact(t *testing.T) {
	cfg := DefaultConfig(1000)
	if !IsMessageProtected(CoreMessage{ToolName: CompressToolName}, cfg) {
		t.Fatal("Compress must be hard-protected")
	}
	if IsMessageProtected(CoreMessage{ToolName: strings.ToLower(CompressToolName)}, cfg) {
		t.Fatal("the always-protected list is an exact match")
	}
}

// A fresh "read" result (lowercase host naming) stays out of the recent zone,
// exactly as Corvid's "Read" does.
func TestComputeProtectedRefsLowercaseReadStaysCompressible(t *testing.T) {
	msgs := []CoreMessage{
		msg("a", RoleUser, CTText, "old alpha"),
		msg("b", RoleAssistant, CTText, "old beta"),
		msg("c", RoleUser, CTText, "old gamma"),
		func() CoreMessage { r := result("d", "k1", "read"); r.Text = strings.Repeat("x", 20000); return r }(),
		msg("e", RoleUser, CTText, "latest user intent"),
	}
	st := withRefs(msgs)
	if got := ComputeProtectedRefs(msgs, st, zoneConfig(3)); got[st.MessageRefs.ByRaw["d"]] {
		t.Fatal("a lowercase read result must take no slot in the recent zone")
	}
}

// --- Single entry object in the content slot (afa1c54, #470) --------------

func TestParseWrapsSingleEntryObject(t *testing.T) {
	r := parse(`{"content":{"startId":"m04083","endId":"m04092","summary":"lost range retry"}}`)
	d := r.Diagnostics
	if d.Kind != ParseOK || !d.Ok || !d.ContentSalvage || d.InvalidItems != 0 {
		t.Fatalf("diagnostics = %+v", d)
	}
	if len(r.Ranges) != 1 || r.Ranges[0].StartRef != "m04083" || r.Ranges[0].EndRef != "m04092" ||
		r.Ranges[0].Summary != "lost range retry" {
		t.Fatalf("ranges = %+v", r.Ranges)
	}
}

func TestParseWrappedEntryKeepsOwnFields(t *testing.T) {
	r := parse(`{"content":{"startRef":"m00001","endRef":"m00002","summary":"s","topic":"Own","summaryMaxChars":1234}}`)
	if len(r.Ranges) != 1 || r.Ranges[0].Topic != "Own" || r.Ranges[0].SummaryMaxChars == nil ||
		*r.Ranges[0].SummaryMaxChars != 1234 {
		t.Fatalf("ranges = %+v", r.Ranges)
	}
}

func TestParseWrappedEntryTakesTopLevelTopic(t *testing.T) {
	r := parse(`{"topic":"Top","content":{"startRef":"m00001","endRef":"m00002","summary":"s"}}`)
	if len(r.Ranges) != 1 || r.Ranges[0].Topic != "Top" {
		t.Fatalf("ranges = %+v", r.Ranges)
	}
}

func TestParseWrapsStringEncodedSingleObject(t *testing.T) {
	inner, _ := json.Marshal(map[string]any{
		"content": map[string]any{"startId": "m04083", "endId": "m04092", "summary": "str"},
	})
	outer, _ := json.Marshal(string(inner))
	r := ParseCompressArgs(outer)
	if r.Diagnostics.Kind != ParseOK || !r.Diagnostics.ContentSalvage || len(r.Ranges) != 1 ||
		r.Ranges[0].Summary != "str" {
		t.Fatalf("result = %+v", r)
	}
}

func TestParseWrapsFencedSingleObject(t *testing.T) {
	r := parse("```json\n" + `{"content":{"startId":"m00001","endId":"m00002","summary":"solo"}}` + "\n```")
	if r.Diagnostics.Kind != ParseOK || !r.Diagnostics.ContentSalvage || len(r.Ranges) != 1 ||
		r.Ranges[0].Summary != "solo" {
		t.Fatalf("result = %+v", r)
	}
}

func TestParseWrapsSingleQuotedSingleObject(t *testing.T) {
	r := parse(`{'content': {'startId': 'm00001', 'endId': 'm00002', 'summary': 'quoted'}}`)
	d := r.Diagnostics
	if d.Kind != ParseOK || !d.QuoteSalvage || !d.ContentSalvage || len(r.Ranges) != 1 {
		t.Fatalf("result = %+v", r)
	}
}

func TestParseUnwrapsNestedRangesArray(t *testing.T) {
	r := parse(`{"content":{"ranges":[{"startRef":"m00001","endRef":"m00002","summary":"a"},{"startRef":"m00003","endRef":"m00004","summary":"b"}]}}`)
	if r.Diagnostics.Kind != ParseOK || !r.Diagnostics.ContentSalvage || len(r.Ranges) != 2 ||
		r.Ranges[0].Summary != "a" || r.Ranges[1].Summary != "b" {
		t.Fatalf("result = %+v", r)
	}
}

// Trap: wrapping invents nothing — a garbage object fails per entry, with the
// reasons the model needs to correct it.
func TestParseWrappedGarbageObjectFailsPerEntry(t *testing.T) {
	r := parse(`{"content":{"foo":1}}`)
	d := r.Diagnostics
	if len(r.Ranges) != 0 || d.Ok || d.Kind != ParseNoValidRanges || !d.ContentSalvage || d.InvalidItems != 1 {
		t.Fatalf("diagnostics = %+v", d)
	}
	if !strings.Contains(d.InvalidReasons[0], "missing startId") {
		t.Fatalf("reasons = %v", d.InvalidReasons)
	}
}

func TestParseWrappedIncompleteEntryReportsMissingSummary(t *testing.T) {
	r := parse(`{"content":{"startId":"m00001","endId":"m00002"}}`)
	d := r.Diagnostics
	if len(r.Ranges) != 0 || d.Kind != ParseNoValidRanges || d.InvalidItems != 1 ||
		!strings.Contains(d.InvalidReasons[0], "missing summary") {
		t.Fatalf("diagnostics = %+v", d)
	}
}

// Trap: a truncated single-object call is dropped, never guessed.
func TestParseTruncatedSingleObjectRecoversNothing(t *testing.T) {
	r := parse(`{"content": {"startId": "m00001", "endId": "m0`)
	if len(r.Ranges) != 0 || r.Diagnostics.Ok {
		t.Fatalf("result = %+v", r)
	}
}

func TestParseScalarContentStaysNotArray(t *testing.T) {
	for _, in := range []string{`{"content":42}`, `{"content":null}`, `{"content":true}`} {
		r := parse(in)
		if r.Diagnostics.Kind != ParseContentNotList || r.Diagnostics.Ok || r.Diagnostics.ContentSalvage || len(r.Ranges) != 0 {
			t.Errorf("%s: result = %+v", in, r)
		}
	}
}

func TestParseCanonicalArrayIsNotMarkedContentSalvage(t *testing.T) {
	r := parse(`{"content":[{"startRef":"m00001","endRef":"m00002","summary":"clean"}]}`)
	if r.Diagnostics.Kind != ParseOK || r.Diagnostics.ContentSalvage || len(r.Ranges) != 1 {
		t.Fatalf("result = %+v", r)
	}
}

// --- Ref capacity widened to 9,999,999 (83b8609, acp-kernel#483) ---------

// Below 100,000 refs render byte-identically (five digits, zero-padded); above
// it they widen naturally, never with a leading zero.
func TestIndexToRefWidensPastOldCap(t *testing.T) {
	cases := map[int]string{
		99999: "m99999", 100000: "m100000", 123456: "m123456", 9_999_999: "m9999999",
		10_000_000: "",
	}
	for in, want := range cases {
		if got := IndexToRef(in); got != want {
			t.Errorf("IndexToRef(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestRefToIndexAcceptsBothWidths(t *testing.T) {
	ok := map[string]int{"m100000": 100000, "m0000001": 1, "m9999999": 9_999_999, "m0100000": 100000}
	for in, want := range ok {
		if got, valid := RefToIndex(in); !valid || got != want {
			t.Errorf("RefToIndex(%q) = %d,%v, want %d,true", in, got, valid, want)
		}
	}
	for _, in := range []string{"m10000000", "m12345678"} {
		if got, valid := RefToIndex(in); valid {
			t.Errorf("RefToIndex(%q) = %d,true, want invalid (over the cap)", in, got)
		}
	}
}

func TestAssignRefsCrossesOldCap(t *testing.T) {
	existing := MessageRefMap{
		ByRaw: map[string]string{"raw-99999": "m99999"},
		ByRef: map[string]string{"m99999": "raw-99999"},
	}
	res := AssignRefs([]CoreMessage{
		msg("new1", RoleUser, CTText, "a"),
		msg("new2", RoleUser, CTText, "b"),
	}, AssignRefsOptions{Existing: existing, NextIndex: 99999})
	if res.Map.ByRaw["new1"] != "m100000" || res.Map.ByRaw["new2"] != "m100001" {
		t.Fatalf("refs = %v, want m100000 and m100001", res.Map.ByRaw)
	}
	if res.NextIndex != 100002 || res.NewlyAssigned != 2 {
		t.Fatalf("next=%d assigned=%d", res.NextIndex, res.NewlyAssigned)
	}
	if HighestUsedIndex(res.Map) != 100001 {
		t.Fatalf("HighestUsedIndex = %d, want 100001", HighestUsedIndex(res.Map))
	}
}

func TestParseBoundaryAcceptsWidenedRefs(t *testing.T) {
	for in, want := range map[string]int{"m100000": 100000, "m9999999": 9_999_999, "m00001": 1, "m1": 1} {
		ref, ok := ParseBoundary(in)
		if !ok || ref.kind != BoundaryMessage || ref.num != want || ref.raw != IndexToRef(want) {
			t.Errorf("ParseBoundary(%q) = %+v,%v", in, ref, ok)
		}
	}
	if ref, ok := ParseBoundary("m10000000"); ok && ref.kind == BoundaryMessage {
		t.Errorf("ParseBoundary(m10000000) = %+v, want no message ref over the cap", ref)
	}
	if ref, ok := ParseBoundary("b3"); !ok || ref.kind != BoundaryBlock {
		t.Errorf("block refs must be unaffected: %+v,%v", ref, ok)
	}
}

// m999999 used to be the out-of-range sentinel; it is a valid ref now, and the
// cap moved to 9,999,999. A widened ref resolves like any other.
func TestResolveBoundariesWidenedRefs(t *testing.T) {
	msgs := []CoreMessage{
		msg("u0", RoleUser, CTText, "task"),
		msg("a1", RoleAssistant, CTText, "x"),
		msg("a2", RoleAssistant, CTText, "y"),
	}
	st := CreateInitialState("s", "/a")
	st.MessageRefs = AssignRefs(msgs, AssignRefsOptions{Existing: st.MessageRefs, NextIndex: 99999}).Map
	if st.MessageRefs.ByRaw["a1"] != "m100000" {
		t.Fatalf("refs = %v", st.MessageRefs.ByRaw)
	}
	rr, err := ResolveBoundaries("m100000", "m0100001", msgs, st)
	if err != nil || rr.StartIndex != 1 || rr.EndIndex != 2 {
		t.Fatalf("resolved = %+v, err = %v", rr, err)
	}
	if _, err := ResolveBoundaries("m10000000", "m100001", msgs, st); err == nil || err.Kind != NotFoundInvalid {
		t.Fatalf("a ref over the cap must be invalid, got %v", err)
	}
	if _, err := ResolveBoundaries("m999999", "m100001", msgs, st); err == nil || err.Kind != NotFoundUnknown {
		t.Fatalf("m999999 is a well-formed ref now, just not assigned: got %v", err)
	}
}

// The protected-zone error lists refs in numeric order across the old cap,
// not lexicographic order (m100000 sorts before m99999 as a string).
func TestSortRefsNumericallyAcrossWidths(t *testing.T) {
	refs := []string{"m100000", "m99999", "m00002"}
	sortRefsNumerically(refs)
	if want := []string{"m00002", "m99999", "m100000"}; !slices.Equal(refs, want) {
		t.Fatalf("sorted = %v, want %v", refs, want)
	}
}
