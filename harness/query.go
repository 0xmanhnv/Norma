package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// QueryInput is a fully-resolved request to run the loop.
type QueryInput struct {
	Provider        llm.Provider
	System          []string
	DynamicBoundary int
	// ModelSettings are sent with every model request of the run: the prompt
	// cache lifetime, thinking mode and reasoning effort. They are per session;
	// changing one between two runs of a conversation invalidates its cache.
	ModelSettings llm.ModelSettings
	Messages      []llm.Message // conversation including the latest user turn
	// SystemReminder, when non-empty, is appended as a text block to the last
	// user message of every model request, like WorkingFrame (and before it).
	// It is not stored in conversation history — the host builds it fresh per
	// Prompt call so it never accumulates across turns. It sits at the end, and
	// not in front of the conversation, because a prompt cache matches the
	// request from its first byte: a reminder that changed (a new date) ahead
	// of the first message would make every request pay for the whole history.
	SystemReminder string
	// WorkingFrame, when non-nil, is called before every model call; a non-empty
	// result is appended as a text block to the last user message of that
	// request only. It never enters the conversation history, so no compaction
	// strategy can drop it and it is always current (the host rebuilds it from
	// its own state: goal, scope, open work, what was already tried). Placing it
	// at the end keeps the cached prompt prefix intact.
	WorkingFrame func(ctx context.Context) string
	// Tracer, when non-nil, receives a span for every model call and tool call.
	Tracer Tracer
	Tools  *tool.Registry
	// StopCheck, when non-nil, is called before every model call, after
	// compaction has had its turn. Returning a StopRequest ends the run there,
	// with the request's reason and detail on the Terminal, instead of making
	// the call. It is how a host ends a run it knows cannot go on — the system
	// prompt, the tools and the part of the conversation that must be kept no
	// longer fit the model's window, a spending limit is reached — with a
	// reason of its own rather than a provider error several turns later.
	StopCheck func(ctx context.Context, st RunState) *StopRequest
	// Policy chooses between strict and compatible behaviour, item by item
	// (see permission.Policy). Its zero value is the documented defaults.
	// Tools see it in permission.Context, hooks through
	// permission.PolicyFrom(ctx), and subagents inherit it.
	Policy permission.Policy
	// Budget, when non-nil, is a spending limit the run shares with every
	// agent it starts from a tool call (subagents inherit it through
	// ParentRun). The loop reports what each model call cost (Spend) and asks
	// before every model call whether another may be made (Exhausted); a
	// non-nil answer ends the run with that StopRequest. Because a child
	// charges the same Budget as it goes, fanning work out to subagents cannot
	// overrun it. See NewBudget.
	Budget Budget
	// DeferredTools names tools whose schemas are withheld from the model's tool
	// list (they stay in Tools and are invoked via ExecuteExtraTool). Empty = all
	// tool schemas sent as usual.
	DeferredTools []string
	// MaxTokens is the output cap of each model request. 0 asks for the model's
	// declared limit (ModelSettings.MaxOutputTokens) and, when that is unknown
	// too, sends no cap: the provider adapter then applies its own default. A
	// value above the declared limit is lowered to it.
	MaxTokens   int
	Temperature *float64
	// MaxTurns bounds tool-turns (0 = off). MaxDuration bounds wall-clock time,
	// checked at the turn boundary so a turn is never aborted mid-stream — unlike
	// cancelling ctx, which cuts an in-flight stream/tool (0 = off).
	MaxTurns       int
	MaxDuration    time.Duration
	MaxConcurrency int
	// NonStreaming makes each model call a single non-streaming request (via
	// QueryDeps.CallModelSync / Provider.Complete) instead of a token stream. The
	// loop still emits the same KindText/KindThinking/KindToolUse/KindUsage events
	// to the host — they are surfaced once, when the full response arrives, rather
	// than incrementally. Zero value = streaming (unchanged default).
	NonStreaming bool
	WorkingDir   string
	AgentID      string
	// ToolOutputDir, when set, makes oversized tool output spill to a file there
	// (full content kept) instead of being discarded; MaxToolOutputChars sets the
	// per-tool output budget (head size; 0 = default 30000). See tool.Capture.
	ToolOutputDir      string
	MaxToolOutputChars int
	// SpillStore, when set, replaces the files in ToolOutputDir as the place
	// oversized output is kept (see tool.SpillStore). Nil keeps the default.
	SpillStore tool.SpillStore
	// TaskOutputTailOnly restores the old foreground-command result: only the
	// last MaxToolOutputChars of the task's output, the rest dropped. By
	// default, with ToolOutputDir set, the whole output goes through
	// tool.Capture so it is spilled intact. See tool.ToolContext.
	TaskOutputTailOnly bool

	// Tasks, when non-nil, is the background-task manager shared with the tools.
	// The loop drains its completion notifications into the conversation between
	// tool turns so the model learns when background work finishes.
	Tasks *tool.Manager

	// Todos, when non-nil, enables todo system-reminders: when the model hasn't
	// used TodoWrite for TodoReminderTurnsSinceWrite turns (and it's been at least
	// TodoReminderTurnsBetweenReminder turns since the last reminder), the current
	// list is re-surfaced as a <system-reminder> user message. Throttle turns are
	// derived by scanning the API-visible history, so it survives compaction and
	// resume. 0 thresholds default to 10/10 (mirrors claude-code).
	Todos                            *tool.TodoStore
	TodoReminderTurnsSinceWrite      int
	TodoReminderTurnsBetweenReminder int

	// BashEnv holds extra "KEY=VALUE" entries injected into Bash subprocess
	// environments (foreground + background). Empty still applies the
	// non-interactive defaults (PAGER/GIT_PAGER/SYSTEMD_PAGER/DEBIAN_FRONTEND);
	// see tool.ToolContext.Env.
	BashEnv []string

	// Permission policy.
	PermissionMode     permission.Mode
	PermissionModeFunc func() permission.Mode
	Allowed            []string
	Disallowed         []string
	CanUseTool         permission.CanUseTool

	// Hooks is optional lifecycle instrumentation. Nil disables it.
	Hooks HookRunner
	// OnToolResult, when non-nil, receives every tool's complete result — after
	// the output cap, before the result enters the conversation — including
	// tool.Result.Data, which the model never sees. It runs on the tool's
	// goroutine (tools may run in parallel), so it must be safe for concurrent use.
	OnToolResult func(ctx context.Context, toolUseID, name string, input json.RawMessage, res tool.Result)
	// RewriteToolResult, when non-nil, is the post-execute policy point: it sees
	// every result a tool produced (after a declared output was validated and
	// rendered, before the output cap) and may keep it, replace only what the
	// model reads (tool.ReplaceContent), or replace the value
	// (tool.ReplaceValue), which is revalidated and rendered again. Replacing
	// content is a presentation change, not a confidentiality mechanism: the
	// value still reaches OnToolResult. A value replacement that fails turns the
	// call into an error result rather than falling back to the original. Not
	// called for calls the harness refused (permission, hooks, invalid input).
	// Runs on the tool's goroutine, so it must be safe for concurrent use.
	RewriteToolResult func(ctx context.Context, toolUseID, name string, input json.RawMessage, res tool.Result) tool.Rewrite
	// OnExit, when set, is called exactly once when the loop returns, however it
	// ends. Normally it gets the Terminal also sent as the KindResult event. When
	// the consumer stopped reading first, no result event was sent; OnExit then
	// gets Reason ReasonConsumerStopped and the conversation as far as it got, so
	// a host can keep its state in step with what was recorded.
	OnExit func(*Terminal)
	// Compactor is optional context-window management. Nil disables it.
	Compactor Compactor
	// Recorder, when non-nil, is notified as the conversation grows so a host can
	// persist a transcript. It fires for genuinely new messages (births) in
	// order, and for compaction boundaries; it is never called for compaction's
	// ephemeral working-set edits (MicroCompact tool-result clearing), so the
	// recorded transcript is the raw, full-fidelity history.
	Recorder Recorder

	// TokenBudget, when > 0, enables budget-driven continuation: after a turn
	// with no tool calls, the agent is nudged to keep working until ~90% of the
	// budget (output tokens), stopping early on diminishing returns.
	TokenBudget int
	// EscalateMaxTokens, when true, first retries a max_tokens truncation with a
	// raised output cap before falling back to resume-recovery. The raised cap
	// is the model's declared output limit (ModelSettings.MaxOutputTokens); when
	// that is unknown it is 64000. A turn already running at that cap is not
	// retried this way.
	EscalateMaxTokens bool

	// Settlement, when non-nil with a Prompt, runs a bounded wrap-up phase the
	// moment a budget (MaxTurns/MaxDuration) is hit: it injects Prompt as a user
	// turn and refuses calls to DisabledTools, so a budget-terminated run can
	// write down what it has instead of ending abruptly. The run still finishes with the budget's
	// reason (ReasonMaxTurns/ReasonTimeout), never ReasonCompleted. Nil = off.
	Settlement *Settlement
}

