# Agent loop

> Package `harness` · `harness/query.go`, `harness/event.go`, `harness/streaming.go`

The loop is the heart of Corvid. Everything else — tools, permissions, compaction,
subagents — is something the loop calls into. This guide explains what one call to
`Session.Prompt` actually does.

## One turn

`harness.Query` returns an `iter.Seq2[Event, error]`. Internally it builds a `loop`
and runs `loop.run` (`harness/query.go`). One iteration of that loop is a **turn**:

1. **Proactive compaction.** If a `Compactor` is configured, `Compactor.Pre`
   conditions the history before the request (non-destructive).
2. **Stream the model turn.** `streamAndExecute` streams the completion, surfacing
   `KindThinking` / `KindText` / `KindToolUse` events, and begins executing tool
   calls as soon as each `tool_use` block finishes streaming.
3. **Append and account.** The assistant message is appended; cumulative token usage
   is surfaced as `KindUsage`.
4. **Branch on tool calls.**
   - *No tool calls* → run the continuation gates (below). If none fire, finish
     `completed`.
   - *Tool calls* → append the `tool_result` message, drain background-task
     notifications, check budgets at the turn boundary, and loop again.

```mermaid
flowchart LR
    Pre[Compactor.Pre] --> Stream[stream turn] --> Tools{tools?}
    Tools -->|yes| Exec[execute + append results] --> Budget{budget hit?}
    Budget -->|no| Pre
    Budget -->|yes| Settle[wrap-up] --> End1([max_turns / timeout])
    Tools -->|no| Gates[continuation gates] -->|continue| Stream
    Gates -->|done| End2([completed])
```

## Continuation gates

When the model stops asking for tools, the loop does **not** always stop. In order:

- **Stop hooks** (`HookRunner.Stop`) may hard-terminate (`stop_hook_prevented`) or
  force another turn by injecting blocking feedback. A guard (`stopHookActive`)
  prevents an always-blocking hook from looping forever without an intervening turn.
- **Background-task notifications** that landed are drained into the conversation; if
  any did, the loop takes another turn so the model can react.
- **Token budget.** If `TokenBudget > 0`, the agent is nudged to keep working until
  roughly 90% of the output budget or diminishing returns (`checkBudgetContinue`).

Only when none of these fire does the loop finish `completed`.

## Recovery paths

The loop recovers from two provider failure modes without the host doing anything:

- **Prompt too long (overflow).** On the first overflow error the loop asks the
  compactor to force one `Reactive` AutoCompact and retries the turn once
  (`hasAttemptedReactiveCompact`). If recovery is unavailable it finishes
  `prompt_too_long`.
- **`max_tokens` truncation with no tool calls.** The loop can escalate the output
  cap (to 64k if `EscalateMaxTokens` is set) and/or run resume-recovery, bounded by
  `maxOutputTokensRecoveryLimit = 3`, before surfacing the truncated turn.

These are *retries of the same turn* — they do not advance `turnCount`.

## Budgets and the wrap-up phase

`MaxTurns` and `MaxDuration` are checked at the **turn boundary**, never mid-stream,
so a turn is never cut in flight. When a budget is hit, an optional **Settlement**
phase runs a short wrap-up (injecting a prompt, optionally hiding some tools) so a
budget-terminated run can checkpoint its findings. The run still finishes with the
budget's reason, never `completed`. The one case that interrupts a tool mid-flight is
a tool overrunning `MaxDuration`: its `toolCtx` deadline fires, and the loop enters
wrap-up on the live parent context instead of aborting.

## Termination reasons

Every exit carries a typed `TerminalReason` on the final `KindResult` event
(`harness/event.go`): `completed`, `max_turns`, `timeout`, `model_error`,
`prompt_too_long`, `aborted_streaming`, `aborted_tools`, `stop_hook_prevented`,
`hook_stopped`, `blocking_limit`, `image_error`. The `Terminal` struct also carries
the error (if any), the run's own usage, what its subagents used (`SubagentUsage`,
kept separate), the turn count, a full message snapshot, and the assistant's final
text. If the host stops ranging before the end, no `KindResult` is sent;
`QueryInput.OnExit` still receives a `Terminal` with reason `consumer_stopped`.

## Streaming vs non-streaming

By default each turn is a token stream. `QueryInput.NonStreaming` (exposed as
`Options.NonStreaming`) runs each model call as a single request via
`Provider.Complete`; the host receives the same event sequence, just delivered when
the full response arrives.

## Testing seam

`QueryDeps` injects the side-effect boundaries — `CallModel`, `CallModelSync`,
`NewID` — so the entire loop can be driven by a scripted provider with no network.
This is how the harness tests exercise tool execution, message threading, permission
denial, max-turns, and parallel reads end to end.
