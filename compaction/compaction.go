// Package compaction keeps a conversation within a model's context window using
// three mechanisms aligned 1:1 with claude-code:
//
//   - MicroCompact — clear stale COMPACTABLE tool-result bodies (non-destructive:
//     the result can be re-read). Keeps the most recent N compactable results and
//     clears the rest, in batches: a pass runs only when it frees enough to be
//     worth rewriting the cached prompt prefix, and the history is left
//     byte-stable between passes.
//   - AutoCompact  — summarize the working head behind a compact boundary at the
//     token threshold (or predictively). Non-destructive: the full history is
//     retained in the message array; only messages after the last boundary are
//     sent to the model (llm.MessagesForAPI).
//   - Reactive     — on a prompt-too-long (413) error, force one AutoCompact and
//     retry. It reuses AutoCompact; it is not a separate algorithm.
//
// Snip and Context-Collapse are intentionally absent: snip's removal decision can
// only come from a model tool / user command (no automatic trigger), and
// context-collapse is disabled by default in claude-code. Thresholds are derived
// from the model's context window (minus a summary reservation and a buffer),
// with a predictive pre-request check. Token counts prefer the previous model
// response's input_tokens and otherwise fall back to a 4/3-scaled per-block
// length estimate.
package compaction

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/tool"
)

// Summarizer condenses messages into a digest. Injected so the host controls
// which model summarizes (and so tests can mock). msgs is the part of the
// conversation the summary replaces. The context carries a SummaryCall
// (SummaryCallFrom) with the run's request prefix and the whole conversation,
// for a summarizer that shares the run's prompt cache, and takes the usage of
// the call (SummaryCall.AddUsage).
type Summarizer func(ctx context.Context, msgs []llm.Message) (string, error)

// Config tunes the compactor.
type Config struct {
	// ContextWindow is the model's total context window in tokens (default 200000).
	ContextWindow int
	// MaxOutputTokens is the model's max output capability; the summary reservation
	// is min(MaxOutputTokens, 20000) (default 8000).
	MaxOutputTokens int
	// KeepRecent caps how many recent messages AutoCompact preserves as the
	// un-summarized tail (default 8). The tail always starts at a turn
	// boundary, so it may hold fewer. MicroCompact uses its own microKeepRecent.
	KeepRecent int
	// KeepRecentTokens is the token budget of that tail: the most recent
	// turns are kept while they fit in it. Default: a fifth of the effective
	// window. Counting messages alone kept eight messages however large, so a
	// few huge tool results could leave nothing to summarize; and it cut
	// wherever the count fell, including between a tool call and its result.
	KeepRecentTokens int
	// BreakerCooldown is how long AutoCompact stays off after its summarizer
	// failed three times in a row. When it has passed, the next compaction is
	// tried once: success closes the breaker, another failure starts a new
	// cool-down. Default five minutes; negative keeps the breaker open for the
	// rest of the session.
	BreakerCooldown time.Duration
	// MicroMinFree is the least a MicroCompact pass must free, in tokens, to
	// run. Clearing a tool result rewrites the conversation from that point
	// on, so every request after it pays to write the prompt cache again; a
	// pass that frees one result is not worth that. Old results are therefore
	// left alone until clearing them all frees at least this much, then
	// cleared in one pass, and the history stays byte-identical until the next
	// one. Default: a tenth of the effective window. Negative clears as soon
	// as anything can be (one result per turn, as before).
	MicroMinFree int
	// Summarizer, when set, replaces the summarizer passed to New. It is how a
	// host that builds its session through agentcore.Options.Compaction picks
	// its own: StandaloneSummarizer(cheapProvider, ...) summarizes with a
	// separate, cheaper model. The default (ProviderSummarizer on the session's
	// provider) summarizes over the run's own prefix and reads its prompt
	// cache, which a different model cannot do — weigh a cheaper model's price
	// against paying for the history uncached.
	Summarizer Summarizer
}

func (c *Config) defaults() {
	if c.ContextWindow == 0 {
		c.ContextWindow = 200000
	}
	if c.MaxOutputTokens == 0 {
		c.MaxOutputTokens = 8000
	}
	if c.KeepRecent == 0 {
		c.KeepRecent = 8
	}
	if c.BreakerCooldown == 0 {
		c.BreakerCooldown = defaultBreakerCooldown
	}
}