// Settlement configures the wrap-up phase that runs when a budget is hit.
type Settlement struct {
	Prompt   string // wrap-up instruction injected as a user turn (empty = settlement off)
	MaxTurns int    // turn budget for the wrap-up phase itself (0 = default 2)
	// DisabledTools names the tools the model may not use during wrap-up (e.g.
	// ["Bash"]). Their schemas stay in the tool list and their calls are
	// refused: a refused call gets an error result naming the reason, and a
	// call through ExecuteExtraTool is checked by the target tool's name.
	// Removing a schema instead would change the request prefix, and with it
	// every prompt cache over the tools and the conversation, so the wrap-up
	// turns would send the whole history uncached.
	DisabledTools []string
	// HideDisabledTools also removes the schemas of DisabledTools from the
	// wrap-up requests, at the price described above. Their calls are refused
	// all the same. The Compactor is told the new tool list
	// (RequestPrefixAware), so a summary made during wrap-up is built on the
	// prefix the wrap-up requests really have.
	HideDisabledTools bool
	// KeepToolSchemas is ignored: keeping the schemas is the default.
	//
	// Deprecated: leave it unset; set HideDisabledTools for the old default.
	KeepToolSchemas bool
	// PromptByReason optionally overrides Prompt based on WHY the run settled
	// (keyed by the terminal reason, e.g. ReasonTimeout / ReasonMaxTurns). When the
	// settling reason has a non-empty entry here it is used; otherwise Prompt is used.
	// Lets a caller inject a different wrap-up depending on whether a run was cut by
	// its time budget vs its step budget, without knowing which at build time.
	PromptByReason map[TerminalReason]string
}

// promptFor returns the wrap-up prompt to inject for a settling reason: the
// PromptByReason override when present and non-empty, else the base Prompt.
func (s *Settlement) promptFor(reason TerminalReason) string {
	if s.PromptByReason != nil {
		if p := s.PromptByReason[reason]; p != "" {
			return p
		}
	}
	return s.Prompt
}

// RunState is what QueryInput.StopCheck is shown before a model call.
type RunState struct {
	// Turn is the number of the model call about to be made, from 1.
	Turn int
	// Messages is the conversation as it stands (the full history, boundaries
	// included; llm.MessagesForAPI gives what would be sent). Read-only.
	Messages []llm.Message
	// LastInputTokens is the size the provider reported for the previous
	// request plus its output; 0 before the first response.
	LastInputTokens int
	// Usage is what the run has spent so far.
	Usage llm.Usage
	// SubagentUsage is what the agents this run started from tool calls have
	// spent so far (reported when each one finishes).
	SubagentUsage llm.Usage
}

// Budget is a spending limit shared by a run and the agents it starts. Both
// methods are called from several runs at once and must be safe for
// concurrent use.
type Budget interface {
	// Spend records usage the run has spent since its last report: one or
	// more model calls, compaction summaries included.
	Spend(u llm.Usage)
	// Exhausted is asked before every model call; non-nil ends the run.
	Exhausted() *StopRequest
}

// NewBudget returns a Budget that adds up every Spend and asks limit about
// the total. limit turns usage into the host's measure (tokens, or money with
// the host's price table) and returns non-nil once no further model call may
// be made. The usual reason is ReasonBudgetExhausted.
func NewBudget(limit func(total llm.Usage) *StopRequest) Budget {
	return &sharedBudget{limit: limit}
}

type sharedBudget struct {
	mu    sync.Mutex
	total llm.Usage
	limit func(llm.Usage) *StopRequest
}

func (b *sharedBudget) Spend(u llm.Usage) {
	b.mu.Lock()
	b.total.Add(u)
	b.mu.Unlock()
}

func (b *sharedBudget) Exhausted() *StopRequest {
	b.mu.Lock()
	total := b.total
	b.mu.Unlock()
	if b.limit == nil {
		return nil
	}
	return b.limit(total)
}

// StopRequest ends a run from QueryInput.StopCheck.
type StopRequest struct {
	// Reason is the Terminal.Reason the run ends with. Empty means
	// ReasonStopRequested. ReasonContextExhausted is the one to use when the
	// conversation can no longer be made to fit the model's window.
	Reason TerminalReason
	// Detail says why, for people and logs (Terminal.Detail).
	Detail string
	// Err, when set, is the Terminal.Err. Left nil, a run that ends with
	// ReasonContextExhausted gets an error carrying llm.CodeContextOverflow.
	Err error
}

// Compactor is the harness's view of the compaction system (implemented in
// package compaction). Compaction is non-destructive; the harness sends only
// llm.MessagesForAPI(messages) to the model.
type Compactor interface {
	// Pre conditions the history before a model request. lastInputTokens is the
	// total token size the previous model response reported (0 if none yet), used
	// as the initial gate count in preference to local estimation.
	Pre(ctx context.Context, msgs []llm.Message, lastInputTokens int) []llm.Message
	Reactive(ctx context.Context, msgs []llm.Message) ([]llm.Message, bool)
	IsOverflow(err error) bool
}

