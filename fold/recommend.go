package fold

import (
	"sort"
	"strings"
)

// ComputeProtectedRefs is the SOFT protected zone: the tail of the conversation
// the model must not compress, because the current step is probably still using
// it.
//
// Distinct from Config.ProtectedTools, which is a hard, permanent exclusion.
// This zone slides forward as the conversation grows — a message protected now
// becomes compressible later.
func ComputeProtectedRefs(msgs []CoreMessage, state CompressionState, cfg Config) map[string]bool {
	out := map[string]bool{}
	covered := CoveredMessageIDs(state)

	type visibleMsg struct {
		ref    string
		tokens int
		isUser bool
	}
	var visible []visibleMsg
	for _, m := range msgs {
		if m.ID == "" || covered[m.ID] || isRenderedSummaryMessage(m) {
			continue
		}
		if strings.HasPrefix(m.Text, SummaryHeader) {
			continue
		}
		ref, ok := state.MessageRefs.ByRaw[m.ID]
		if !ok || ref == BlockedRef {
			continue
		}
		// Bulky, inherently re-obtainable output does not occupy a slot in the
		// recency window — otherwise a single large Read would push everything
		// else out of protection. It stays fully compressible either way.
		if IsNeverPreserveRecent(m, cfg) {
			continue
		}
		visible = append(visible, visibleMsg{ref: ref, tokens: DefaultCountTokens(m.Text), isUser: m.Role == RoleUser})
	}

	if cfg.PreserveRecentMessages > 0 {
		from := max(len(visible)-cfg.PreserveRecentMessages, 0)
		for _, v := range visible[from:] {
			out[v.ref] = true
		}
	}
	if cfg.PreserveRecentTokens > 0 {
		acc := 0
		for i := len(visible) - 1; i >= 0; i-- {
			if acc >= cfg.PreserveRecentTokens {
				break
			}
			out[visible[i].ref] = true
			acc += visible[i].tokens
		}
	}
	if cfg.PreserveRecentMessages > 0 {
		// The most recent user message carries the live instruction; compressing
		// it changes the task.
		for i := len(visible) - 1; i >= 0; i-- {
			if visible[i].isUser {
				out[visible[i].ref] = true
				break
			}
		}
	}
	return out
}

// Recommendation is what the nudge shows the model: what it may compress, what
// it may not, and whether there is anything worth doing at all.
type Recommendation struct {
	CompressibleRanges []RangeInfo
	ProtectedRanges    []ProtectedRange
	NothingToCompress  bool
}

// scanned is one message classified during the linear scan.
type scanned struct {
	id        string
	ref       string
	index     int
	tokens    int
	chars     int
	isTool    bool
	isUser    bool
	gapBefore bool
	// blockBefore marks that an active block (its covered messages or its
	// rendered summary) lies between this message and the previous
	// compressible one.
	blockBefore bool
}

// BuildCompressibleRanges walks the view once and groups contiguous
// compressible messages, breaking the run wherever something interrupts it.
func BuildCompressibleRanges(msgs []CoreMessage, state CompressionState, cfg Config,
	protectedRefs map[string]bool, count TokenCountFn) ([]RangeInfo, []ProtectedRange) {

	if count == nil {
		count = DefaultCountTokens
	}
	covered := CoveredMessageIDs(state)
	protectedCallIDs := collectProtectedToolCallIDs(msgs, cfg)

	var compressible []scanned
	var protectedRuns []ProtectedRange
	var curProtected *ProtectedRange

	gapSinceCompressible := false
	blockSinceCompressible := false
	flushProtected := func() {
		if curProtected != nil {
			protectedRuns = append(protectedRuns, *curProtected)
			curProtected = nil
		}
	}

	for idx, m := range msgs {
		if isRenderedSummaryMessage(m) {
			// A rendered summary carries no ref, but it stands for an active
			// block: a range spanning it would span the block. In a pruned
			// view it is the only trace the block leaves, so it must interrupt
			// the run here rather than be skipped as unaddressable.
			gapSinceCompressible = true
			blockSinceCompressible = true
			flushProtected()
			continue
		}
		ref, hasRef := state.MessageRefs.ByRaw[m.ID]
		if m.ID == "" || !hasRef || ref == BlockedRef {
			// Unaddressable: it interrupts nothing, because the model could not
			// have named it either way.
			continue
		}
		switch {
		case covered[m.ID] || isRenderedSummaryMessage(m) || strings.HasPrefix(m.Text, SummaryHeader):
			gapSinceCompressible = true
			blockSinceCompressible = true
			flushProtected()
		case isMessageProtectedWithPairing(m, cfg, protectedCallIDs):
			gapSinceCompressible = true
			tk := CountMessageTokens(m, count)
			if curProtected == nil {
				curProtected = &ProtectedRange{StartRef: ref, EndRef: ref, Count: 1, Tokens: tk,
					StartIndex: idx, EndIndex: idx, Positioned: true}
			} else {
				curProtected.EndRef = ref
				curProtected.EndIndex = idx
				curProtected.Count++
				curProtected.Tokens += tk
			}
			if m.ToolName != "" && !containsString(curProtected.Tools, m.ToolName) {
				curProtected.Tools = append(curProtected.Tools, m.ToolName)
			}
		case protectedRefs[ref]:
			gapSinceCompressible = true
			flushProtected()
		default:
			flushProtected()
			compressible = append(compressible, scanned{
				id:        m.ID,
				ref:       ref,
				index:     idx,
				tokens:    CountMessageTokens(m, count),
				chars:     jsLen(m.Text),
				isTool:    m.ContentType == CTToolCall || m.ContentType == CTToolResult,
				isUser:    m.Role == RoleUser,
				gapBefore: gapSinceCompressible,

				blockBefore: blockSinceCompressible,
			})
			gapSinceCompressible = false
			blockSinceCompressible = false
		}
	}
	flushProtected()

	return groupCompressible(screenUnfoldable(msgs, compressible)), protectedRuns
}

