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
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	// cool-down. Default five minutes; negative turns the cool-down off
	// (with BreakerRegrowth negative too, the breaker then stays open for the
	// rest of the session).
	BreakerCooldown time.Duration
	// BreakerRegrowth is the other way a tripped breaker turns half-open: the
	// conversation has grown by this many tokens since it tripped. A summary
	// that failed on one conversation may well work on a longer, different
	// one, and by then the need is greater. Default: a tenth of the effective
	// window; negative leaves the cool-down as the only way back.
	BreakerRegrowth int
	// StaticPrefixTokens is the measured size, in tokens, of what every
	// request carries besides the conversation: the system prompt and the
	// tool schemas. The local estimate only sees messages, so without it a
	// session with a large tool list is counted tens of thousands of tokens
	// short whenever the provider's own count is not available (the first
	// request, and after every pass). Left zero it is estimated from the
	// request prefix the loop announces (SetRequestPrefix).
	StaticPrefixTokens int
	// TokenCalibration scales the local estimate, which assumes four
	// characters per token: tokens = characters / 4 × TokenCalibration. The
	// default, 4/3, suits prose and code. Hex, base64 and dense JSON run
	// closer to two characters per token: a session full of such tool output
	// wants 2 or more. Left zero, the factor is learned from the provider's
	// reported usage whenever a response carries it.
	TokenCalibration float64
	// OnEvent, when set, is called for every compaction pass and for every
	// pass that was due and did not happen, with the reason (see Event). Both
	// the passes Pre makes on its own and the ones Reactive is asked for go
	// through it, so a host can record each one, with tokens before and
	// after. It is called on the goroutine that called Pre or Reactive.
	OnEvent func(Event)
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

	// AfterCompact, when set, is called each time a summary has been made,
	// before the compacted view is accepted. The messages it returns are placed
	// right after the summary (and after any skill instructions put back
	// there), ahead of the kept tail: the first thing the model reads once the
	// detail is gone. It is the place for what a prose summary loses and the
	// work cannot continue without — a note on how to recover something, the
	// current plan, a list of open items. See Compacted for what it is shown.
	//
	// Only user messages are used; anything else is dropped, since the view
	// must go on reading summary (user), assistant, user, …. What is returned
	// counts toward the size of the new view, and a view that is not smaller
	// than the old one is thrown away, so keep it short. The messages become
	// part of the history and of the transcript. It is called on the goroutine
	// that called Pre or Reactive.
	AfterCompact func(ctx context.Context, c Compacted) []llm.Message
	// MicroStub, when set, writes the text left in place of a tool result that
	// MicroCompact clears. It receives the result as it stands and returns the
	// stub. Use it to keep whatever lets the content be found again — an id, a
	// path, a locator the tool printed — in a form the host recognises. Left
	// nil, DefaultMicroStub is used. An empty return also falls back to it. A
	// stub that is not shorter than the result it replaces is not applied.
	//
	// After a session is resumed the function can be shown a result that was
	// already cleared before the restart (its stub): return that unchanged.
	MicroStub func(result llm.ContentBlock) string
	// SummaryPrompt, when set, replaces the built-in summary instruction (the
	// last message of the summary request). DefaultSummaryPrompt returns the
	// built-in one, for a host that wants to extend it rather than replace it.
	// The built-in summarizers send it; a custom Summarizer reads it from
	// SummaryCall.Prompt. A replacement should keep asking for plain text and
	// no tool calls: the request lists the session's tools.
	SummaryPrompt string
	// References, when set, makes a "References" section mandatory in every
	// summary. It is called with the messages about to be summarized and
	// returns the identifiers that must survive them: ids, paths, URLs,
	// anything the model will need verbatim to find a thing again. The summary
	// instruction asks the model to list them; the compactor then checks the
	// summary and appends, under a References heading, every identifier that
	// is not in it word for word, in the order given. The result therefore
	// always holds all of them, whatever the model wrote.
	References func(summarized []llm.Message) []string
}