// compactableTools — tools whose results MicroCompact may clear. Mirrors
// claude-code COMPACTABLE_TOOLS (file read, shell, grep/glob, web, edit/write);
// deliberately excludes LS/MultiEdit/NotebookEdit.
var compactableTools = map[string]bool{
	tool.ReadName: true, tool.BashName: true, tool.GrepName: true, tool.GlobName: true,
	tool.WebFetchName: true, tool.WebSearchName: true, tool.EditName: true, tool.WriteName: true,
}

// CompactableTools lists the tool names whose results MicroCompact may clear.
// Every name must be a real tool name (the agentcore tests check this against
// the default toolset), or that tool's old results are never cleared.
func CompactableTools() []string {
	out := make([]string, 0, len(compactableTools))
	for n := range compactableTools {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

const (
	microClearedMessage   = "[Old tool result content cleared]" // == claude-code TIME_BASED_MC_CLEARED_MESSAGE
	microKeepRecent       = 5                                   // == claude-code KEEP_RECENT (compactable results kept)
	toolResultGrowthGuess = 15000                               // == claude-code TOOL_RESULT_GROWTH_ESTIMATE
	maxAutoCompactFails   = 3                                   // == claude-code MAX_CONSECUTIVE_AUTOCOMPACT_FAILURES

	// defaultBreakerCooldown is how long a tripped breaker waits before letting
	// one compaction through again. Long enough that a summarizer that is
	// really broken is not hammered with a full-context request every turn,
	// short enough that an outage does not cost the session its compaction.
	defaultBreakerCooldown = 5 * time.Minute
)

// Compactor applies the strategy. Construct with New.
type Compactor struct {
	cfg       Config
	summarize Summarizer
	failures  int
	// Circuit breaker over the summarizer. tripped is the open state; retryAt
	// is when it turns half-open (zero: never). See breakerAllows.
	tripped bool
	retryAt time.Time
	now     func() time.Time // the clock; replaced in tests
	// lastReason is why the most recent autoCompact did nothing ("" if it ran).
	lastReason Reason

	// mu guards prefix and usage, which the loop's goroutine and a host
	// reading TakeUsage may touch from different goroutines.
	mu        sync.Mutex
	prefix    llm.CompletionRequest
	hasPrefix bool
	usage     llm.Usage // summary usage not yet taken
}

// New builds a Compactor. summarize may be nil to disable AutoCompact.
func New(cfg Config, summarize Summarizer) *Compactor {
	cfg.defaults()
	if cfg.Summarizer != nil {
		summarize = cfg.Summarizer
	}
	return &Compactor{cfg: cfg, summarize: summarize, now: time.Now}
}

// SetRequestPrefix tells the compactor what every model request of the run
// starts with: its system prompt, tools and session settings, and in
// prefix.Messages whatever the loop sends in front of the conversation. The
// harness loop calls it when a run starts (harness.RequestPrefixAware). The
// summarizer builds its request on it so that it reads the run's prompt cache.
func (c *Compactor) SetRequestPrefix(prefix llm.CompletionRequest) {
	c.mu.Lock()
	c.prefix, c.hasPrefix = prefix, true
	c.mu.Unlock()
}

// TakeUsage returns what the summary calls made since the last TakeUsage cost,
// and resets the count. The harness loop adds it to the run's usage
// (harness.CompactionUsageReporter); a host driving the compactor itself can
// call it after Pre or Reactive.
func (c *Compactor) TakeUsage() llm.Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.usage
	c.usage = llm.Usage{}
	return u
}

// summaryCall builds the SummaryCall for a summary over msgs. overflowed is
// true when the run's last request was rejected for size: the conversation
// offered to the summarizer is then only the part being summarized.
func (c *Compactor) summaryCall(msgs, head []llm.Message, overflowed bool) *SummaryCall {
	c.mu.Lock()
	call := &SummaryCall{Prefix: c.prefix, HasPrefix: c.hasPrefix}
	c.mu.Unlock()
	if overflowed {
		call.Conversation = pairForReplay(head)
	} else {
		call.Conversation = llm.MessagesForAPI(msgs)
	}
	return call
}

// --- circuit breaker ---

// trip opens the breaker: AutoCompact is skipped until the cool-down passes.
func (c *Compactor) trip() {
	c.tripped = true
	c.retryAt = time.Time{}
	if c.cfg.BreakerCooldown > 0 {
		c.retryAt = c.now().Add(c.cfg.BreakerCooldown)
	}
}

// breakerAllows reports whether a summary may be attempted. A closed breaker
// always allows it. An open one allows a single probe once its cool-down has
// passed (half-open): summaryFailed re-opens it at the first failure,
// summarySucceeded closes it.
func (c *Compactor) breakerAllows() bool {
	if !c.tripped {
		return true
	}
	return !c.retryAt.IsZero() && !c.now().Before(c.retryAt)
}

// summaryFailed counts a summarizer failure. Three in a row open the breaker;
// a failed half-open probe re-opens it straight away.
func (c *Compactor) summaryFailed() {
	c.failures++
	if c.tripped || c.failures >= maxAutoCompactFails {
		c.trip()
	}
}

// summarySucceeded closes the breaker and forgets earlier failures.
func (c *Compactor) summarySucceeded() {
	c.failures = 0
	c.tripped = false
	c.retryAt = time.Time{}
}

// --- threshold math ---

// reservedForSummary is min(model max output, 20000). MaxOutputTokens stands in
// for the model's output capability; 0 (unset) reserves the full 20000.
func (c *Compactor) reservedForSummary() int {
	r := c.cfg.MaxOutputTokens
	if r > 20000 || r == 0 {
		r = 20000
	}
	return r
}
func (c *Compactor) effectiveWindow() int { return c.cfg.ContextWindow - c.reservedForSummary() }

// buffer tiers off the EFFECTIVE window (== claude-code getAutocompactBufferTokens).
func (c *Compactor) buffer() int {
	switch ew := c.effectiveWindow(); {
	case ew >= 800000:
		return 50000
	case ew >= 400000:
		return 30000
	default:
		return 13000
	}
}
func (c *Compactor) autoThreshold() int  { return c.effectiveWindow() - c.buffer() }
func (c *Compactor) microThreshold() int { return c.effectiveWindow() * 6 / 10 }

// microMinFree is the least a MicroCompact pass must free to be worth breaking
// the cached prefix (see Config.MicroMinFree).
func (c *Compactor) microMinFree() int {
	switch m := c.cfg.MicroMinFree; {
	case m > 0:
		return m
	case m < 0:
		return 1
	}
	return max(c.effectiveWindow()/10, 1)
}

// summaryDue reports whether a context of tok tokens calls for AutoCompact.
func (c *Compactor) summaryDue(tok int) bool {
	return tok >= c.autoThreshold() || c.predictiveOver(tok)
}

// maxTurnGrowth == claude-code estimateMaxTurnGrowth: min(maxOutput,20000)+15000.
func (c *Compactor) maxTurnGrowth() int { return c.reservedForSummary() + toolResultGrowthGuess }
func (c *Compactor) predictiveOver(tok int) bool {
	return tok+c.maxTurnGrowth() > c.effectiveWindow()
}

// count returns the token count of the API view (post-boundary). When
// lastInputTokens > 0 (the total token size the previous model response
// reported), it is preferred over local estimation — mirroring claude-code's
// tokenCountWithEstimation. After a working-set mutation, callers pass 0 to force
// a fresh per-block estimate.
func (c *Compactor) count(msgs []llm.Message, lastInputTokens int) int {
	if lastInputTokens > 0 {
		return lastInputTokens
	}
	return EstimateTokens(llm.MessagesForAPI(msgs)) * 4 / 3
}

// EstimateTokens approximates the token footprint of messages (~4 chars/token),
// counting each block by type (text, thinking text, tool_use name+input, and
// nested tool_result content). Mirrors claude-code roughTokenCountEstimation
// (without the 4/3 scale, which count applies).
func EstimateTokens(msgs []llm.Message) int {
	chars := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			chars += blockChars(b)
		}
	}
	return chars / 4
}

