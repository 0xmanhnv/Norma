package fold

// Tests for the recent-zone and latest-instance protection knobs, adapted from
// acp-kernel's tests/protected-latest.test.ts and its never/preserve-recent
// coverage (MIT, Copyright © 2026 ranxianglei).

import (
	"strings"
	"testing"
)

func TestNeverPreserveRecentDefaultsToBuiltInList(t *testing.T) {
	for _, name := range []string{"Read", "bash", "LS"} {
		if !IsNeverPreserveRecent(CoreMessage{ToolName: name}, DefaultConfig(1000)) {
			t.Errorf("%s should be on the built-in list", name)
		}
	}
	if IsNeverPreserveRecent(CoreMessage{ToolName: "Edit"}, DefaultConfig(1000)) {
		t.Error("Edit is not on the built-in list")
	}
}

func TestNeverPreserveRecentExplicitListReplaces(t *testing.T) {
	cfg := DefaultConfig(1000)
	cfg.NeverPreserveRecentTools = []string{"mcp__fetch*"}
	if IsNeverPreserveRecent(CoreMessage{ToolName: "Read"}, cfg) {
		t.Error("an explicit list must replace the built-in one, not extend it")
	}
	if !IsNeverPreserveRecent(CoreMessage{ToolName: "mcp__fetch_url"}, cfg) {
		t.Error("glob suffix patterns must match")
	}
	cfg.NeverPreserveRecentTools = []string{}
	if IsNeverPreserveRecent(CoreMessage{ToolName: "Bash"}, cfg) {
		t.Error("an empty, non-nil list excludes nothing")
	}
}

func TestPreserveRecentToolsSubtracts(t *testing.T) {
	cfg := DefaultConfig(1000)
	cfg.PreserveRecentTools = []string{"read"}
	if IsNeverPreserveRecent(CoreMessage{ToolName: "Read"}, cfg) {
		t.Error("PreserveRecentTools must take Read off the list")
	}
	if !IsNeverPreserveRecent(CoreMessage{ToolName: "Bash"}, cfg) {
		t.Error("the rest of the built-in list must be untouched")
	}
	// The built-in-only helper ignores configuration.
	if !IsNeverPreserveRecentTool(CoreMessage{ToolName: "Read"}) {
		t.Error("IsNeverPreserveRecentTool must keep reporting the built-in list")
	}
}

// The fold-then-re-read loop: with Read preserved, a fresh read stays inside
// the recent zone instead of being offered for compression at once.
func TestPreserveRecentToolsKeepsFreshReadInZone(t *testing.T) {
	msgs := []CoreMessage{
		msg("u0", RoleUser, CTText, "task"),
		call("c1", "k1", "Read"),
		func() CoreMessage { r := result("r1", "k1", "Read"); r.Text = strings.Repeat("s", 400); return r }(),
	}
	st := withRefs(msgs)
	cfg := zoneConfig(2)

	if got := ComputeProtectedRefs(msgs, st, cfg); got[st.MessageRefs.ByRaw["r1"]] {
		t.Fatal("by default a Read result takes no slot in the recent zone")
	}
	cfg.PreserveRecentTools = []string{"Read"}
	if got := ComputeProtectedRefs(msgs, st, cfg); !got[st.MessageRefs.ByRaw["r1"]] {
		t.Fatal("with Read preserved, the fresh result must sit in the recent zone")
	}
}

func todoExchange(n string) []CoreMessage {
	c := call("tc"+n, "todo"+n, "TodoWrite")
	c.Text = `{"todos":["step ` + n + `"]}`
	r := result("tr"+n, "todo"+n, "TodoWrite")
	r.Text = strings.Repeat("todo state "+n+" ", 40)
	return []CoreMessage{c, r}
}

func latestFixture() []CoreMessage {
	msgs := []CoreMessage{msg("u0", RoleUser, CTText, "task")}
	msgs = append(msgs, todoExchange("1")...)
	msgs = append(msgs, msg("a1", RoleAssistant, CTText, strings.Repeat("w", 600)))
	msgs = append(msgs, todoExchange("2")...)
	msgs = append(msgs, msg("a2", RoleAssistant, CTText, strings.Repeat("w", 600)))
	msgs = append(msgs, todoExchange("3")...)
	msgs = append(msgs, msg("a3", RoleAssistant, CTText, strings.Repeat("w", 600)))
	msgs = append(msgs, msg("u1", RoleUser, CTText, "next"))
	return msgs
}

func TestProtectedLatestToolsKeepsOnlyNewestInstance(t *testing.T) {
	msgs := latestFixture()
	cfg := zoneConfig(1)
	cfg.ProtectedLatestTools = []string{"Todo*"}
	st := withRefs(msgs)

	res := applyWith(msgs, st, cfg, CompressRange{StartRef: "m00002", EndRef: "m00010", Summary: longSummary})
	if len(res.Errors) != 0 || len(res.BlocksCreated) != 1 {
		t.Fatalf("errors=%v blocks=%d", res.Errors, len(res.BlocksCreated))
	}
	b := res.BlocksCreated[0]
	for _, id := range []string{"tc1", "tr1", "tc2", "tr2"} {
		if !covers(b, id) {
			t.Errorf("older instance %s must stay compressible", id)
		}
	}
	for _, id := range []string{"tc3", "tr3"} {
		if covers(b, id) {
			t.Errorf("newest instance %s must not fold", id)
		}
	}
}

func TestProtectedLatestToolsShowsAsProtectedInRecommendation(t *testing.T) {
	msgs := latestFixture()
	cfg := zoneConfig(1)
	cfg.ProtectedLatestTools = []string{"TodoWrite"}
	st := withRefs(msgs)
	_, prot := BuildCompressibleRanges(msgs, st, cfg, ComputeProtectedRefs(msgs, st, cfg), nil)
	found := false
	for _, p := range prot {
		if p.StartRef == st.MessageRefs.ByRaw["tc3"] && p.EndRef == st.MessageRefs.ByRaw["tr3"] {
			found = containsString(p.Tools, "TodoWrite")
		}
		if p.StartRef == st.MessageRefs.ByRaw["tc1"] {
			t.Fatal("an older instance was reported as protected")
		}
	}
	if !found {
		t.Fatalf("protected = %+v, want the newest TodoWrite exchange", prot)
	}
}

// Unset, nothing changes.
func TestProtectedLatestToolsUnsetIsNoop(t *testing.T) {
	if ids := latestProtectedCallIDs(latestFixture(), DefaultConfig(1000)); len(ids) != 0 {
		t.Fatalf("ids = %v, want none", ids)
	}
}