// screenUnfoldable drops every message the fold-integrity gate would withdraw
// if the whole compressible set were folded, and marks a gap where it was.
//
// The protected-zone carve removes individual messages after a range is built,
// so a range that brushes the zone can hold only messages whose turn would lose
// its reasoning run, or whose call/result pair would split. Advertised as
// compressible, such a range is rejected the moment the model submits it.
// Running the same gate ApplyCompression runs keeps every advertised range
// foldable.
func screenUnfoldable(msgs []CoreMessage, items []scanned) []scanned {
	if len(items) == 0 {
		return items
	}
	ids := make(map[string]bool, len(items))
	for _, it := range items {
		ids[it.id] = true
	}
	iw := ComputeIntegrityWithdrawals(msgs, ids)
	if len(iw.Withdrawn) == 0 {
		return items
	}
	kept := make([]scanned, 0, len(items))
	gap := false
	for _, it := range items {
		if iw.Withdrawn[it.id] {
			gap = true
			continue
		}
		if gap {
			it.gapBefore = true
			gap = false
		}
		kept = append(kept, it)
	}
	return kept
}

// groupCompressible cuts the scanned run into ranges.
//
// A group also breaks at a user message once the group already holds three
// messages: user turns are natural seams, and a summary that spans several
// unrelated requests is much harder to write well.
func groupCompressible(items []scanned) []RangeInfo {
	var out []RangeInfo
	var cur *RangeInfo
	var curToolCount int

	flush := func() {
		if cur == nil {
			return
		}
		if cur.Count > 0 {
			cur.ToolPct = roundHalfUp(float64(curToolCount) * 100 / float64(cur.Count))
			cur.TextPct = 100 - cur.ToolPct
		}
		out = append(out, *cur)
		cur = nil
		curToolCount = 0
	}

	for _, it := range items {
		if cur != nil && (it.gapBefore || (it.isUser && cur.Count >= 3)) {
			flush()
		}
		if cur == nil {
			cur = &RangeInfo{StartRef: it.ref, EndRef: it.ref, StartIndex: it.index, Positioned: true,
				BlockBefore: it.blockBefore}
		}
		cur.EndRef = it.ref
		cur.EndIndex = it.index
		cur.Count++
		cur.Tokens += it.tokens
		cur.Chars += it.chars
		if it.isTool {
			curToolCount++
		}
		if it.isUser {
			cur.UserMsgs++
		}
	}
	flush()
	return out
}