// blockChars is the per-block character footprint. A Corvid ContentBlock has no
// image/document type; if one is added, its fixed 2000-token cost (claude-code
// IMAGE_MAX_TOKEN_SIZE) would be added here.
func blockChars(b llm.ContentBlock) int {
	switch b.Type {
	case llm.BlockText:
		return len(b.Text)
	case llm.BlockThinking:
		return len(b.Thinking)
	case llm.BlockToolUse:
		return len(b.Name) + len(b.Input)
	case llm.BlockToolResult:
		n := 0
		for _, cb := range b.Content {
			n += blockChars(cb)
		}
		return n
	default:
		return len(b.Text)
	}
}

// Pre conditions the history before a model request: MicroCompact to clear stale
// tool results, then AutoCompact at the threshold (or predictively). Both are
// non-destructive. lastInputTokens is the previous response's total token size
// (0 if none yet), used as the initial gate count.
//
// Most calls return msgs untouched, and that is the point: a provider's prompt
// cache matches the request prefix byte for byte, so every edit to an earlier
// message makes the next request pay for everything after it again. Pre edits
// the history only in passes large enough to be worth that:
//
//   - below the micro threshold: nothing;
//   - above it, with no summary due: one MicroCompact pass once the old tool
//     results add up to MicroMinFree, nothing until then;
//   - with a summary due: the MicroCompact pass instead, if it is large enough
//     and gets the context back under the summary threshold; otherwise the
//     summary, made over the still-unedited history so that it reads the cache.
func (c *Compactor) Pre(ctx context.Context, msgs []llm.Message, lastInputTokens int) []llm.Message {
	if c.cfg.ContextWindow <= 0 {
		return msgs
	}
	tok := c.count(msgs, lastInputTokens)
	if tok <= c.microThreshold() {
		return msgs
	}
	freed := c.microFreeable(msgs)
	due := c.summaryDue(tok)
	switch {
	case freed == 0:
		// Nothing to clear.
	case !due:
		if freed >= c.microMinFree() {
			msgs, _ = c.microCompact(msgs)
		}
		return msgs
	case freed >= c.microMinFree() && !c.summaryDue(tok-freed):
		// Clearing alone gets back under the threshold: no summary needed.
		msgs, _ = c.microCompact(msgs)
		return msgs
	case c.summarize == nil || !c.breakerAllows():
		// A summary is due and cannot be made. Clearing is the only relief
		// left, whatever it frees: an overflow costs more than a cache write.
		msgs, _ = c.microCompact(msgs)
		return msgs
	}
	// AutoCompact: at the threshold, or predictively if the next turn would overflow.
	if due {
		if out, ok := c.autoCompact(ctx, msgs, tok, "auto"); ok {
			msgs = out
		}
	}
	return msgs
}

