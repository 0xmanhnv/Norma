package fold

import "maps"

// TurnGroup is one assistant turn as the provider sees it: the reasoning run,
// the assistant acts that follow it, and the tool results those acts produced.
//
// The group is the unit turn-integrity protects. A provider that validates
// thinking-to-tool_use pairing rejects a request where half a group survived.
type TurnGroup struct {
	// ReasoningIdx are the indices of the leading reasoning run.
	ReasoningIdx []int
	// ActIdx are the assistant text / tool-call indices.
	ActIdx []int
	// ResultIdx are the tool results belonging to those acts.
	ResultIdx []int
}

// indices flattens the group into every index it owns.
func (g TurnGroup) indices() []int {
	out := make([]int, 0, len(g.ReasoningIdx)+len(g.ActIdx)+len(g.ResultIdx))
	out = append(out, g.ReasoningIdx...)
	out = append(out, g.ActIdx...)
	out = append(out, g.ResultIdx...)
	return out
}

// ComputeTurnGroups partitions messages into assistant turns.
//
// Only groups that actually have a reasoning run matter for turn integrity —
// a turn with no thinking cannot lose it — but they are all returned so callers
// can reason about the structure uniformly.
func ComputeTurnGroups(msgs []CoreMessage) []TurnGroup {
	var groups []TurnGroup
	i := 0
	for i < len(msgs) {
		if msgs[i].ContentType != CTReasoning && !isAssistantAct(msgs[i]) {
			i++
			continue
		}
		var g TurnGroup
		for i < len(msgs) && msgs[i].ContentType == CTReasoning {
			g.ReasoningIdx = append(g.ReasoningIdx, i)
			i++
		}
		callIDs := map[string]bool{}
		for i < len(msgs) && isAssistantAct(msgs[i]) {
			g.ActIdx = append(g.ActIdx, i)
			if msgs[i].ToolCallID != "" {
				callIDs[msgs[i].ToolCallID] = true
			}
			i++
		}
		// The acts' results follow immediately; anything else ends the turn.
		for i < len(msgs) && msgs[i].ContentType == CTToolResult && callIDs[msgs[i].ToolCallID] {
			g.ResultIdx = append(g.ResultIdx, i)
			i++
		}
		if len(g.ReasoningIdx) > 0 || len(g.ActIdx) > 0 {
			groups = append(groups, g)
		}
	}
	return groups
}

// WithdrawSplitTurns removes from folded any turn that would be left broken: a
// group whose reasoning is being compressed away while one of its tool calls
// stays visible.
//
// The visible call would then have no reasoning run to point back to, and a
// strict-echo provider rejects the rebuilt request. Withdrawing the whole group
// is the conservative repair — the range simply compresses a little less.
//
// Returns the surviving set and the indices withdrawn.
//
// This checks the turn invariant only. A fold must also never split a tool
// call from its result; ComputeIntegrityWithdrawals enforces both invariants
// together and is what compression and the recommendation use.
func WithdrawSplitTurns(msgs []CoreMessage, folded map[string]bool) (map[string]bool, []string) {
	if len(folded) == 0 {
		return folded, nil
	}
	out := make(map[string]bool, len(folded))
	maps.Copy(out, folded)
	var withdrawn []string

	for _, g := range ComputeTurnGroups(msgs) {
		if len(g.ReasoningIdx) == 0 {
			continue // nothing to lose
		}
		reasoningFolded := false
		for _, i := range g.ReasoningIdx {
			if out[msgs[i].ID] {
				reasoningFolded = true
				break
			}
		}
		if !reasoningFolded {
			continue
		}
		callStaysVisible := false
		for _, i := range g.ActIdx {
			if msgs[i].ContentType == CTToolCall && !out[msgs[i].ID] {
				callStaysVisible = true
				break
			}
		}
		if !callStaysVisible {
			continue
		}
		for _, i := range g.indices() {
			if out[msgs[i].ID] {
				delete(out, msgs[i].ID)
				withdrawn = append(withdrawn, msgs[i].ID)
			}
		}
	}
	return out, withdrawn
}