// ContextView is optionally implemented by a Compactor that builds the request
// message array itself, replacing llm.MessagesForAPI. Hosts with their own
// context manager (e.g. model-driven compression) implement it to project the
// history per request without mutating it.
//
// View MUST be pure: the returned slice is used for this request only and is
// never written back to the loop's history. Implementations that depend on
// stable message identity (content hashing, per-message refs) rely on this —
// writing back would change message bytes and invalidate that identity.
type ContextView interface {
	View(ctx context.Context, msgs []llm.Message) []llm.Message
}

// Recorder receives conversation events for durable transcript persistence
// (implemented in package transcript). RecordMessage fires once per new message
// in birth order; usage carries the token delta attributed to that turn
// (assistant messages only; zero otherwise). RecordBoundary fires when
// compaction inserts a boundary, for observability — boundaries are not part of
// the raw reconstructable history.
type Recorder interface {
	RecordMessage(m llm.Message, usage llm.Usage)
	RecordBoundary(meta llm.BoundaryMeta)
}

// HookRunner is the harness's view of the hook system (implemented in package
// hook). Stop returns two distinct outcomes: preventContinuation (hard
// terminate) and/or blockingErrors (inject + continue).
type HookRunner interface {
	PreToolUse(ctx context.Context, name string, input []byte) (block bool, message string, updated []byte)
	PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool)
	Stop(ctx context.Context, messages []llm.Message) (prevent bool, blockingErrors []string, message string)
}

// QueryDeps are injectable side-effect boundaries for testing (FR-01.6).
type QueryDeps struct {
	CallModel func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error]
	// CallModelSync is the non-streaming counterpart of CallModel, used when
	// QueryInput.NonStreaming is set. It performs a single request and returns the
	// fully assembled assistant message, the stop reason, and cumulative usage.
	CallModelSync func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error)
	NewID         func() string
}

func (d *QueryDeps) withDefaults(in QueryInput) {
	if d.CallModel == nil && in.Provider != nil {
		d.CallModel = in.Provider.Stream
	}
	if d.CallModelSync == nil && in.Provider != nil {
		d.CallModelSync = in.Provider.Complete
	}
	if d.NewID == nil {
		var n int64
		d.NewID = func() string { return "id_" + strconv.FormatInt(atomic.AddInt64(&n, 1), 10) }
	}
}

// Query runs the agent loop and returns its event stream. The final event is
// always KindResult carrying a Terminal (unless the consumer stops early).
func Query(ctx context.Context, in QueryInput, deps QueryDeps) iter.Seq2[Event, error] {
	deps.withDefaults(in)
	deps.traced(in.Tracer)
	conc := in.MaxConcurrency
	if conc <= 0 {
		conc = 10
	}
	return func(yield func(Event, error) bool) {
		l := &loop{ctx: permission.WithPolicy(ctx, in.Policy), in: in, deps: deps, yield: yield, sem: make(chan struct{}, conc)}
		l.messages = append([]llm.Message(nil), in.Messages...)
		l.run()
		if in.Budget != nil {
			l.chargeBudget()
		}
		if in.OnExit != nil {
			t := l.terminal
			if t == nil {
				t = &Terminal{Reason: ReasonConsumerStopped, StopReason: l.lastStopReason, Usage: l.usage,
					SubagentUsage: l.subagentUsage(), Turns: l.turnCount, Messages: l.messages}
			}
			in.OnExit(t)
		}
	}
}

// budgetTracker tracks token-budget continuation state.
type budgetTracker struct {
	continuationCount    int
	lastGlobalTurnTokens int
	lastDeltaTokens      int
}

type loop struct {
	ctx context.Context
	// charged is the part of usage already reported to QueryInput.Budget.
	charged llm.Usage
	// subUsage sums SubagentUsage reported by tool results; tools run on their own
	// goroutines, hence the lock.
	subMu    sync.Mutex
	subUsage llm.Usage
	// terminal is the result finish emitted; nil when the loop stopped because
	// the consumer did.
	terminal *Terminal
	// toolCtx bounds tool execution with the run's MaxDuration deadline, so a tool
	// that overruns the wall-clock budget is interrupted mid-flight (the model call
	// stays on ctx — streams are never cut mid-flight). Derived from ctx, so a parent
	// cancellation (pause/kill/shutdown) still cancels it. Swapped to ctx once the
	// wrap-up phase begins, so settlement tools run live, bounded only by settleMaxTurns.
	toolCtx context.Context
	in      QueryInput
	deps    QueryDeps
	yield   func(Event, error) bool
	sem     chan struct{} // bounds concurrent tool execution

	messages        []llm.Message
	usage           llm.Usage
	lastInputTokens int    // total token size the previous model response reported (compaction gate)
	lastStopReason  string // stop reason of the last model turn that finished (Terminal.StopReason)
	turnCount       int
	boundaryCount   int       // boundary markers already seen, so Recorder fires once each
	startTime       time.Time // loop start, for MaxDuration (wall-clock budget)

	// settlement (wrap-up) phase state, entered when a budget is hit. settling
	// is read by tool goroutines (settleRefusal) while the loop goroutine sets
	// it, and a tool that overran the budget may still be running when it is
	// set, hence the atomic. The other two are the loop goroutine's own.
	settling     atomic.Bool
	settleReason TerminalReason // the budget reason to finish with after wrap-up
	settleTurn   int

	// recovery / continuation state
	maxOutputTokensOverride      int
	maxOutputTokensRecoveryCount int
	hasAttemptedReactiveCompact  bool
	stopHookActive               bool
	stopDetail                   string // StopRequest.Detail of the request that ended the run
	budget                       budgetTracker
	lastContinue                 ContinueReason
}

// Stop reasons the loop acts on, as the adapters normalize them.
const (
	stopMaxTokens = "max_tokens"
	stopRefusal   = "refusal"
)

// truncatedCallText answers a tool call whose arguments the output limit cut
// short. The model reads it, so it says what happened and what to do next.
const truncatedCallText = "Error: truncated: the call was cut off at the output limit before its arguments were complete, so it was not run. " +
	"Retry with a smaller call: send less in one call, or split the work across several calls."

const (
	budgetCompletionThresholdNum = 9 // 0.9
	budgetCompletionThresholdDen = 10
	budgetDiminishingThreshold   = 500
	defaultEscalatedMaxTokens    = 64000 // the escalated cap when the model's limit is unknown
	maxOutputTokensRecoveryLimit = 3
	// maxLoopIterations is a defensive backstop; turnCount counts only tool-turns,
	// so this only guards against a pathological recovery/continuation loop.
	maxLoopIterations = 10000
)

func (l *loop) emit(ev Event) bool { return l.yield(ev, nil) }

// record forwards a newly appended message to the transcript Recorder, if any.
func (l *loop) record(m llm.Message, u llm.Usage) {
	if l.in.Recorder != nil {
		l.in.Recorder.RecordMessage(m, u)
	}
}