// Reactive force-compacts after a prompt-too-long error by running one
// AutoCompact (mirrors claude-code tryReactiveCompact, which only summarizes —
// no micro/collapse/snip cascade). Returns whether the API view shrank.
func (c *Compactor) Reactive(ctx context.Context, msgs []llm.Message) ([]llm.Message, bool) {
	before := c.count(msgs, 0)
	if out, ok := c.autoCompact(ctx, msgs, before, "reactive"); ok {
		return out, c.count(out, 0) < before
	}
	return msgs, false
}

// IsOverflow is the method form (satisfies the harness Compactor interface).
func (c *Compactor) IsOverflow(err error) bool { return IsOverflow(err) }

// IsOverflow reports whether err is a context-length / prompt-too-long failure.
//
// The code decides first: the provider adapters classify the rejections they
// recognise as llm.CodeContextOverflow, and an error they classified as
// something else is not an overflow. Only an error that carries no code (or
// llm.CodeUnknown) — from a custom Provider, a wrapper, or an adapter that saw
// a phrasing it did not know — falls back to matching the text
// (llm.LooksLikeOverflow).
func IsOverflow(err error) bool {
	if err == nil {
		return false
	}
	switch llm.CodeOf(err) {
	case llm.CodeContextOverflow:
		return true
	case llm.CodeUnknown:
		return llm.LooksLikeOverflow(err.Error())
	default:
		return false
	}
}