// IntegrityWithdrawals is the fold-integrity verdict for a set of ids about to
// be folded: which of them must stay visible instead, and how many turns and
// tool call/result pairs that protects.
type IntegrityWithdrawals struct {
	// Withdrawn holds only ids that were in the folded set.
	Withdrawn      map[string]bool
	SplitTurnCount int
	SplitPairCount int
}

// ComputeIntegrityWithdrawals is the fold-integrity gate. Given the ids a fold
// removes from the visible stream, it returns the ids that must stay visible.
//
// Two invariants, and they feed each other:
//
//   - a kept assistant tool call keeps its reasoning run, so a turn whose
//     reasoning folds while one of its calls stays visible is withdrawn whole;
//   - a tool call and its result are one exchange, so a pair the fold would
//     split is withdrawn together. A visible call whose result folded is an
//     unanswered tool_call_id, a visible result whose call folded answers
//     nothing, and strict providers reject both.
//
// Withdrawing a call for the pair invariant can leave a turn whose reasoning
// still folds while its call is now visible — exactly what the turn invariant
// forbids — and withdrawing a turn can strand a result whose call lived outside
// it. One pass in a fixed order misses both, so the gate repeats until nothing
// changes. Each turn and each pair is acted on, and counted, once.
//
// The reverse split stays legal: reasoning and text may stay visible while a
// whole call/result pair folds, because that still leaves a valid stream.
//
// folded is not modified.
//
// Ported from acp-kernel (MIT, Copyright © 2026 ranxianglei):
// src/turn-integrity.ts, computeIntegrityWithdrawals.
func ComputeIntegrityWithdrawals(msgs []CoreMessage, folded map[string]bool) IntegrityWithdrawals {
	out := IntegrityWithdrawals{Withdrawn: map[string]bool{}}
	if len(folded) == 0 {
		return out
	}
	remaining := make(map[string]bool, len(folded))
	maps.Copy(remaining, folded)

	resultByCallID := map[string]string{}
	for _, m := range msgs {
		if m.ID != "" && m.ContentType == CTToolResult && m.ToolCallID != "" {
			if _, seen := resultByCallID[m.ToolCallID]; !seen {
				resultByCallID[m.ToolCallID] = m.ID
			}
		}
	}

	groups := ComputeTurnGroups(msgs)
	handledTurns := map[int]bool{}
	handledPairs := map[string]bool{}
	withdraw := func(id string) {
		if remaining[id] {
			delete(remaining, id)
			out.Withdrawn[id] = true
		}
	}

	for changed := true; changed; {
		changed = false

		for gi, g := range groups {
			if handledTurns[gi] || len(g.ReasoningIdx) == 0 {
				continue
			}
			reasoningFolds := false
			for _, i := range g.ReasoningIdx {
				if remaining[msgs[i].ID] {
					reasoningFolds = true
					break
				}
			}
			if !reasoningFolds {
				continue
			}
			callKept := false
			for _, i := range g.ActIdx {
				if msgs[i].ContentType == CTToolCall && msgs[i].ID != "" && !remaining[msgs[i].ID] {
					callKept = true
					break
				}
			}
			if !callKept {
				continue
			}
			handledTurns[gi] = true
			for _, i := range g.indices() {
				withdraw(msgs[i].ID)
			}
			changed = true
		}

		for _, m := range msgs {
			if m.ID == "" || m.Role != RoleAssistant || m.ContentType != CTToolCall || m.ToolCallID == "" {
				continue
			}
			if handledPairs[m.ToolCallID] {
				continue
			}
			rid, ok := resultByCallID[m.ToolCallID]
			if !ok || remaining[m.ID] == remaining[rid] {
				continue
			}
			handledPairs[m.ToolCallID] = true
			withdraw(m.ID)
			withdraw(rid)
			changed = true
		}
	}

	out.SplitTurnCount = len(handledTurns)
	out.SplitPairCount = len(handledPairs)
	return out
}
