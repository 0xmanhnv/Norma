package compaction

import "github.com/0xmanhnv/corvid/llm"

// Reason says why a compaction that was due did not happen. The zero value
// means it did.
type Reason string

const (
	// ReasonNoSummarizer: no summarizer is configured.
	ReasonNoSummarizer Reason = "no_summarizer"
	// ReasonBreakerOpen: the summarizer failed repeatedly and is being rested
	// (see Config.BreakerCooldown).
	ReasonBreakerOpen Reason = "breaker_open"
	// ReasonNothingToSummarize: everything since the last boundary is in the
	// kept tail, or there is no turn boundary to cut at.
	ReasonNothingToSummarize Reason = "nothing_to_summarize"
	// ReasonMessageExceedsBudget: cannot compact: one message exceeds the
	// budget. The most recent turn, which has to be kept, is by itself larger
	// than what fits beside a summary, so summarizing the rest cannot bring
	// the context under the threshold.
	ReasonMessageExceedsBudget Reason = "message_exceeds_budget"
	// ReasonSummaryFailed: the summarizer returned an error (Event.Err).
	ReasonSummaryFailed Reason = "summary_failed"
	// ReasonSummaryNotSmaller: the summary was made, but the view with it is
	// no smaller than the view without, so it was thrown away.
	ReasonSummaryNotSmaller Reason = "summary_not_smaller"
	// ReasonSummaryRejected: a summary was thrown away earlier on this same
	// turn, so no second one was asked for.
	ReasonSummaryRejected Reason = "summary_rejected_this_turn"
	// ReasonCancelled: the run's context ended while the summary was being made.
	ReasonCancelled Reason = "cancelled"
)

// Kinds of compaction pass an Event reports.
const (
	KindMicro   = "micro"   // old tool results cleared
	KindSummary = "summary" // the head summarized behind a boundary
)

// Event describes one compaction pass, or one that was due and did not
// happen. It is delivered to Config.OnEvent for the passes the compactor makes
// by itself before a request (Trigger "auto") and for the ones made after a
// context overflow (Trigger "reactive") alike.
type Event struct {
	Kind    string // KindMicro or KindSummary
	Trigger string // "auto" or "reactive"
	// Applied is true when the history was changed. When false, Reason says
	// why nothing happened and Err carries the summarizer's error, if any.
	Applied bool
	Reason  Reason
	Err     error
	// TokensBefore is the size of the context the pass was decided on: the
	// provider's reported count when there is one, the local estimate
	// otherwise. TokensAfter is that size less what the pass saved; equal to
	// TokensBefore when nothing was applied.
	TokensBefore, TokensAfter int
	// MessagesSummarized (summary) and ResultsCleared (micro) size the pass.
	MessagesSummarized, ResultsCleared int
}

// tailHardLimit is the most the kept tail may weigh for a compaction to be
// worth making: what is left under the summary threshold once the summary
// itself has taken its reservation.
func (c *Compactor) tailHardLimit() int {
	return max(c.autoThreshold()-c.reservedForSummary(), 0)
}

// keepRecentTokens is the token budget of the kept tail (Config.KeepRecentTokens).
func (c *Compactor) keepRecentTokens() int {
	if c.cfg.KeepRecentTokens > 0 {
		return c.cfg.KeepRecentTokens
	}
	return max(c.effectiveWindow()/5, 1)
}

