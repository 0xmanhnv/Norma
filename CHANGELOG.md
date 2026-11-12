# Changelog

Notable changes to Corvid, newest first. Releases are tagged `vX.Y.Z` from `v0.5.0`
on (earlier ones were `v0.4.4-p.N`); the
entries under **Unreleased** are on `dev` and not yet in a tagged release.

## Unreleased

### Security

- **Go 1.26.11 and golang.org/x/net v0.62.0.** `govulncheck` found 19 reachable
  vulnerabilities with Go 1.26.3 (crypto/tls, crypto/x509, encoding/asn1, net/http,
  net/textproto, net/url) and golang.org/x/net v0.55.0 (used by WebFetch and
  web_search). The `go` directive is now 1.26.11, and CI runs `govulncheck` on every
  pull request.
- **Every tightened check is a `permission.Policy` setting.** Each has a strict and a
  compatible value (how corvid behaved before); the zero value of each is its
  default, so a host that upgrades without changing its code keeps working.
  `StrictPolicy()` and `CompatiblePolicy()` set every field, and a session reports
  each field not at its strict value once through `OnWarn`. The settings:
  `DeferredPermissions` (a deferred tool is permission-checked under its own name;
  default strict), `DeferredHooks` (hooks also see it under its own name; default
  compatible), `ShellSessions` (`shell_open`/`shell_send` ask, with the Bash floor and
  `Bash(...)` deny rules; default strict), `MemoryWrites` (`RecordMemory` asks; default
  strict), `HookDecisions` (an unknown hook decision blocks; default strict),
  `RewrittenInput` (rewritten input is checked again; default strict), `CallPanics` (a
  panic anywhere in a call fails that call; default strict), `CallsAfterCancel` (a
  queued call does not start after cancel; default strict). Table: the
  [permissions guide](docs/guides/permissions.md#strict-and-compatible-permissionpolicy).
- **`ExecuteExtraTool` runs nothing itself.** It hands the deferred tool to the loop
  (`ToolContext.RunDeferred`), which a tool wrapper passes on, so the check holds for
  hosts that decorate their tools; with no loop behind it, it refuses. A test
  enumerates every tool a session can hold and fails on one that changes something
  and allows itself without a reviewed reason, under the strict and the compatible
  preset.
- **The child environment is a policy setting.** `permission.Policy.ChildEnv`
  `allowlist` makes the processes a session's tools start (Bash, background tasks,
  shell sessions, ripgrep) begin from an allowlist (`PATH`, `LANG`, `LANGUAGE`, `LC_*`,
  `TERM`, `TZ`, `TMPDIR`, `HOME`, `USER`, `LOGNAME`, `SHELL`) plus what the host names
  (`ChildProcess.KeepEnv`, `BashEnv`, `ToolContext.Env`), so a provider key in the host's
  environment does not reach a command the model runs. `tool.ChildProcess.Allowlist` is
  the same choice process-wide, which also covers stdio MCP servers. The default is
  `allowlist`; `process` keeps the behaviour before (with no `ChildProcess` settings
  installed, a child gets the whole environment).

### Behaviour changes

With the default policy, compared with before:

- A deferred tool called through `ExecuteExtraTool` is checked by the permission
  pipeline under its own name: a deny rule on that name, the tool's own permission
  check (a hard deny holds even in `bypassPermissions`), and in `default` mode the
  approval callback. A call `ExecuteExtraTool` makes outside the agent loop is refused.
- `shell_open` and `shell_send` ask for approval (nothing changes in
  `bypassPermissions`), and a `Bash(...)` deny rule or the Bash safety floor now also
  denies them, in every mode.
- `RecordMemory` asks for approval (nothing changes in `bypassPermissions`).
- A `hook.Registry` decision other than `"block"`, `"approve"` or empty blocks.
- Input a `PreToolUse` hook rewrites is validated against the tool's schema and
  evaluated by the permission pipeline again.
- Bash, background tasks, shell sessions and ripgrep started by a session's tools get
  the environment allowlist plus the variables the host names, not the whole
  environment (`ChildEnv` `allowlist`). A host whose commands need a variable from its
  own environment names it in `Options.BashEnv` or `tool.ChildProcess.KeepEnv`, or sets
  `ChildEnv` to `process`. MCP servers are unchanged (process-wide settings only).