// injectTaskNotifications drains completion notifications from the background-task
// manager and appends them to the conversation as a user message, so the model is
// told when background work finishes. Returns true if any were injected.
func (l *loop) injectTaskNotifications() bool {
	if l.in.Tasks == nil {
		return false
	}
	notes := l.in.Tasks.DrainNotifications()
	if len(notes) == 0 {
		return false
	}
	var b strings.Builder
	for i, n := range notes {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(n.String())
	}
	msg := llm.UserText(b.String())
	l.messages = append(l.messages, msg)
	l.record(msg, llm.Usage{})
	return true
}

// recordNewBoundary persists a boundary that compaction just inserted (detected
// by a rise in the boundary-marker count). It writes the post-boundary working
// set — the boundary marker, the summary, and the kept recent tail — as durable
// messages, so a COLD-START Resume restores the compacted view via MessagesForAPI
// instead of replaying raw history and re-summarizing. The tail is re-recorded
// after the boundary (it also exists earlier as births); only the post-boundary
// copy is sent to the model. RecordBoundary keeps the observability-only marker
// (a Type:"boundary" record, skipped during reconstruction).
//
// A new boundary is also when the todo list is put back in front of the model
// (injectTodoReminderAfterCompaction).
func (l *loop) recordNewBoundary() {
	c := countBoundaries(l.messages)
	if c <= l.boundaryCount {
		return
	}
	defer l.injectTodoReminderAfterCompaction()
	if l.in.Recorder == nil {
		l.boundaryCount = c
		return
	}
	if i := llm.LastBoundaryIndex(l.messages); i >= 0 {
		if cr, ok := l.in.Recorder.(CompactionRecorder); ok {
			if meta, ok := llm.ParseBoundaryMeta(l.messages[i]); ok {
				cr.RecordCompaction(meta, l.messages[i:])
				l.boundaryCount = c
				return
			}
		}
		if meta, ok := llm.ParseBoundaryMeta(l.messages[i]); ok {
			l.in.Recorder.RecordBoundary(meta)
		}
		for _, m := range l.messages[i:] { // boundary marker + summary + kept tail
			l.in.Recorder.RecordMessage(m, llm.Usage{})
		}
	}
	l.boundaryCount = c
}

func usageDelta(before, after llm.Usage) llm.Usage {
	return llm.Usage{
		InputTokens:      after.InputTokens - before.InputTokens,
		OutputTokens:     after.OutputTokens - before.OutputTokens,
		CacheReadTokens:  after.CacheReadTokens - before.CacheReadTokens,
		CacheWriteTokens: after.CacheWriteTokens - before.CacheWriteTokens,
	}
}

func countBoundaries(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		if llm.IsBoundaryMarker(m) {
			n++
		}
	}
	return n
}

// finishStopped ends the run on a StopCheck request.
func (l *loop) finishStopped(stop *StopRequest) {
	reason, err := stop.Reason, stop.Err
	if reason == "" {
		reason = ReasonStopRequested
	}
	if err == nil && reason == ReasonContextExhausted {
		msg := "the conversation no longer fits the model's context window"
		if stop.Detail != "" {
			msg += ": " + stop.Detail
		}
		err = llm.NewError(llm.CodeContextOverflow, msg)
	}
	l.stopDetail = stop.Detail
	l.finish(reason, err, "")
}

func (l *loop) finish(reason TerminalReason, err error, text string) {
	l.terminal = &Terminal{
		Reason:        reason,
		Err:           err,
		Detail:        l.stopDetail,
		StopReason:    l.lastStopReason,
		Usage:         l.usage,
		SubagentUsage: l.subagentUsage(),
		Turns:         l.turnCount,
		Messages:      l.messages,
		Text:          text,
	}
	l.yield(Event{Kind: KindResult, Terminal: l.terminal}, nil)
}

// baseMaxTokens is the output cap a request carries when no escalation is in
// force: QueryInput.MaxTokens, else the model's declared limit. It never
// exceeds that limit when the limit is known, and is 0 when neither is: the
// adapter then applies its own default rather than the loop guessing a number
// the model may not accept.
func (l *loop) baseMaxTokens() int {
	limit := max(l.in.ModelSettings.MaxOutputTokens, 0)
	n := l.in.MaxTokens
	if n <= 0 || (limit > 0 && n > limit) {
		n = limit
	}
	return n
}

// maxTokens is the output cap of the next request: the escalated cap while one
// is in force, else baseMaxTokens.
func (l *loop) maxTokens() int {
	if l.maxOutputTokensOverride > 0 {
		return l.maxOutputTokensOverride
	}
	return l.baseMaxTokens()
}

// escalationCap is the raised output cap a truncated turn is retried with
// (EscalateMaxTokens): the model's declared output limit, or
// defaultEscalatedMaxTokens when that is unknown. 0 means there is nothing to
// raise: the run already asks for as much as the model gives.
func (l *loop) escalationCap() int {
	target := defaultEscalatedMaxTokens
	if limit := l.in.ModelSettings.MaxOutputTokens; limit > 0 {
		target = limit
	}
	if l.baseMaxTokens() >= target {
		return 0
	}
	return target
}