// workingBounds returns [start, tailStart): start is just after the last
// boundary marker (0 if none); tailStart is where the preserved recent tail
// begins. Messages in [start, tailStart) are summarized. A non-empty reason
// means there is nothing that can be summarized, and why.
//
// The tail is the longest run of recent turns that fits the token budget
// (Config.KeepRecentTokens) and the message cap (Config.KeepRecent), and it
// always begins at a turn boundary:
//
//   - at an assistant message, so the view after the boundary reads summary
//     (user), assistant, user, … with roles alternating;
//   - never inside a tool exchange: a tool_use and all of its tool_results
//     (parallel calls included) stay on the same side of the cut. Cutting by
//     message count used to start the tail on a tool_result, which the view
//     then dropped as unpaired — silently losing the result — while its call
//     was dropped from the summary input.
//
// The most recent turn is kept even when it is over the budget; but when it is
// larger than tailHardLimit no summary can help, and that is reported instead
// of summarizing a head that leaves the context just as full.
//
// A conversation with no assistant message after the boundary (not a shape a
// run produces) is cut at plain user messages instead.
func (c *Compactor) workingBounds(msgs []llm.Message) (start, tailStart int, reason Reason) {
	start = llm.LastBoundaryIndex(msgs) + 1
	n := len(msgs)
	if start >= n {
		return start, start, ReasonNothingToSummarize
	}
	split := splitsToolPair(msgs, start)
	clean := func(i int, role llm.Role) bool {
		m := msgs[i]
		if split[i-start] || llm.IsBoundaryMarker(m) || m.Role != role {
			return false
		}
		return role == llm.RoleAssistant || !hasToolResult(m)
	}
	role := llm.RoleUser
	for i := start + 1; i < n; i++ {
		if clean(i, llm.RoleAssistant) {
			role = llm.RoleAssistant
			break
		}
	}

	budget, maxMsgs := c.keepRecentTokens(), c.cfg.KeepRecent
	tailStart = -1
	tokens, count := 0, 0
	for i := n - 1; i >= start; i-- {
		if !llm.IsBoundaryMarker(msgs[i]) {
			tokens += c.estimate(msgs[i : i+1])
			count++
		}
		over := tokens > budget || (maxMsgs > 0 && count > maxMsgs)
		if i == start {
			if !over {
				// Everything since the boundary fits in the tail.
				return start, start, ReasonNothingToSummarize
			}
			break
		}
		if !clean(i, role) {
			continue
		}
		if over && tailStart >= 0 {
			break // the previous cut was the longest tail that fits
		}
		tailStart = i
		if over {
			// Even the most recent turn is over the budget. It is kept
			// anyway, unless nothing could fit beside it.
			if tokens > c.tailHardLimit() {
				return start, i, ReasonMessageExceedsBudget
			}
			break
		}
	}
	if tailStart < 0 {
		// No turn boundary to cut at. If what cannot be cut is also too large
		// to keep, say so: it is a different problem from having nothing to do.
		if tokens > c.tailHardLimit() {
			return start, start, ReasonMessageExceedsBudget
		}
		return start, start, ReasonNothingToSummarize
	}
	if len(contentOnly(msgs[start:tailStart])) == 0 {
		return start, start, ReasonNothingToSummarize
	}
	return start, tailStart, ""
}

// splitsToolPair reports, for each index i of msgs[start:], whether cutting the
// conversation just before msgs[start+i] would separate a tool_use from one of
// its tool_results.
func splitsToolPair(msgs []llm.Message, start int) []bool {
	useAt := map[string]int{}
	// diff[i] counts exchanges opened before i and not yet closed at i.
	diff := make([]int, len(msgs)-start+1)
	for i := start; i < len(msgs); i++ {
		for _, b := range msgs[i].Content {
			switch b.Type {
			case llm.BlockToolUse:
				useAt[b.ID] = i
			case llm.BlockToolResult:
				if u, ok := useAt[b.ToolUseID]; ok && u < i {
					// A cut at any index in (u, i] splits this exchange.
					diff[u+1-start]++
					diff[i+1-start]--
				}
			}
		}
	}
	out := make([]bool, len(msgs)-start)
	open := 0
	for i := range out {
		open += diff[i]
		out[i] = open > 0
	}
	return out
}

func hasToolResult(m llm.Message) bool {
	for _, b := range m.Content {
		if b.Type == llm.BlockToolResult {
			return true
		}
	}
	return false
}

// estimate is the local token estimate of msgs, on the scale count uses.
func (c *Compactor) estimate(msgs []llm.Message) int {
	return EstimateTokens(msgs) * 4 / 3
}