// Compacted describes a summary that has just been made, for
// Config.AfterCompact. The slices are the compactor's own: read, do not modify.
type Compacted struct {
	Trigger            string // "auto" or "reactive"
	PreTokens          int    // size of the context the pass was decided on
	MessagesSummarized int
	// Summary is the summary text as it will be inserted (References included).
	Summary string
	// Summarized is what the summary replaces; Tail is what is kept after it.
	Summarized, Tail []llm.Message
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
	// lastReason is why the most recent autoCompact did nothing ("" if it ran),
	// lastErr the summarizer error behind it, if any.
	lastReason Reason
	lastErr    error
	// lastRefsAdded is how many required references the most recent summary
	// left out and the compactor appended.
	lastRefsAdded int
	// viewTokens is the local estimate of the view autoCompact last looked at;
	// trippedAt is that estimate at the moment the breaker opened.
	viewTokens int
	trippedAt  int
	// rejected is set when Pre's summary was thrown away for not shrinking the
	// view, so that a Reactive call in the same turn does not pay for another.
	rejected bool
	// learned is the calibration factor measured from the provider's reported
	// usage (0 until one has been seen); see learn.
	learned float64

	// mu guards prefix and usage, which the loop's goroutine and a host
	// reading TakeUsage may touch from different goroutines.
	mu        sync.Mutex
	prefix    llm.CompletionRequest
	hasPrefix bool
	usage     llm.Usage // summary usage not yet taken

	// cleared maps the tool_use ids whose results this compactor has replaced
	// to the stub it left. A custom stub (Config.MicroStub) cannot be
	// recognised by its text alone, so this is how a cleared result is told
	// from a live one: the id is known and the content is still that stub.
	cleared map[string]string
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
// starts with: its system prompt, tools and session settings. The harness loop
// calls it when a run starts, and again if the tool list changes
// (harness.RequestPrefixAware). The summarizer builds its request on it so
// that it reads the run's prompt cache. prefix.Messages is for a caller that
// sends messages of its own in front of the conversation; the harness loop
// sends none.
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
func (c *Compactor) summaryCall(msgs, head []llm.Message, overflowed bool, refs []string) *SummaryCall {
	c.mu.Lock()
	call := &SummaryCall{Prefix: c.prefix, HasPrefix: c.hasPrefix}
	c.mu.Unlock()
	call.Prompt = c.summaryPrompt(refs)
	if overflowed {
		call.Conversation = pairForReplay(head)
	} else {
		call.Conversation = llm.MessagesForAPI(msgs)
	}
	return call
}

// --- circuit breaker ---

// trip opens the breaker: AutoCompact is skipped until the cool-down passes or
// the conversation has grown by BreakerRegrowth.
func (c *Compactor) trip() {
	c.tripped = true
	c.trippedAt = c.viewTokens
	c.retryAt = time.Time{}
	if c.cfg.BreakerCooldown > 0 {
		c.retryAt = c.now().Add(c.cfg.BreakerCooldown)
	}
}

// breakerAllows reports whether a summary may be attempted. A closed breaker
// always allows it. An open one allows a single probe (half-open) once its
// cool-down has passed, or once the conversation has grown by BreakerRegrowth
// tokens since it opened: summaryFailed re-opens it at the first failure,
// summarySucceeded closes it.
func (c *Compactor) breakerAllows() bool {
	if !c.tripped {
		return true
	}
	if !c.retryAt.IsZero() && !c.now().Before(c.retryAt) {
		return true
	}
	if g := c.breakerRegrowth(); g > 0 && c.viewTokens >= c.trippedAt+g {
		return true
	}
	return false
}

func (c *Compactor) breakerRegrowth() int {
	switch g := c.cfg.BreakerRegrowth; {
	case g > 0:
		return g
	case g < 0:
		return 0
	}
	return max(c.effectiveWindow()/10, 1)
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
//
// The estimate covers the whole request, not the messages alone: the static
// prefix (system prompt and tool schemas) is added, and the character count is
// scaled by the calibration factor (see estimate and prefixTokens).
func (c *Compactor) count(msgs []llm.Message, lastInputTokens int) int {
	if lastInputTokens > 0 {
		return lastInputTokens
	}
	return c.estimate(llm.MessagesForAPI(msgs)) + c.prefixTokens()
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
	case llm.BlockRedactedThinking:
		return len(b.Data)
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
	c.rejected = false // a new turn
	c.learn(msgs, lastInputTokens)
	tok := c.count(msgs, lastInputTokens)
	if tok <= c.microThreshold() {
		return msgs
	}
	freed := c.microFreeable(msgs)
	due := c.summaryDue(tok)
	c.viewTokens = c.estimate(llm.MessagesForAPI(msgs))
	switch {
	case freed == 0:
		// Nothing to clear.
	case !due:
		if freed >= c.microMinFree() {
			msgs = c.microPass(msgs, tok, freed)
		}
		return msgs
	case freed >= c.microMinFree() && !c.summaryDue(tok-freed):
		// Clearing alone gets back under the threshold: no summary needed.
		return c.microPass(msgs, tok, freed)
	case c.summarize == nil || !c.breakerAllows():
		// A summary is due and cannot be made. Clearing is the only relief
		// left, whatever it frees: an overflow costs more than a cache write.
		c.summaryPass(ctx, msgs, tok, "auto") // reports why no summary is made
		return c.microPass(msgs, tok, freed)
	}
	// AutoCompact: at the threshold, or predictively if the next turn would overflow.
	if due {
		msgs, _ = c.summaryPass(ctx, msgs, tok, "auto")
	}
	return msgs
}

// microPass runs one MicroCompact pass and reports it.
func (c *Compactor) microPass(msgs []llm.Message, tok, freed int) []llm.Message {
	before := c.clearedResults(msgs)
	out, changed := c.microCompact(msgs)
	if changed {
		c.emit(Event{Kind: KindMicro, Trigger: "auto", Applied: true,
			TokensBefore: tok, TokensAfter: max(tok-freed, 0), ResultsCleared: c.clearedResults(out) - before})
	}
	return out
}

// summaryPass runs autoCompact and reports the outcome: a pass with its token
// counts, or the reason nothing happened.
func (c *Compactor) summaryPass(ctx context.Context, msgs []llm.Message, tok int, trigger string) ([]llm.Message, bool) {
	out, ok := c.autoCompact(ctx, msgs, tok, trigger)
	ev := Event{Kind: KindSummary, Trigger: trigger, Applied: ok, TokensBefore: tok, TokensAfter: tok}
	if ok {
		// The saving is measured locally and taken off the count we came in
		// with, which is the provider's own when it reported one.
		saved := c.estimate(llm.MessagesForAPI(msgs)) - c.estimate(llm.MessagesForAPI(out))
		ev.TokensAfter = max(tok-saved, 0)
		if meta, found := llm.ParseBoundaryMeta(out[llm.LastBoundaryIndex(out)]); found {
			ev.MessagesSummarized = meta.MessagesSummarized
		}
		ev.ReferencesAdded = c.lastRefsAdded
	} else {
		ev.Reason, ev.Err = c.lastReason, c.lastErr
	}
	c.emit(ev)
	return out, ok
}

func (c *Compactor) emit(ev Event) {
	if c.cfg.OnEvent != nil {
		c.cfg.OnEvent(ev)
	}
}

// clearedResults counts the tool results MicroCompact has cleared in msgs.
func (c *Compactor) clearedResults(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult && c.isCleared(b) {
				n++
			}
		}
	}
	return n
}