// --- working-set geometry ---

// --- MicroCompact: keep the most recent N compactable results, clear the rest ---

// microClearSet returns the tool_use ids whose results a MicroCompact pass
// clears: every compactable call in the working set except the most recent
// microKeepRecent. Nil when there are none.
func microClearSet(msgs []llm.Message) map[string]bool {
	var ids []string
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolUse && compactableTools[b.Name] {
				ids = append(ids, b.ID)
			}
		}
	}
	if len(ids) <= microKeepRecent {
		return nil
	}
	clearSet := make(map[string]bool, len(ids)-microKeepRecent)
	for _, id := range ids[:len(ids)-microKeepRecent] {
		clearSet[id] = true
	}
	return clearSet
}

// microFreeable estimates how many tokens a MicroCompact pass over msgs would
// free, on the same scale as count: the bodies it would clear, less the
// placeholder left in their place. Only the working set (after the last
// boundary) counts, since that is all that is sent.
func (c *Compactor) microFreeable(msgs []llm.Message) int {
	clearSet := microClearSet(msgs)
	if clearSet == nil {
		return 0
	}
	chars := 0
	for _, m := range llm.MessagesAfterBoundary(msgs) {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult && clearSet[b.ToolUseID] && !isCleared(b.Content) {
				if n := blockChars(b) - len(microClearedMessage); n > 0 {
					chars += n
				}
			}
		}
	}
	return chars / 4 * 4 / 3
}

// microCompact is one MicroCompact pass: it clears the bodies of all COMPACTABLE
// tool results except the most recent microKeepRecent, returning the (possibly
// new) message slice and whether anything changed. Only a result's body is
// replaced; the tool_use block and its tool_result stay paired. It is
// non-destructive: a cleared result can be re-read. Pre decides when a pass is
// worth running.
func (c *Compactor) microCompact(msgs []llm.Message) ([]llm.Message, bool) {
	clearSet := microClearSet(msgs)
	if clearSet == nil {
		return msgs, false // nothing old enough to clear
	}
	changed := false
	out := append([]llm.Message(nil), msgs...)
	for i := range out {
		var nc []llm.ContentBlock
		for j, b := range out[i].Content {
			if b.Type == llm.BlockToolResult && clearSet[b.ToolUseID] && !isCleared(b.Content) {
				if nc == nil {
					nc = append([]llm.ContentBlock(nil), out[i].Content...)
				}
				nc[j].Content = []llm.ContentBlock{llm.TextBlock(microClearedMessage)}
				changed = true
			}
		}
		if nc != nil {
			out[i].Content = nc
		}
	}
	if !changed {
		return msgs, false
	}
	return out, true
}

func isCleared(blocks []llm.ContentBlock) bool {
	return len(blocks) == 1 && blocks[0].Type == llm.BlockText && blocks[0].Text == microClearedMessage
}

// --- AutoCompact: non-destructive boundary + summary ---

