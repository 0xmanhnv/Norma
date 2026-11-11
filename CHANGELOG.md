# Changelog

Notable changes to Corvid, newest first. Releases are tagged `v0.4.4-p.N`; the
entries under **Unreleased** are on `dev` and not yet in a tagged release.

## Unreleased

### Fixed

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
  encode it unchanged. The Anthropic adapter does not produce these blocks yet.
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
- The guide [Keeping the prompt cache warm](docs/guides/prompt-caching.md).

### Changed

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