func (l *loop) run() {
	var schemas []llm.ToolSchema
	if l.in.Tools != nil {
		schemas = l.in.Tools.Schemas()
		// Deferred tools stay in the registry (callable via ExecuteExtraTool) but
		// their schemas are withheld from the model to save context tokens.
		if len(l.in.DeferredTools) > 0 {
			schemas = filterToolSchemas(schemas, l.in.DeferredTools)
		}
	}
	l.announcePrefix(schemas)
	l.turnCount = 1 // the first model call is turn 1
	l.boundaryCount = countBoundaries(l.messages)
	l.startTime = time.Now() // wall-clock baseline for MaxDuration
	// toolCtx carries the MaxDuration deadline so a tool that overruns the wall-clock
	// budget is interrupted mid-flight (rather than the run only noticing at the next
	// turn boundary, which a hung tool never reaches). On interrupt the loop enters the
	// wrap-up phase on the live parent ctx instead of aborting — see the tools branch.
	l.toolCtx = l.ctx
	if l.in.MaxDuration > 0 {
		var stopTool context.CancelFunc
		l.toolCtx, stopTool = context.WithDeadline(l.ctx, l.startTime.Add(l.in.MaxDuration))
		defer stopTool()
	}
	iterations := 0
	for {
		if l.ctx.Err() != nil {
			l.finish(ReasonAbortedStreaming, l.ctx.Err(), "")
			return
		}
		// Defensive backstop against a pathological recovery/continuation loop.
		// turnCount itself only counts tool-turns (below); every continuation path
		// is individually bounded, so this guard should never fire in practice.
		iterations++
		if iterations > maxLoopIterations {
			l.finish(ReasonMaxTurns, nil, "")
			return
		}

		// Non-destructive proactive compaction (MicroCompact → AutoCompact).
		if l.in.Compactor != nil {
			l.messages = l.in.Compactor.Pre(l.ctx, l.messages, l.lastInputTokens)
			l.absorbCompactionUsage()
			l.recordNewBoundary()
		}
		if l.in.Budget != nil {
			l.chargeBudget()
			if stop := l.in.Budget.Exhausted(); stop != nil {
				if stop.Reason == "" {
					stop.Reason = ReasonBudgetExhausted
				}
				l.finishStopped(stop)
				return
			}
		}
		if l.in.StopCheck != nil {
			st := RunState{Turn: iterations, Messages: l.messages, LastInputTokens: l.lastInputTokens, Usage: l.usage, SubagentUsage: l.subagentUsage()}
			if stop := l.in.StopCheck(l.ctx, st); stop != nil {
				l.finishStopped(stop)
				return
			}
		}

		usageBefore := l.usage
		asst, exec, stopReason, streamErr, consumerStopped, aborted := l.streamAndExecute(schemas)
		if consumerStopped {
			return
		}
		if streamErr != nil {
			// Tools that had fully arrived before the stream broke have run. Keep
			// that in the conversation and the transcript — the assistant turn as
			// far as those calls, and their results — so a retry or a resume sees
			// the side effects instead of repeating them blind.
			if exec != nil && exec.count() > 0 {
				l.messages = append(l.messages, asst)
				l.record(asst, usageDelta(usageBefore, l.usage))
				l.appendToolResults(exec)
			}
			if l.ctx.Err() != nil {
				l.finish(ReasonAbortedStreaming, l.ctx.Err(), "")
				return
			}
			// Reactive recovery from prompt-too-long: one forced AutoCompact, then retry.
			if l.in.Compactor != nil && l.in.Compactor.IsOverflow(streamErr) && !l.hasAttemptedReactiveCompact {
				l.hasAttemptedReactiveCompact = true
				if msgs, ok := l.in.Compactor.Reactive(l.ctx, l.messages); ok {
					l.absorbCompactionUsage()
					l.messages = msgs
					l.recordNewBoundary()
					l.lastContinue = ContinueReactiveCompactRetry
					continue
				}
				l.absorbCompactionUsage()
				l.finish(ReasonPromptTooLong, streamErr, "")
				return
			}
			l.finish(ReasonModelError, streamErr, "")
			return
		}
		l.hasAttemptedReactiveCompact = false
		l.lastStopReason = stopReason
		toolUses := asst.ToolUses()
		// A refusal ends the run as its own terminal state. The provider may send
		// no content at all with it; an assistant message with no content is not
		// appended (it says nothing, and providers reject it on replay).
		refused := stopReason == stopRefusal && len(toolUses) == 0
		turnUsage := usageDelta(usageBefore, l.usage)
		// The size the model just reported for its input, preferred by the
		// compactor over local estimation on the next turn's gate check.
		l.lastInputTokens = turnUsage.InputTokens + turnUsage.OutputTokens
		if !refused || len(asst.Content) > 0 {
			l.messages = append(l.messages, asst)
			l.record(asst, turnUsage)
		}
		// surface cumulative usage after each model turn so hosts can show live token
		// counts during a run (not just at the terminal event).
		if u := l.usage; u != (llm.Usage{}) {
			if !l.emit(Event{Kind: KindUsage, Usage: &u}) {
				return
			}
		}
		if refused {
			l.finish(ReasonRefusal, llm.NewError(llm.CodeRefusal, "the model refused to continue (stop reason: refusal)"), asst.Text())
			return
		}

		// max_tokens truncation with no tool calls → escalate / resume-recovery.
		// These continuations are retries of the same turn (they do not advance
		// turnCount).
		if stopReason == "max_tokens" && len(toolUses) == 0 {
			if l.handleMaxOutputTokens() {
				continue
			}
			// recovery exhausted: surface the truncated turn as completion.
		} else if exec.truncated {
			// The output limit cut a tool call short. The call was answered with a
			// "truncated" error instead of being run; with escalation on, the
			// model retries under the raised cap, which stays until a turn fits.
			if l.in.EscalateMaxTokens {
				l.maxOutputTokensOverride = l.escalationCap()
			}
		} else {
			l.maxOutputTokensOverride = 0
		}

		if len(toolUses) == 0 {
			// During the wrap-up phase, a natural stop ends the run with the ORIGINAL
			// budget reason (not "completed") — the run was budget-terminated; the
			// settlement is just a checkpoint. Skip the keep-working nudges below.
			if l.settling.Load() {
				l.finish(l.settleReason, nil, asst.Text())
				return
			}
			// Stop hooks: hard terminate, or force continue with injected errors.
			// stopHookActive prevents an always-blocking hook from looping forever
			// without an intervening tool-turn.
			if l.in.Hooks != nil && !l.stopHookActive {
				prevent, blocking, msg := l.in.Hooks.Stop(l.ctx, l.messages)
				if prevent {
					l.finish(ReasonStopHookPrevented, nil, msg)
					return
				}
				if len(blocking) > 0 {
					for _, b := range blocking {
						bm := llm.UserText(b)
						l.messages = append(l.messages, bm)
						l.record(bm, llm.Usage{})
					}
					l.stopHookActive = true
					l.lastContinue = ContinueStopHookBlocking
					continue
				}
			}
			l.stopHookActive = false
			// Surface any background-task completions before ending the turn; if any
			// landed, take another turn so the model can react to them.
			if l.injectTaskNotifications() {
				l.lastContinue = ContinueTaskNotification
				continue
			}
			// Token budget: nudge to keep working until ~90% / diminishing returns.
			if l.in.TokenBudget > 0 && l.checkBudgetContinue() {
				l.lastContinue = ContinueTokenBudget
				continue
			}
			l.finish(ReasonCompleted, nil, asst.Text())
			return
		}

		// Tools were executed during streaming; record their results.
		l.appendToolResults(exec)
		// Surface any background-task completions that landed during this turn so
		// the next model round sees them alongside the tool results.
		l.injectTaskNotifications()
		// Parent-ctx cancellation (pause / planner kill / shutdown) → real abort, no wrap-up.
		if l.ctx.Err() != nil {
			l.finish(ReasonAbortedTools, l.ctx.Err(), "")
			return
		}
		// aborted with the parent still alive ⇒ the run's own MaxDuration deadline (on
		// toolCtx) interrupted an in-flight tool. Enter the wrap-up phase on the live
		// parent ctx instead of dying, so a run whose tool overran the wall-clock budget
		// still checkpoints its findings. Mid-tool interrupts are always time-driven
		// (MaxTurns is only ever checked at the turn boundary), hence ReasonTimeout.
		if aborted {
			if l.settling.Load() {
				l.finish(l.settleReason, nil, "")
				return
			}
			if l.beginSettlement(ReasonTimeout, &schemas) {
				l.toolCtx = l.ctx // wrap-up tools run live, bounded only by settleMaxTurns
				l.lastContinue = ContinueNextTurn
				continue
			}
			l.finish(ReasonTimeout, nil, "")
			return
		}

		// next_turn: only tool-turns advance turnCount.
		l.stopHookActive = false // a real turn happened; stop hooks may fire fresh
		if l.settling.Load() {
			// wrap-up phase: bounded by its own small turn budget. When spent, finish
			// with the original budget reason (not "completed").
			l.settleTurn++
			if l.settleTurn >= l.settleMaxTurns() {
				l.finish(l.settleReason, nil, "")
				return
			}
			l.lastContinue = ContinueNextTurn
			continue
		}
		// Primary budgets are checked HERE, at the turn boundary — never mid-stream —
		// so a turn is not aborted in flight (that would surface as an abort/error).
		// On hit, optionally run the wrap-up phase before finishing.
		nextTurn := l.turnCount + 1
		hitTurns := l.in.MaxTurns > 0 && nextTurn > l.in.MaxTurns
		hitTime := l.in.MaxDuration > 0 && time.Since(l.startTime) >= l.in.MaxDuration
		if hitTurns || hitTime {
			reason := ReasonMaxTurns
			if hitTime && !hitTurns {
				reason = ReasonTimeout
			}
			if l.beginSettlement(reason, &schemas) {
				l.lastContinue = ContinueNextTurn
				continue
			}
			l.finish(reason, nil, "")
			return
		}
		l.turnCount = nextTurn
		l.lastContinue = ContinueNextTurn
	}
}