- A panic in a permission check, a hook or a result rewriter fails that call instead
  of ending the process; a call queued behind others does not start once the run is
  cancelled or out of time.

### Upgrading

- To keep a behaviour exactly as it was, set its field to the compatible value, or
  start from `permission.CompatiblePolicy()`. Each compatible setting is reported once
  per session through `OnWarn`.
- To run everything strict, set `Options.Policy: permission.StrictPolicy()`. That adds
  hooks seeing deferred tools under their own name: a hook that unwraps
  `ExecuteExtraTool` itself then sees the call twice (once as `ExecuteExtraTool`, once
  under the deferred tool's name).
- A host that wraps tools keeps the deferred path working as long as its wrapper
  passes the `ToolContext` it receives to the wrapped tool's `Call`.

### Changed

- **The fork's own version line.** `go.mod` retracts the inherited tags whose `go.mod`
  declares an earlier module path (`v0.1.0` through `v0.4.4-p.4`, and `v0.4.4`). The
  next release is `v0.5.0`. `NOTICE` records the modifications (Apache-2.0 §4(b)),
  `SECURITY.md` says how to report a vulnerability, and `.github/CODEOWNERS` names the
  maintainer.

### Fixed

- **Data race in the wrap-up phase.** The loop set its "settling" state while a tool
  goroutine that had overrun `MaxDuration` could still be reading it. The state is
  now atomic, and the race detector job in CI blocks a merge.
- **The system reminder no longer breaks the cached prefix.** It was sent as the
  first message of every request, so a reminder that changed (a new date) moved the
  start of the conversation and every request paid for the whole history again. It
  is now appended as a text block to the last user message, before the working
  frame, and both are counted as the volatile tail that the cache marker skips.
- **The memory index stays out of the cached system prompt.** With
  `MemoryOptions.IncludeIndexInPrompt` and no `DynamicBoundary` of the host's, the
  index (which changes on every saved memory) was part of the cached prefix. The
  boundary is now placed right before it.
- **`redacted_thinking` blocks from the Anthropic adapter.** They are kept when
  parsing a whole response, emitted as `SERedactedThinking` when streaming, stripped
  together with thinking blocks when thinking is off, and never carry a cache marker.
- **A non-streaming tool call cut off by the output limit is not run.** On a whole
  response the arguments of a cut call can arrive incomplete yet well-formed. The
  adapters (Anthropic, OpenAI Chat Completions, OpenAI Responses) now mark such a
  call `ContentBlock.Truncated`, and the loop answers it with a `truncated` error, as
  it already did when streaming.
- **A stream that breaks mid-turn no longer runs a half-received tool call.** When
  the model stream failed part-way through a tool call, that call was run with empty
  arguments, the tool calls that had already run were dropped from the conversation,
  and their goroutines could outlive the loop. Now the incomplete call is never run,
  the loop waits for the calls that did start, and the assistant turn (up to the last
  complete call) and its results are kept in the conversation and the transcript. The
  run still ends `model_error`, but a retry or a resume can see what ran.
- **A tool call cut off by the output limit is not run with empty arguments.** A
  streamed turn that hit `max_tokens` in the middle of a tool call's arguments used
  to run the tool with `{}`, which failed as "invalid input" and repeated until
  `MaxTurns`. The call is now answered with a `truncated` error
  (`tool.CodeTruncated`) that tells the model the call was cut off and to retry with
  a smaller one. With `EscalateMaxTokens` set, the retry gets the raised output cap.
- **`Session.Usage()` is the running total for the session.** Each `Prompt` used to
  overwrite it with that prompt's usage, including after `Resume`, which discarded
  the total loaded from the transcript. It now accumulates, as its documentation
  always said. `Session.LastUsage()` is new and returns the most recent prompt alone.
- **Thinking blocks with empty text are kept.** When streaming, a thinking block that
  carried a signature but no text was dropped together with its signature. When not
  streaming it was kept but sent back without its `thinking` key, which is malformed.
  Both paths now keep the block and its signature, and a thinking block is always
  encoded with a `thinking` key.
- **The `refusal` stop reason is reported as a refusal.** It used to finish the run
  as `completed` and append an assistant message with no content. The run now ends
  with the terminal reason `refusal` and an error carrying `llm.CodeRefusal`, and an
  empty assistant message is not appended.
- **A transcript survives a write that was cut short.** A record of 4 KB or more and
  its newline were written separately, so a crash between the two left a record with
  no line ending; the next record was glued to it and the file could no longer be
  loaded. Each append is now a single write, an append to a file that does not end
  in a newline starts on a fresh line, and `Load` skips a line it cannot decode
  instead of failing the whole file.
- **Resuming no longer hides a tool call that may have run.** A conversation that
  ended in a tool call with no recorded result had that call dropped on `Resume`
  (and when the caller stopped reading mid-turn), so the model issued it again. The
  call is now kept and answered with an error result saying it was interrupted and
  may have executed, so the model checks before repeating it. That result is also
  written to the transcript.
- **Tool input validation enforces `enum`.** The validator checked types and
  `required` only; a value outside a declared enum now fails validation, and the
  error lists the allowed values. This also applies to declared output schemas.
- **`tool.DeclareOutput` no longer panics without a `Render` function.** It returns
  an `Output` that reports the mistake through `Output.Err()` and fails every
  `Project` and `Apply` with the same `llm.CodeInvalidConfig` error.

- **Subagents, coordinator workers and zero-option sessions cache their prompt.**
  The Anthropic adapter only sent cache markers when `DynamicBoundary > 0`, which
  none of those set, so each of their turns paid full input price for tools, system
  prompt and history. Caching is now on by default; see
  [Keeping the prompt cache warm](docs/guides/prompt-caching.md).
- **The compaction summary reads the session's prompt cache.** It was a separate
  request with its own system prompt and no tools, so the whole summarized history
  was billed as fresh input on every compaction and every retry, and its cost never
  reached the run's usage. It is now made over the run's own prefix (same tools and
  system prompt, the conversation, then the instruction), and its usage is added to
  the run's.
- **MicroCompact no longer rewrites the history on every turn.** Above its threshold
  it cleared one more old tool result per turn, which changed the request prefix
  every time and defeated the prompt cache. It now clears in batches.
- **`thinking: disabled` is not sent to endpoints that never asked for the field.**
  The compaction summary sent it everywhere; endpoints and models that reject it
  failed the summary, and three failures turned compaction off for the rest of the
  session.
- **The compaction circuit breaker recovers.** Once open it stayed open until the
  session ended. It now lets one summary through after a cool-down, or once the
  conversation has grown, and closes when that works.
- **The tail kept after a summary never starts on a tool result.** It was cut by
  message count, wherever that fell: a tool result at the cut was silently dropped
  from the view while its call was dropped from the summary input, and a kept user
  message could follow the summary directly. The tail is now a token budget cut at
  turn boundaries: it starts on an assistant message and keeps every tool call with
  all of its results.
- **A few very large messages after a boundary are compacted.** With eight or fewer
  messages since the last boundary nothing was summarized, whatever their size.
- **A summary that does not shrink the context is thrown away** by the proactive
  pass as well as the reactive one, and a second summary is not requested on the
  same turn.
- **A compaction is written to the transcript in one piece.** The boundary, the
  summary and the kept tail were appended record by record; a crash part-way left a
  boundary with only some of its tail, and a resume hid the rest of the history
  behind it. They now go to the file in a single write
  (`transcript.Writer.RecordCompaction`).
- **Context overflow is recognised by the providers' error shapes.** Any error
  containing "too long" (an over-long tool name, a request that "took too long") was
  treated as an overflow and answered with a forced compaction.
- **Retries wait long enough to matter.** `Retry-After` is honoured, backoff has
  jitter, and 529 (overloaded) is retried. The old ladder gave up after 3.5 seconds.
- **`foldadapter.Enable` keeps the host's `DynamicBoundary`.** It overwrote it with
  the end of the prompt, moving the host's dynamic segments into the cached prefix.
- **OpenAI-format usage reports cache writes**, and cache reads under the field names
  gateways use, without double counting.
- **The local token estimate counts the system prompt and tool schemas**, and can be
  calibrated for dense content; it used to cover messages only at a fixed ratio.

### Added

- **A hard spending limit shared with subagents.** `QueryInput.Budget` /
  `Options.Budget` (`harness.Budget`, `harness.NewBudget`) is charged after every
  model response, before its tool calls start, and asked before every model call; when
  it says no, the run ends with the new `ReasonBudgetExhausted`. Subagents charge
  their parent's budget (`ParentRun.Budget`), or `subagent.Config.Budget` when they
  have no parent, so fan-out cannot overrun it. `RunState.SubagentUsage` shows a
  `StopCheck` what finished subagents spent.
- Compaction hooks on `compaction.Config`: `AfterCompact` (messages placed right
  after the summary, shown a `compaction.Compacted`), `MicroStub` (the text left in
  place of a cleared tool result; `compaction.DefaultMicroStub` keeps the first line
  and the spill pointer), `SummaryPrompt` (with `compaction.DefaultSummaryPrompt`)
  and `References` (identifiers the summary must contain; missing ones are appended
  under a References heading). `Event.ReferencesAdded`, `SummaryCall.Prompt`.
- `tool.SpillPointer`, which finds the spill pointer in captured tool output.
- The todo list is put back in front of the model right after a compaction.
- `subagent.ChildSummary` carries the sidechain transcript locator (`SessionID`,
  `AgentID`, `TranscriptPath`) and a typed `Output` / `OutputErr` from the new
  `Definition.ParseOutput`; `subagent.RunWithSummary` returns it.
- `coordinator.Config.Transcript` and `ParseOutput`, and `coordinator.Result.Summary`.
- `tool.TodoPersister`, `tool.NewPersistentTodoStore`, `tool.TodoFile` and
  `TodoStore.Err`: a todo list that survives a restart.
- `tool.Todo.Meta`, free-form string metadata on a todo.
- `QueryInput.StopCheck` (`agentcore.Options.StopCheck`), `harness.RunState` and
  `harness.StopRequest`: a host can end a run before a model call with a reason of
  its own, for instance when the system prompt, the tools and the part of the
  conversation that must be kept no longer fit the window and no compaction can
  help. New terminal reasons `stop_requested` and `context_exhausted` (the latter
  with an error carrying `llm.CodeContextOverflow`), and `Terminal.Detail` for the
  explanation.
- `compaction.Compactor.Fit` and `compaction.Fit`: whether the fixed prefix plus the
  most recent turn (what a compaction always keeps) still fit the window. The
  question to ask in a `StopCheck`.
- `llm.ModelSettings.MaxOutputTokens`, the model's declared output limit. It is the
  output cap when `MaxTokens` is 0, the ceiling for an explicit `MaxTokens`, and the
  target of `EscalateMaxTokens`.
- `llm.ContentBlock.Truncated` (never encoded).
- `harness.Settlement.HideDisabledTools`, to remove the disabled tools' schemas
  during wrap-up as before. The compactor is then told the new request prefix.
- With `llm.Config.CacheControl`, the OpenAI format also marks the end of the tool
  list, so sessions with identical tools share that entry whatever their system
  prompt.
- `harness.ReasonRefusal`, `llm.CodeRefusal`, and `Terminal.StopReason` (the
  provider's stop reason for the last model turn).
- `tool.CodeTruncated`, the code on the error result of a tool call the output limit
  cut short.
- `Session.LastUsage()`.
- `transcript.Store.LoadReport`, which returns the lines `Load` skipped
  (`transcript.Skipped`); `Session.Resume` reports them through `Options.OnWarn`.
- `transcript.Store.SetSync`, to fsync the transcript after every append.
- `llm.BlockRedactedThinking`, `ContentBlock.Data` and `llm.SERedactedThinking`: the
  message model and the stream accumulator carry a `redacted_thinking` block and
  encode it unchanged.
- `tool.Output.Err()`.

- `llm.ModelSettings` on `agentcore.Options`, `harness.QueryInput`,
  `subagent.Config` and `coordinator.Config`: prompt-cache lifetimes
  (`CacheTTL`, `ConversationCacheTTL`), `DisableCache`, `Thinking` and
  `ReasoningEffort`, set once per session. Subagents inherit the calling run's.
- `llm.CompletionRequest.DisableCache` and `ConversationCacheTTL`.
- `llm.Config.CacheControl`: `cache_control` markers on the OpenAI format, for
  gateways that serve caching models through Chat Completions. Off by default.
- `llm.Config.RetryMaxDelay` and `llm.Config.StreamIdleTimeout`.
- `compaction.Config`: `KeepRecentTokens`, `MicroMinFree`, `BreakerCooldown`,
  `BreakerRegrowth`, `StaticPrefixTokens`, `TokenCalibration`, `Summarizer` and
  `OnEvent`.
- `compaction.StandaloneSummarizer`, to summarize with a separate (cheaper) model.
- `compaction.Event` and `compaction.Reason`: every compaction pass, and every pass
  that was due and skipped, reported with tokens before and after.
- `compaction.SummaryCall` / `SummaryCallFrom`, `Compactor.SetRequestPrefix` and
  `Compactor.TakeUsage`; `harness.RequestPrefixAware` and
  `harness.CompactionUsageReporter` for custom compactors.
- `transcript.Writer.RecordCompaction` and `harness.CompactionRecorder`.
- The guide [Keeping the prompt cache warm](docs/guides/prompt-caching.md).

### Changed

- MicroCompact leaves the cleared notice followed by the result's first line (and its
  spill pointer, if any) instead of the bare notice. A result the stub would not
  shorten is left as it is.
- **Deprecated: `tool.TodoRef` (`Todo.Ref`).** Its two fields named host-specific
  concepts. It is replaced by `Todo.Meta`, a free-form `map[string]string`; the Go
  type and field are removed. On read, a todo with the old `"ref"` object still
  decodes: each of its string fields becomes a `Meta` entry under the same key, so
  stored lists and transcripts load unchanged.
  `"ref"` is never written, and the TodoWrite schema now offers `meta`.
- **The default output cap.** The loop used to send `max_tokens` 32768 whatever the
  model. With neither `MaxTokens` nor `ModelSettings.MaxOutputTokens` set it now sends
  0 and the adapter applies its default: 8192 on the Anthropic format (which
  requires the field), no field at all on the OpenAI format. Set one of the two to
  keep a larger cap. `EscalateMaxTokens` raises the cap to the declared limit, or to
  64000 when it is unknown, and does nothing when the run already asks for that much.
- **Wrap-up keeps the tool schemas.** `Settlement.DisabledTools` now leaves the tools
  in the list and refuses their calls (what `KeepToolSchemas` used to opt into), so
  the wrap-up turns still read the prompt cache. `KeepToolSchemas` is deprecated and
  ignored; set `HideDisabledTools` for the old behaviour.
- The request prefix given to a `RequestPrefixAware` compactor no longer carries the
  system reminder in `Messages` (it is empty).
- On the OpenAI format, several text blocks of one user message are joined with a
  blank line; they used to be concatenated with nothing between them.
- The race detector job in CI is blocking.
- `transcript.Store.Load` returns the records it could read where it used to return
  an error for a corrupt line. Callers that relied on that error to detect a damaged
  transcript should call `LoadReport` and inspect what was skipped.
- A run that ends on a `refusal` stop reason has `Terminal.Reason == "refusal"` and
  a non-nil `Terminal.Err`; it used to be `completed` with no error.
- `Session.Resume` may append one message to the transcript (the result that answers
  an interrupted tool call).
- Requests on the Anthropic format carry `cache_control` markers even when
  `DynamicBoundary` is 0, and the whole system prompt is then the cached prefix. Set
  `ModelSettings.DisableCache` for an Anthropic-compatible endpoint that rejects
  them.
- `llm.Config.MaxRetries` defaults to 5 (was 3) and the waits are jittered.
- The compaction summary request lists the session's tools and system prompt. A
  summarizer reply with no text is an error, not an empty summary.
- `compaction.Config.KeepRecent` is a maximum: the kept tail starts at a turn
  boundary and may hold fewer messages.
- A summary that is not smaller than what it replaces is no longer inserted.
- `Terminal.Usage` includes the tokens spent on compaction summaries.
