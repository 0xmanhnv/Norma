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

### Changed

- `transcript.Store.Load` returns the records it could read where it used to return
  an error for a corrupt line. Callers that relied on that error to detect a damaged
  transcript should call `LoadReport` and inspect what was skipped.
- A run that ends on a `refusal` stop reason has `Terminal.Reason == "refusal"` and
  a non-nil `Terminal.Err`; it used to be `completed` with no error.
- `Session.Resume` may append one message to the transcript (the result that answers
  an interrupted tool call).