func (c *Compactor) autoCompact(ctx context.Context, msgs []llm.Message, preTok int, trigger string) ([]llm.Message, bool) {
	c.lastReason = ""
	switch {
	case c.summarize == nil:
		c.lastReason = ReasonNoSummarizer
		return msgs, false
	case !c.breakerAllows():
		c.lastReason = ReasonBreakerOpen
		return msgs, false
	}
	start, tailStart, reason := c.workingBounds(msgs)
	if reason != "" {
		c.lastReason = reason
		return msgs, false
	}
	toSummarize := contentOnly(msgs[start:tailStart])
	call := c.summaryCall(msgs, toSummarize, trigger == "reactive")
	summary, err := c.summarize(context.WithValue(ctx, summaryCallKey{}, call), toSummarize)
	c.mu.Lock()
	c.usage.Add(call.usage)
	c.mu.Unlock()
	if err != nil {
		// A cancelled run is not a summarizer failure: counting it would open
		// the breaker on a session that was merely interrupted.
		if ctx.Err() == nil {
			c.summaryFailed()
		}
		return msgs, false
	}
	c.summarySucceeded()

	// Skill instructions in the summarized head are lossy in the prose summary, so
	// re-inject the latest invocation of each skill verbatim just after the summary
	// (post-boundary → sent to the model). Retained history keeps the originals; we
	// only re-surface skills that fell behind the new boundary.
	reinjected := reinjectSkills(msgs, tailStart)
	boundary := llm.BoundaryMessage(llm.BoundaryMeta{
		Trigger:            trigger,
		PreTokens:          preTok,
		MessagesSummarized: len(toSummarize),
		ActiveSkills:       skillNames(reinjected),
	})
	summaryMsg := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
		llm.TextBlock(compactContinuationPreamble + summary),
	}}
	// Retain everything before tailStart (non-destructive); insert the new
	// boundary + summary (+ re-injected skills) just before the preserved recent tail.
	out := make([]llm.Message, 0, len(msgs)+2+len(reinjected))
	out = append(out, msgs[:tailStart]...)
	out = append(out, boundary, summaryMsg)
	out = append(out, reinjected...)
	out = append(out, msgs[tailStart:]...)
	return out, true
}

// compactContinuationPreamble prefixes the inserted summary message, mirroring
// claude-code getCompactUserSummaryMessage. formatCompactSummary already prefixes
// the digest with "Summary:\n".
const compactContinuationPreamble = "This session is being continued from a previous conversation that ran out of context. The conversation is summarized below:\n\n"

// Skill re-injection budget: per-skill and total token caps for verbatim skill
// instructions re-surfaced after a compaction boundary (mirrors claude-code's
// 5K/25K policy). Most-recently-invoked skills win when the total is exceeded.
const (
	maxSkillReinjectTokens      = 5000
	maxSkillReinjectTotalTokens = 25000
)

// skillNames returns the invoked-skill names of the given re-injected messages.
func skillNames(msgs []llm.Message) []string {
	var out []string
	for _, m := range msgs {
		if name, ok := llm.SkillInvocationName(m); ok {
			out = append(out, name)
		}
	}
	return out
}

// reinjectSkills finds the latest invocation of each skill in the history behind
// the new boundary (msgs[:tailStart]) and returns copies to place after the
// summary, newest-first under budget, restored to chronological order. A skill
// whose latest invocation is already in the preserved tail (index >= tailStart)
// is skipped — it is still in the model's view.
func reinjectSkills(msgs []llm.Message, tailStart int) []llm.Message {
	type entry struct {
		msg llm.Message
		idx int
	}
	latest := map[string]*entry{}
	var order []string
	for i, m := range msgs {
		name, ok := llm.SkillInvocationName(m)
		if !ok {
			continue
		}
		if e, seen := latest[name]; seen {
			e.msg, e.idx = m, i
		} else {
			latest[name] = &entry{msg: m, idx: i}
			order = append(order, name)
		}
	}
	if len(latest) == 0 {
		return nil
	}
	// Candidates whose latest invocation fell behind the boundary, newest-first.
	var cands []*entry
	for _, name := range order {
		if e := latest[name]; e.idx < tailStart {
			cands = append(cands, e)
		}
	}
	sort.Slice(cands, func(a, b int) bool { return cands[a].idx > cands[b].idx })

	kept := make([]*entry, 0, len(cands))
	total := 0
	for _, e := range cands {
		m := llm.TruncateSkillMessage(e.msg, maxSkillReinjectTokens*4)
		t := EstimateTokens([]llm.Message{m})
		if total+t > maxSkillReinjectTotalTokens {
			continue // drop this one; a smaller, older skill may still fit
		}
		total += t
		kept = append(kept, &entry{msg: m, idx: e.idx})
	}
	if len(kept) == 0 {
		return nil
	}
	sort.Slice(kept, func(a, b int) bool { return kept[a].idx < kept[b].idx })
	out := make([]llm.Message, 0, len(kept))
	for _, e := range kept {
		out = append(out, e.msg)
	}
	return out
}

func contentOnly(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		if !llm.IsBoundaryMarker(m) {
			out = append(out, m)
		}
	}
	return out
}