// appendToolResults appends the turn's tool results to the conversation as one
// user message, followed by any extra messages a tool injected after its result
// (e.g. the Skill tool's instructions as an independent user message), so the
// next model round sees them as standing guidance.
func (l *loop) appendToolResults(exec *streamExec) {
	resultMsg := llm.Message{Role: llm.RoleUser, Content: exec.results()}
	l.messages = append(l.messages, resultMsg)
	l.record(resultMsg, llm.Usage{})
	for _, em := range exec.extraMessages() {
		l.messages = append(l.messages, em)
		l.record(em, llm.Usage{})
	}
}

// settleMaxTurns is the wrap-up phase's own turn budget (default 2).
func (l *loop) settleMaxTurns() int {
	if l.in.Settlement != nil && l.in.Settlement.MaxTurns > 0 {
		return l.in.Settlement.MaxTurns
	}
	return 2
}

// beginSettlement enters the wrap-up phase when configured: injects the caller's
// prompt as a user turn and turns DisabledTools off (settleRefusal), so a
// budget-terminated run can write down what it has. The tool list stays as it
// was unless HideDisabledTools asks for the schemas to go; the Compactor is
// then told the list the wrap-up requests carry. Returns false when no
// settlement is configured (caller then finishes immediately).
func (l *loop) beginSettlement(reason TerminalReason, schemas *[]llm.ToolSchema) bool {
	s := l.in.Settlement
	if s == nil {
		return false
	}
	prompt := s.promptFor(reason)
	if prompt == "" {
		return false
	}
	msg := llm.UserText(prompt)
	l.messages = append(l.messages, msg)
	l.record(msg, llm.Usage{})
	l.settling.Store(true)
	l.settleReason = reason
	l.settleTurn = 0
	if len(s.DisabledTools) > 0 && s.HideDisabledTools {
		*schemas = filterToolSchemas(*schemas, s.DisabledTools)
		l.announcePrefix(*schemas)
	}
	return true
}

// settleRefusal returns the error text for a call made during wrap-up to a tool
// the settlement disables, or "" when the call may run. It applies whether or
// not the tool's schema is still listed: a model can call a tool it remembers.
// It runs on tool goroutines.
func (l *loop) settleRefusal(use llm.ContentBlock) string {
	s := l.in.Settlement
	if !l.settling.Load() || s == nil || len(s.DisabledTools) == 0 {
		return ""
	}
	name := use.Name
	if name == tool.ExecuteExtraToolName {
		var args struct {
			ToolName string `json:"tool_name"`
		}
		_ = json.Unmarshal(use.Input, &args)
		name = args.ToolName
	}
	for _, d := range s.DisabledTools {
		if d == name {
			return fmt.Sprintf("Error: %s is not available during wrap-up. Write back what you have and finish.", name)
		}
	}
	return ""
}