// Reactive force-compacts after a prompt-too-long error by running one
// AutoCompact (mirrors claude-code tryReactiveCompact, which only summarizes —
// no micro/collapse/snip cascade). Returns whether the API view shrank.
//
// When Pre has just thrown a summary away on this same turn for not making the
// view smaller, Reactive does not ask for another: the same conversation would
// get the same answer, at the price of a second model call.
func (c *Compactor) Reactive(ctx context.Context, msgs []llm.Message) ([]llm.Message, bool) {
	before := c.count(msgs, 0)
	if c.rejected {
		c.emit(Event{Kind: KindSummary, Trigger: "reactive", Reason: ReasonSummaryRejected, TokensBefore: before, TokensAfter: before})
		return msgs, false
	}
	return c.summaryPass(ctx, msgs, before, "reactive")
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

// microStub is the text a cleared result is replaced with: Config.MicroStub's,
// or DefaultMicroStub's when there is none or it returns nothing.
func (c *Compactor) microStub(result llm.ContentBlock) string {
	if c.cfg.MicroStub != nil {
		if s := c.cfg.MicroStub(result); s != "" {
			return s
		}
	}
	return DefaultMicroStub(result)
}

// microStubFirstLineMax bounds the first line DefaultMicroStub keeps.
const microStubFirstLineMax = 120

// DefaultMicroStub is the stub MicroCompact leaves in place of a cleared tool
// result when Config.MicroStub is not set: the notice that the content was
// cleared, then the first line of the result (up to 120 bytes), then, when the
// output had been spilled to a store, the pointer to the full copy
// (tool.SpillPointer). The first line is where a tool names what it returned,
// and the pointer is how the whole of it is read again, so clearing a result
// does not lose the way back to it. A result with no text gets the notice
// alone.
func DefaultMicroStub(result llm.ContentBlock) string {
	var text string
	for _, b := range result.Content {
		if b.Type == llm.BlockText {
			text += b.Text
		}
	}
	if strings.HasPrefix(text, microClearedMessage) {
		return text // already a stub
	}
	stub := microClearedMessage
	first, _, _ := strings.Cut(strings.TrimLeft(text, "\r\n"), "\n")
	first = strings.TrimSpace(first)
	if len(first) > microStubFirstLineMax {
		cut := microStubFirstLineMax
		for cut > 0 && !utf8.RuneStart(first[cut]) {
			cut--
		}
		first = first[:cut] + "…"
	}
	if first != "" {
		stub += "\n" + first
	}
	if ptr, ok := tool.SpillPointer(text); ok && !strings.Contains(first, ptr) {
		stub += "\n" + ptr
	}
	return stub
}

// microFreeable estimates how many tokens a MicroCompact pass over msgs would
// free, on the same scale as count: the bodies it would clear, less the
// stub left in their place. Only the working set (after the last
// boundary) counts, since that is all that is sent.
func (c *Compactor) microFreeable(msgs []llm.Message) int {
	clearSet := microClearSet(msgs)
	if clearSet == nil {
		return 0
	}
	chars := 0
	for _, m := range llm.MessagesAfterBoundary(msgs) {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult && clearSet[b.ToolUseID] && !c.isCleared(b) {
				if n := blockChars(b) - len(c.microStub(b)); n > 0 {
					chars += n
				}
			}
		}
	}
	return c.scale(chars / 4)
}

