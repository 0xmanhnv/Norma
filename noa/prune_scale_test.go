package noa

// Tests for building prune's anchor-pairing index once per pass, adapted from
// acp-kernel's tests/prune-recommend-scale.test.ts (78df864, acp-kernel#498;
// MIT plus Additional Term, Copyright (c) 2026 ranxianglei — see
// THIRD-PARTY-LICENSES.md).

import (
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"testing"
	"time"
)

// pairSafeAnchorIndexReference is the per-anchor full scan the index replaced,
// kept verbatim as the oracle for the differential test below.
func pairSafeAnchorIndexReference(msgs []CoreMessage, index int) int {
	safe := index
	for safe > 0 && safe < len(msgs) &&
		msgs[safe-1].Role == RoleAssistant && msgs[safe].Role == RoleAssistant {
		safe--
	}
	resultIndex := map[string]int{}
	for i, m := range msgs {
		if m.ContentType == CTToolResult && m.ToolCallID != "" {
			if _, seen := resultIndex[m.ToolCallID]; !seen {
				resultIndex[m.ToolCallID] = i
			}
		}
	}
	for range len(msgs) {
		moved := safe
		for i := 0; i < safe && i < len(msgs); i++ {
			m := msgs[i]
			if m.Role != RoleAssistant || m.ContentType != CTToolCall || m.ToolCallID == "" {
				continue
			}
			if ri, ok := resultIndex[m.ToolCallID]; ok && ri >= safe && ri+1 > moved {
				moved = ri + 1
			}
		}
		if moved == safe {
			break
		}
		safe = moved
	}
	return safe
}

// randomPairingHistory mixes users, reasoning, text, calls and results, with
// duplicate and unmatched call ids and results that arrive out of order.
func randomPairingHistory(r *rand.Rand, n int) []CoreMessage {
	msgs := make([]CoreMessage, 0, n)
	var open []string
	nextCall := 0
	for i := range n {
		id := "x" + strconv.Itoa(i)
		switch k := r.Intn(10); {
		case k < 2:
			msgs = append(msgs, msg(id, RoleUser, CTText, "u"))
		case k < 3:
			msgs = append(msgs, think(id))
		case k < 4:
			msgs = append(msgs, msg(id, RoleAssistant, CTText, "a"))
		case k < 7:
			callID := "t" + strconv.Itoa(nextCall)
			if r.Intn(8) == 0 && nextCall > 0 {
				callID = "t" + strconv.Itoa(r.Intn(nextCall)) // duplicate id
			} else {
				nextCall++
			}
			if r.Intn(10) == 0 {
				callID = "" // id-less call
			}
			msgs = append(msgs, call(id, callID, "Read"))
			if callID != "" {
				open = append(open, callID)
			}
		default:
			callID := "orphan" + strconv.Itoa(i)
			if len(open) > 0 {
				j := r.Intn(len(open)) // answer any open call, not only the newest
				callID = open[j]
				open = slices.Delete(open, j, j+1)
			}
			msgs = append(msgs, result(id, callID, "Read"))
		}
	}
	return msgs
}

// The index is an optimisation only: on every input and every anchor position,
// including out-of-range ones, it must agree with the full scan.
func TestPairSafeAnchorIndexMatchesFullScan(t *testing.T) {
	r := rand.New(rand.NewSource(498))
	for trial := range 3000 {
		msgs := randomPairingHistory(r, r.Intn(40))
		ix := buildAnchorPairingIndex(msgs)
		for at := -2; at <= len(msgs)+2; at++ {
			want := pairSafeAnchorIndexReference(msgs, at)
			if got := pairSafeAnchorIndex(msgs, at, ix); got != want {
				t.Fatalf("trial %d, anchor %d: got %d, want %d\nhistory: %v", trial, at, got, want, describe(msgs))
			}
		}
	}
}

func describe(msgs []CoreMessage) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = fmt.Sprintf("%d:%s/%s/%s", i, m.Role, m.ContentType, m.ToolCallID)
	}
	return out
}

// Exact placement: crossing one result exposes a farther call, so the anchor
// moves 2 → 5 (past ta's result) → 7 (past tb's result).
func TestPruneAnchorFollowsMultiStepChain(t *testing.T) {
	msgs := []CoreMessage{
		msg("u0", RoleUser, CTText, "task"),
		call("c1", "ta", "Read"),
		msg("u2", RoleUser, CTText, "covered"),
		call("c3", "tb", "Read"),
		result("o4", "ta", "Read"),
		msg("u5", RoleUser, CTText, "between"),
		result("o6", "tb", "Read"),
		msg("u7", RoleUser, CTText, "after"),
	}
	st := CreateInitialState("s", "/tmp")
	st.Blocks = []CompressionBlock{block("b1", 1, "u2")}
	got := ids(pruneWith(msgs, st))
	want := []string{"u0", "c1", "c3", "o4", "u5", "o6", SummaryMessageID("b1"), "u7"}
	if !slices.Equal(got, want) {
		t.Fatalf("view = %v, want %v", got, want)
	}
}

// Scale: a long session with many active blocks. Before the index, every block
// rescanned the whole history (O(blocks × N)); the bound here is loose and only
// catches a return to that shape.
func TestPruneScalesWithManyBlocks(t *testing.T) {
	const turns = 7000 // 21000 messages
	msgs := []CoreMessage{msg("u0", RoleUser, CTText, "task")}
	for i := range turns {
		n := strconv.Itoa(i)
		msgs = append(msgs,
			call("c"+n, "t"+n, "Read"),
			result("r"+n, "t"+n, "Read"),
			msg("a"+n, RoleAssistant, CTText, "step "+n))
	}
	st := CreateInitialState("s", "/tmp")
	// One block per ten turns, each covering that stretch.
	for b := 0; b*10 < turns-10; b++ {
		var covered []string
		for i := b * 10; i < b*10+10; i++ {
			n := strconv.Itoa(i)
			covered = append(covered, "c"+n, "r"+n, "a"+n)
		}
		st.Blocks = append(st.Blocks, block("b"+strconv.Itoa(b+1), 1, covered...))
	}

	start := time.Now()
	out := pruneWith(msgs, st)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("prune over %d messages and %d blocks took %v", len(msgs), len(st.Blocks), elapsed)
	}
	summaries := 0
	for _, m := range out {
		if isRenderedSummaryMessage(m) {
			summaries++
		}
	}
	if summaries != len(st.Blocks) {
		t.Fatalf("rendered %d summaries, want %d", summaries, len(st.Blocks))
	}
}