// MergeRangesToThreshold coalesces adjacent ranges until each batch clears
// minChars, so the model is not handed fragments that would fail the atomic
// size gate the moment it submits them.
//
// Invariant: EVERY returned batch alone clears minChars. A batch never
// stretches across an active block (RangeInfo.BlockBefore): a span covering
// one sends the model into "already compressed" the moment it picks a
// sub-range. A sub-threshold segment closed by a block is dropped; a
// sub-threshold tail is folded into the batch before it when no block
// separates them, and dropped otherwise, since no selection of it can pass the
// gate.
//
// Ported from acp-kernel (MIT, Copyright © 2026 ranxianglei):
// src/recommend.ts, mergeRangesToThreshold.
//
// Deviation (deliberate): upstream closes a batch at ANY positional gap, which
// includes soft- and hard-protected messages. In fold that starves the
// recommendation. Bulky re-obtainable tool output (NeverPreserveRecentTools)
// takes no slot in the recent zone, so on a read-heavy session the short
// reasoning and text between reads all fall inside the zone's token budget;
// every read is then its own sub-threshold island and nothing would ever be
// advertised. Bridging protected messages is harmless — ApplyCompression
// excludes them and says so — so only active blocks close a batch here.
func MergeRangesToThreshold(ranges []RangeInfo, minChars int) []RangeInfo {
	if minChars <= 0 || len(ranges) == 0 {
		return ranges
	}
	var out []RangeInfo
	var batch []RangeInfo
	batchChars := 0

	closeBatch := func() {
		if len(batch) > 0 && batchChars >= minChars {
			out = append(out, mergeBatch(batch))
		}
		batch, batchChars = nil, 0
	}

	for _, r := range ranges {
		if len(batch) > 0 && r.BlockBefore {
			closeBatch()
		}
		batch = append(batch, r)
		batchChars += r.Chars
		if batchChars >= minChars {
			closeBatch()
		}
	}
	if len(batch) > 0 && len(out) > 0 {
		prev := out[len(out)-1]
		switch {
		case !batch[0].BlockBefore:
			out[len(out)-1] = mergeBatch(append([]RangeInfo{prev}, batch...))
		case batchChars >= minChars:
			out = append(out, mergeBatch(batch))
		}
	}
	return out
}

// mergeBatch folds consecutive ranges into one. The tool share is a
// count-weighted average.
func mergeBatch(batch []RangeInfo) RangeInfo {
	out := batch[0]
	weightedTool, totalCount := out.ToolPct*out.Count, out.Count
	for _, r := range batch[1:] {
		out.EndRef = r.EndRef
		out.Count += r.Count
		out.Tokens += r.Tokens
		out.Chars += r.Chars
		out.UserMsgs += r.UserMsgs
		out.Dangerous = out.Dangerous || r.Dangerous
		weightedTool += r.ToolPct * r.Count
		totalCount += r.Count
		if out.Positioned && r.Positioned {
			out.StartIndex = min(out.StartIndex, r.StartIndex)
			out.EndIndex = max(out.EndIndex, r.EndIndex)
		} else {
			out.Positioned = false
		}
	}
	if totalCount > 0 {
		out.ToolPct = roundHalfUp(float64(weightedTool) / float64(totalCount))
		out.TextPct = 100 - out.ToolPct
	}
	return out
}

// ViableRanges drops fragments too small to be worth a turn.
//
// Without this filter a tiny range dragged into a batch makes the whole batch
// fail the character gate, and models tend to resubmit the identical batch.
func ViableRanges(ranges []RangeInfo) []RangeInfo {
	out := make([]RangeInfo, 0, len(ranges))
	for _, r := range ranges {
		if r.Tokens >= ViableRangeMinTokens {
			out = append(out, r)
		}
	}
	return out
}

// BuildRecommendation is the full recommendation pass.
func BuildRecommendation(msgs []CoreMessage, state CompressionState, cfg Config, count TokenCountFn) Recommendation {
	protectedRefs := ComputeProtectedRefs(msgs, state, cfg)
	ranges, protectedRuns := BuildCompressibleRanges(msgs, state, cfg, protectedRefs, count)
	merged := MergeRangesToThreshold(ranges, cfg.Compress.MinCompressRange)
	viable := ViableRanges(merged)
	return Recommendation{
		CompressibleRanges: viable,
		ProtectedRanges:    protectedRuns,
		NothingToCompress:  len(viable) == 0,
	}
}

// ActiveBlockSpans summarises the active blocks for the nudge's block map.
func ActiveBlockSpans(state CompressionState) []BlockSpan {
	active := ActiveBlocks(state)
	out := make([]BlockSpan, 0, len(active))
	for _, b := range active {
		start, end := b.StartRef, b.EndRef
		// Only message refs are a span. A block whose stored endpoints are
		// block ids (or empty, on state from elsewhere) would otherwise render
		// as an uninterpretable "b1–b4"; derive the span from its coverage.
		if _, ok := RefToIndex(start); !ok {
			start, end = refSpan(b.EffectiveMessageIDs, state.MessageRefs)
		} else if _, ok := RefToIndex(end); !ok {
			start, end = refSpan(b.EffectiveMessageIDs, state.MessageRefs)
		}
		if start == "" {
			continue
		}
		out = append(out, BlockSpan{BlockID: b.BlockID, Tier: b.Tier, StartRef: start, EndRef: end})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := RefToIndex(out[i].StartRef)
		b, _ := RefToIndex(out[j].StartRef)
		return a < b
	})
	return out
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