// filterToolSchemas returns schemas with the named tools removed.
func filterToolSchemas(schemas []llm.ToolSchema, disabled []string) []llm.ToolSchema {
	block := make(map[string]bool, len(disabled))
	for _, d := range disabled {
		block[d] = true
	}
	out := make([]llm.ToolSchema, 0, len(schemas))
	for _, s := range schemas {
		if !block[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// handleMaxOutputTokens implements the escalate→recover(3×) flow. Returns true
// if it injected a recovery and the loop should continue.
func (l *loop) handleMaxOutputTokens() bool {
	if esc := l.escalationCap(); l.in.EscalateMaxTokens && l.maxOutputTokensOverride == 0 && esc > 0 {
		l.maxOutputTokensOverride = esc
		l.lastContinue = ContinueMaxOutputTokensEscalate
		return true
	}
	if l.maxOutputTokensRecoveryCount < maxOutputTokensRecoveryLimit {
		nudge := llm.UserText(
			"Output token limit hit. Resume directly — no apology, no recap of what you already wrote. Continue exactly where you left off.")
		l.messages = append(l.messages, nudge)
		l.record(nudge, llm.Usage{})
		l.maxOutputTokensRecoveryCount++
		l.maxOutputTokensOverride = 0
		l.lastContinue = ContinueMaxOutputTokensRecovery
		return true
	}
	return false
}

// checkBudgetContinue continues with a nudge while under 90% of budget and not
// in diminishing returns; otherwise stops.
func (l *loop) checkBudgetContinue() bool {
	budget := l.in.TokenBudget
	if budget <= 0 || l.in.AgentID != "" { // subagents have no budget
		return false
	}
	turnTokens := l.usage.OutputTokens
	delta := turnTokens - l.budget.lastGlobalTurnTokens
	diminishing := l.budget.continuationCount >= 3 && delta < budgetDiminishingThreshold && l.budget.lastDeltaTokens < budgetDiminishingThreshold
	if !diminishing && turnTokens < budget*budgetCompletionThresholdNum/budgetCompletionThresholdDen {
		l.budget.continuationCount++
		l.budget.lastDeltaTokens = delta
		l.budget.lastGlobalTurnTokens = turnTokens
		pct := 0
		if budget > 0 {
			pct = turnTokens * 100 / budget
		}
		nudge := llm.UserText(fmt.Sprintf(
			"You have used %d%% of your token budget for this task (%d/%d output tokens). Keep going and finish the task; wrap up before the budget is exhausted.",
			pct, turnTokens, budget))
		l.messages = append(l.messages, nudge)
		l.record(nudge, llm.Usage{})
		return true
	}
	return false
}

// Todo system-reminder throttle defaults (mirror claude-code's TODO_REMINDER_CONFIG).
const (
	defaultTodoTurnsSinceWrite = 10
	defaultTodoTurnsBetween    = 10
)

func (l *loop) todoTurnsSinceWrite() int {
	if l.in.TodoReminderTurnsSinceWrite > 0 {
		return l.in.TodoReminderTurnsSinceWrite
	}
	return defaultTodoTurnsSinceWrite
}

func (l *loop) todoTurnsBetween() int {
	if l.in.TodoReminderTurnsBetweenReminder > 0 {
		return l.in.TodoReminderTurnsBetweenReminder
	}
	return defaultTodoTurnsBetween
}

// maybeInjectTodoReminder appends a todo <system-reminder> user message to the
// history when the model has gone long enough without touching TodoWrite and
// without a recent reminder. Throttle turns are derived from the API-visible
// window (post-boundary) so a reminder re-appears after compaction drops the
// prior one. No-op when todos are disabled or the list is empty.
func (l *loop) maybeInjectTodoReminder() {
	if l.in.Todos == nil {
		return
	}
	todos := l.in.Todos.List()
	if len(todos) == 0 {
		return
	}
	sinceWrite, sinceReminder := todoReminderTurnCounts(llm.MessagesForAPI(l.messages))
	if sinceWrite < l.todoTurnsSinceWrite() || sinceReminder < l.todoTurnsBetween() {
		return
	}
	body := tool.RenderTodoReminder(todos)
	if body == "" {
		return
	}
	msg := llm.TodoReminderMessage(body)
	l.messages = append(l.messages, msg)
	l.record(msg, llm.Usage{})
}

// injectTodoReminderAfterCompaction puts the todo list back in front of the
// model as soon as a compaction has summarized the turns that held it. The
// throttle in maybeInjectTodoReminder counts assistant turns in the view, and
// a fresh view holds only the few turns of the kept tail: left to it, the list
// would stay out of sight for up to TodoReminderTurnsSinceWrite turns, which
// is exactly when the model has just lost the detail of what it was doing. A
// view that still shows a TodoWrite call or a reminder needs nothing.
func (l *loop) injectTodoReminderAfterCompaction() {
	if l.in.Todos == nil {
		return
	}
	body := tool.RenderTodoReminder(l.in.Todos.List())
	if body == "" {
		return
	}
	for _, m := range llm.MessagesForAPI(l.messages) {
		if llm.IsTodoReminder(m) || (m.Role == llm.RoleAssistant && messageHasToolUse(m, tool.TodoWriteName)) {
			return
		}
	}
	msg := llm.TodoReminderMessage(body)
	l.messages = append(l.messages, msg)
	l.record(msg, llm.Usage{})
}

// todoReminderTurnCounts scans messages (most-recent first) and returns the
// number of assistant turns since the last TodoWrite tool_use and since the last
// todo reminder. The turn that contains the TodoWrite is not itself counted (so
// the call turn reads as 0). A never-seen event yields the total assistant-turn
// count (effectively "long ago").
func todoReminderTurnCounts(msgs []llm.Message) (sinceWrite, sinceReminder int) {
	foundWrite, foundReminder := false, false
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if !foundReminder && llm.IsTodoReminder(m) {
			foundReminder = true
			continue
		}
		if m.Role != llm.RoleAssistant {
			continue
		}
		if !foundWrite {
			if messageHasToolUse(m, tool.TodoWriteName) {
				foundWrite = true
			} else {
				sinceWrite++
			}
		}
		if !foundReminder {
			sinceReminder++
		}
	}
	return
}

func messageHasToolUse(m llm.Message, name string) bool {
	for _, b := range m.Content {
		if b.Type == llm.BlockToolUse && b.Name == name {
			return true
		}
	}
	return false
}

// streamAndExecute runs one model completion, surfacing text/thinking, and feeds
// tool_use blocks to the streaming executor as they finish — so tools begin
// running while the rest of the message is still arriving. It returns the
// assembled assistant message, the executor, the stop reason, and flags.
// requestMessages builds the message array for one model request. A Compactor
// that implements ContextView takes over the projection entirely; otherwise the
// default working set (post-boundary, unpaired tool blocks dropped) is used.
// Either way l.messages is left untouched.
func (l *loop) requestMessages() []llm.Message {
	if v, ok := l.in.Compactor.(ContextView); ok {
		return v.View(l.ctx, l.messages)
	}
	return llm.MessagesForAPI(l.messages)
}

func (l *loop) streamAndExecute(schemas []llm.ToolSchema) (asst llm.Message, exec *streamExec, stopReason string, err error, consumerStopped, aborted bool) {
	l.maybeInjectTodoReminder()
	msgs := l.requestMessages()
	// The reminder and the working frame ride at the very end of the request,
	// each as one text block of the last user message, and are counted as the
	// volatile tail: the cache breakpoint goes before them, so everything the
	// next request sends again is read back, and neither ever sits in front of
	// content that is.
	volatile := 0
	if l.in.SystemReminder != "" {
		msgs = withTrailingText(msgs, l.in.SystemReminder)
		volatile++
	}
	if l.in.WorkingFrame != nil {
		if frame := l.in.WorkingFrame(l.ctx); frame != "" {
			msgs = withTrailingText(msgs, frame)
			volatile++
		}
	}
	req := llm.CompletionRequest{
		System:          l.in.System,
		DynamicBoundary: l.in.DynamicBoundary,
		VolatileTail:    volatile,
		Messages:        msgs,
		Tools:           schemas,
		MaxTokens:       l.maxTokens(),
		Temperature:     l.in.Temperature,
	}
	l.in.ModelSettings.Apply(&req)
	if l.in.NonStreaming {
		return l.completeAndExecute(req)
	}
	exec = newStreamExec(l)
	acc := llm.NewAccumulator()

	var building bool
	var curID, curName string
	var curInput strings.Builder
	// finalizeTool dispatches the call being assembled. stop is the turn's stop
	// reason when it is already known (the event closing the call carries it, or
	// the stream has ended), else "".
	finalizeTool := func(stop string, closing llm.Usage) bool { // false = consumer stopped
		if !building {
			return true
		}
		building = false
		raw := strings.TrimSpace(curInput.String())
		curInput.Reset()
		// Arguments that stop mid-JSON on a max_tokens turn were cut off by the
		// output limit. Running the call with "{}" in their place would fail it
		// as "invalid input" and tell the model nothing about why, so it is
		// answered with a truncation error instead and never runs.
		truncated := stop == stopMaxTokens && raw != "" && !jsonValid(raw)
		if raw == "" || !jsonValid(raw) {
			raw = "{}"
		}
		block := llm.ContentBlock{Type: llm.BlockToolUse, ID: curID, Name: curName, Input: []byte(raw)}
		if !l.emit(Event{Kind: KindToolUse, ToolUse: &block}) {
			return false
		}
		if truncated {
			exec.truncated = true
			exec.addRefused(block, toolFailure(block, truncatedCallText, tool.CodeTruncated))
		} else {
			if l.in.Budget != nil {
				// Charge what this response has cost so far before the call
				// starts: a subagent it starts must see the parent's spend.
				u := acc.Usage
				u.MergeEcho(closing)
				total := l.usage
				total.Add(u)
				l.chargeBudgetTo(total)
			}
			exec.add(block)
		}
		return exec.drain()
	}

	for ev, e := range l.deps.CallModel(l.ctx, req) {
		if e != nil {
			err = e
			break
		}
		// Events that belong INSIDE a tool_use block must not reach
		// finalizeTool below — that dispatches the tool and clears `building`,
		// so any argument delta still to come would be dropped on the floor
		// (curInput.WriteString is gated on `building`) and the tool would run
		// with raw == "{}" while acc.Message() records the real arguments.
		// SEToolUseSignature trails the arguments in Gemini's current framing,
		// but a gateway is free to attach extra_content to the chunk that only
		// declares id/name — so route it to the accumulator, not the boundary.
		if ev.Type == llm.SEToolInputJSON || ev.Type == llm.SEToolUseSignature {
			if building && ev.Type == llm.SEToolInputJSON {
				curInput.WriteString(ev.Text)
			}
			acc.Add(ev)
			continue
		}
		// A message_delta with no stop reason (usage only) does not close a
		// call; the one that does carries the stop reason finalizeTool needs.
		if ev.Type == llm.SEMessageDelta && ev.StopReason == "" {
			acc.Add(ev)
			continue
		}
		if !finalizeTool(ev.StopReason, ev.Usage) {
			return asst, exec, "", nil, true, false
		}
		switch ev.Type {
		case llm.SETextDelta:
			if ev.Text != "" && !l.emit(Event{Kind: KindText, Text: ev.Text}) {
				return asst, exec, "", nil, true, false
			}
		case llm.SEThinkingDelta:
			if ev.Text != "" && !l.emit(Event{Kind: KindThinking, Text: ev.Text}) {
				return asst, exec, "", nil, true, false
			}
		case llm.SEToolUseStart:
			curID, curName, building = ev.ToolID, ev.ToolName, true
		}
		acc.Add(ev)
	}
	if err != nil {
		// The stream broke. A call still being received is incomplete and is
		// never dispatched. The calls that had fully arrived are already running:
		// wait for them, so no tool outlives the loop, and hand back the turn as
		// far as the last of them for the caller to record.
		l.usage.Add(acc.Usage)
		if cs, _ := exec.finish(); cs {
			return asst, exec, "", nil, true, false
		}
		return dispatchedPrefix(acc.Message(), exec.count()), exec, acc.StopReason, err, false, false
	}
	if !finalizeTool(acc.StopReason, llm.Usage{}) {
		return asst, exec, "", nil, true, false
	}
	l.usage.Add(acc.Usage)
	asst = acc.Message()
	stopReason = acc.StopReason
	// Drain tools to completion (or synthesize on abort).
	cs, ab := exec.finish()
	if cs {
		return asst, exec, stopReason, nil, true, false
	}
	return asst, exec, stopReason, nil, false, ab
}

// completeAndExecute is the non-streaming counterpart of streamAndExecute's body.
// It performs a single non-streaming model call, then replays the assembled
// message as host events (thinking → text → each tool_use) and runs the same
// tool executor. The returned tuple matches streamAndExecute so the turn loop is
// oblivious to which path produced it. Usage is folded into l.usage exactly as
// the streaming accumulator does, so the loop's per-turn usage delta, KindUsage
// emission, and recorder all see identical numbers.
func (l *loop) completeAndExecute(req llm.CompletionRequest) (asst llm.Message, exec *streamExec, stopReason string, err error, consumerStopped, aborted bool) {
	exec = newStreamExec(l)
	msg, sr, usage, cerr := l.deps.CallModelSync(l.ctx, req)
	if cerr != nil {
		return asst, exec, "", cerr, false, false
	}
	l.usage.Add(usage)
	if l.in.Budget != nil {
		l.chargeBudget() // before any call of this response starts
	}
	asst = msg
	stopReason = sr
	for _, b := range msg.Content {
		switch b.Type {
		case llm.BlockThinking:
			if b.Thinking != "" && !l.emit(Event{Kind: KindThinking, Text: b.Thinking}) {
				return asst, exec, stopReason, nil, true, false
			}
		case llm.BlockText:
			if b.Text != "" && !l.emit(Event{Kind: KindText, Text: b.Text}) {
				return asst, exec, stopReason, nil, true, false
			}
		case llm.BlockToolUse:
			block := b
			if !l.emit(Event{Kind: KindToolUse, ToolUse: &block}) {
				return asst, exec, stopReason, nil, true, false
			}
			// A call the output limit cut short is answered, not run, exactly as
			// on the streaming path. The adapter says so (ContentBlock.Truncated):
			// a whole response hides what a stream shows, since the arguments of
			// a cut call can arrive as a well-formed but incomplete object.
			// Arguments that are not JSON at all on a max_tokens turn are the
			// same case from a provider that does not mark it.
			if b.Truncated || (stopReason == stopMaxTokens && !json.Valid(b.Input)) {
				exec.truncated = true
				exec.addRefused(block, toolFailure(block, truncatedCallText, tool.CodeTruncated))
			} else {
				exec.add(block)
			}
			if !exec.drain() { // surface any results ready so far (FIFO)
				return asst, exec, stopReason, nil, true, false
			}
		}
	}
	// Drain tools to completion (or synthesize on abort).
	cs, ab := exec.finish()
	if cs {
		return asst, exec, stopReason, nil, true, false
	}
	return asst, exec, stopReason, nil, false, ab
}

// dispatchedPrefix cuts an assistant message short after its n-th tool call.
// On a broken stream only what precedes the last dispatched call is known to
// be complete: a later block may be half-received (a tool call with partial
// arguments, a thinking block with no signature yet) and must not be replayed.
func dispatchedPrefix(m llm.Message, n int) llm.Message {
	seen := 0
	for i, b := range m.Content {
		if b.Type != llm.BlockToolUse {
			continue
		}
		if seen++; seen == n {
			return llm.Message{Role: m.Role, Content: m.Content[:i+1]}
		}
	}
	return llm.Message{Role: m.Role}
}

func jsonValid(s string) bool {
	// minimal check used only to guard tool input assembly
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	depth := 0
	inStr := false
	esc := false
	for _, r := range s {
		if inStr {
			if esc {
				esc = false
			} else if r == '\\' {
				esc = true
			} else if r == '"' {
				inStr = false
			}
			continue
		}
		switch r {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}
	return depth == 0 && !inStr
}

// withTrailingText appends frame as a text block to the request's last user
// message, copying so the loop's history is never touched. With no trailing user
// message it adds one. It carries the system reminder and the working frame.
func withTrailingText(msgs []llm.Message, frame string) []llm.Message {
	if frame == "" {
		return msgs
	}
	out := append([]llm.Message{}, msgs...)
	if n := len(out); n > 0 && out[n-1].Role == llm.RoleUser {
		last := out[n-1]
		last.Content = append(append([]llm.ContentBlock{}, last.Content...), llm.TextBlock(frame))
		out[n-1] = last
		return out
	}
	return append(out, llm.UserText(frame))
}

// chargeBudget reports to QueryInput.Budget what the run has spent since its
// last report.
func (l *loop) chargeBudget() { l.chargeBudgetTo(l.usage) }

// chargeBudgetTo reports the spend up to total, a running figure that only
// grows.
func (l *loop) chargeBudgetTo(total llm.Usage) {
	if d := usageDelta(l.charged, total); d != (llm.Usage{}) {
		l.in.Budget.Spend(d)
	}
	l.charged = total
}