// microCompact is one MicroCompact pass: it clears the bodies of all COMPACTABLE
// tool results except the most recent microKeepRecent, returning the (possibly
// new) message slice and whether anything changed. Only a result's body is
// replaced, by a stub that keeps what lets the content be found again
// (microStub); the tool_use block and its tool_result stay paired. It is
// non-destructive: a cleared result can be re-read. A stub that would not be
// shorter than the result is not applied. Pre decides when a pass is worth
// running.
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
			if b.Type == llm.BlockToolResult && clearSet[b.ToolUseID] && !c.isCleared(b) {
				stub := c.microStub(b)
				if len(stub) >= blockChars(b) {
					continue // clearing it would not make it smaller
				}
				if c.cleared == nil {
					c.cleared = map[string]string{}
				}
				c.cleared[b.ToolUseID] = stub
				if nc == nil {
					nc = append([]llm.ContentBlock(nil), out[i].Content...)
				}
				nc[j].Content = []llm.ContentBlock{llm.TextBlock(stub)}
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

// isCleared reports whether a tool result has already been through
// MicroCompact: this compactor cleared it, or it carries the default notice
// (a result cleared before a restart, or by another compactor).
func (c *Compactor) isCleared(result llm.ContentBlock) bool {
	blocks := result.Content
	if len(blocks) != 1 || blocks[0].Type != llm.BlockText {
		return false
	}
	if stub, ok := c.cleared[result.ToolUseID]; ok && stub == blocks[0].Text {
		return true
	}
	return strings.HasPrefix(blocks[0].Text, microClearedMessage)
}

// --- AutoCompact: non-destructive boundary + summary ---

// autoCompact summarizes the head of the working set behind a new boundary. It
// returns msgs unchanged and false when nothing was done; lastReason then says
// why. A summary is only accepted if the view it leaves is smaller than the one
// it replaces.
func (c *Compactor) autoCompact(ctx context.Context, msgs []llm.Message, preTok int, trigger string) ([]llm.Message, bool) {
	c.lastReason, c.lastErr, c.lastRefsAdded = "", nil, 0
	viewBefore := c.estimate(llm.MessagesForAPI(msgs))
	c.viewTokens = viewBefore
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
	var refs []string
	if c.cfg.References != nil {
		refs = cleanReferences(c.cfg.References(toSummarize))
	}
	call := c.summaryCall(msgs, toSummarize, trigger == "reactive", refs)
	summary, err := c.summarize(context.WithValue(ctx, summaryCallKey{}, call), toSummarize)
	c.mu.Lock()
	c.usage.Add(call.usage)
	c.mu.Unlock()
	if err != nil {
		// A cancelled run is not a summarizer failure: counting it would open
		// the breaker on a session that was merely interrupted.
		c.lastReason, c.lastErr = ReasonCancelled, err
		if ctx.Err() == nil {
			c.lastReason = ReasonSummaryFailed
			c.summaryFailed()
		}
		return msgs, false
	}

	// The identifiers the host said must survive are in the summary word for
	// word, whatever the model wrote: the missing ones are appended.
	summary, c.lastRefsAdded = ensureReferences(summary, refs)

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
	// What the host wants in front of the model right after the summary.
	var injected []llm.Message
	if c.cfg.AfterCompact != nil {
		for _, m := range c.cfg.AfterCompact(ctx, Compacted{
			Trigger: trigger, PreTokens: preTok, MessagesSummarized: len(toSummarize),
			Summary: summary, Summarized: toSummarize, Tail: msgs[tailStart:],
		}) {
			if m.Role == llm.RoleUser && len(m.Content) > 0 && !llm.IsBoundaryMarker(m) {
				injected = append(injected, m)
			}
		}
	}
	// Retain everything before tailStart (non-destructive); insert the new
	// boundary + summary (+ re-injected skills, + the host's messages) just
	// before the preserved recent tail.
	out := make([]llm.Message, 0, len(msgs)+2+len(reinjected)+len(injected))
	out = append(out, msgs[:tailStart]...)
	out = append(out, boundary, summaryMsg)
	out = append(out, reinjected...)
	out = append(out, injected...)
	out = append(out, msgs[tailStart:]...)

	// The point of a summary is a smaller view. One that is not (a summary as
	// long as what it replaced, a head that was mostly skills re-injected
	// verbatim) is thrown away: accepting it would add a boundary, lose the
	// original wording and leave the context just as full, so the next turn
	// would summarize again. It counts against the summarizer like a failure.
	if c.estimate(llm.MessagesForAPI(out)) >= viewBefore {
		c.lastReason = ReasonSummaryNotSmaller
		c.rejected = true
		c.summaryFailed()
		return msgs, false
	}
	c.summarySucceeded()
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
