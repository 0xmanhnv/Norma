# Corvid Architecture

> How the pieces fit together, verified against the source on the `dev` branch.
> Every diagram below reflects real control flow; package and file paths are cited so
> you can read the code next to the prose. When a doc and the code disagree, the code wins —
> please open an issue.

Corvid is an **agent harness**: the machinery that turns a language model into an
autonomous agent. A host supplies a model, a system prompt, a set of tools, and a
safety policy; Corvid runs the loop that lets the model think, call tools, observe
results, and continue until it is done — while the host keeps control of every
boundary that matters.

- [1. The big picture](#1-the-big-picture)
- [2. Package map](#2-package-map)
- [3. The agent loop](#3-the-agent-loop)
- [4. Tools and the permission pipeline](#4-tools-and-the-permission-pipeline)
- [5. Context and compaction](#5-context-and-compaction)
- [6. Subagents and coordination](#6-subagents-and-coordination)
- [7. MCP, skills, memory, plan mode](#7-mcp-skills-memory-plan-mode)
- [8. Background jobs](#8-background-jobs)
- [9. Streaming and the event model](#9-streaming-and-the-event-model)
- [10. Transcript and resume](#10-transcript-and-resume)

---

## 1. The big picture

A single stateful `Session` (`agentcore/agentcore.go`) is the public entry point.
It composes a provider, a tool registry, a permission policy, hooks, and an optional
context manager, then streams the loop implemented in `harness`.

```mermaid
flowchart TB
    Host["Host application<br/>(CLI, service, your code)"]

    subgraph Core["agentcore.Session"]
        direction TB
        Opts["Options<br/>(prompt, tools, policy, hooks)"]
        Prompt["Session.Prompt → iter.Seq2[Event, error]"]
    end

    subgraph Loop["harness.Query — the loop"]
        direction TB
        Stream["stream model turn"]
        Sched["schedule + run tools"]
        Gate["Stop / budget gates"]
    end

    Provider["llm.Provider<br/>Anthropic · OpenAI · OpenAI-Responses"]
    Tools["tool.Registry<br/>file / shell / network / deferred"]
    Perm["permission.Evaluate<br/>four-stage pipeline"]
    Ctx["Compactor<br/>compaction · fold"]
    Hooks["hook.Registry<br/>8 lifecycle points"]

    Host --> Core
    Core --> Loop
    Loop --> Provider
    Loop --> Sched
    Sched --> Tools
    Sched --> Perm
    Loop --> Ctx
    Loop --> Hooks
```

Three principles are wired into the structure and worth stating up front:

- **The host owns the prompt.** The SDK injects no preset system prompt; it only
  ever adds a small `<system-reminder>` carrying the current date plus anything the
  host opts into (`agentcore.buildSystemReminder`). Scenario prompts live in
  `examples/`, never in the core.
- **Tools fail closed.** A tool built through `tool.Build` defaults to *not*
  read-only, *not* concurrency-safe, and *approval-required* (`tool/tool.go`). A
  tool becomes parallelizable or auto-allowed only by opting in.
- **Streaming is an iterator.** `Session.Prompt` and `harness.Query` are
  range-over-func iterators (`iter.Seq2[Event, error]`); cancellation is a plain
  `context.Context`.

---

## 2. Package map

Each top-level directory is one Go package with one job. Dependencies flow
downward — `agentcore` depends on everything, `llm` depends on nothing in the tree.

```mermaid
flowchart TD
    agentcore --> harness
    agentcore --> compaction
    agentcore --> memory
    agentcore --> plan
    agentcore --> skill
    agentcore --> transcript
    agentcore --> tool
    agentcore --> permission
    agentcore --> llm

    harness --> tool
    harness --> permission
    harness --> llm

    tool --> permission
    tool --> llm

    subagent --> harness
    subagent --> transcript
    coordinator --> subagent
    mcp --> tool
    skill --> tool
    plan --> tool
    memory -.tools.-> tool
    hook --> llm

    foldadapter --> fold
    foldadapter --> agentcore
    foldadapter --> tool
    fold --> llm

    permission --> llm
    compaction --> llm
    transcript --> llm
```

| Package | Owns | Depends on |
|---|---|---|
| `llm` | Vendor-neutral message model (`Message`, `ContentBlock`, `StreamEvent`, `Usage`), the `Provider` interface, the Anthropic / OpenAI / OpenAI-Responses adapters, and the stable error taxonomy (`llm.Error`, `llm.Code`). | stdlib + `golang.org/x/net` |
| `tool` | The `CoreTool` contract, the fail-closed `Build` factory, the `Registry`, input validation/coercion, and the built-in tools. | `llm`, `permission` |
| `permission` | The safety boundary: decisions, modes, rule matching, and the four-stage `Evaluate` pipeline. | `llm` |
| `harness` | The loop: streaming a turn, scheduling and executing tools, recovery, Stop/budget gates, cancellation. | `llm`, `tool`, `permission` |
| `compaction` | Built-in context-window management (MicroCompact, AutoCompact, Reactive) with a circuit breaker. | `llm` |
| `hook` | Eight lifecycle callbacks; a `Registry` that satisfies `harness.HookRunner`. | `llm` |
| `subagent` | Delegation to isolated child agents via an `Agent` tool, with a depth guard. | `harness`, `tool`, `transcript` |
| `coordinator` | A top-level agent that fans work out to `worker` subagents (LLM-driven or programmatic). | `subagent`, `harness` |
| `mcp` | Model Context Protocol: a stdio JSON-RPC client and in-process servers, each tool adapted as `mcp__<server>__<tool>`. | `tool` |
| `plan` | Plan mode: read-only exploration, then approval flips the permission mode mid-loop. | `permission`, `tool` |
| `memory` | File-backed long-term memory with a `MEMORY.md` index and recall/record tools. | stdlib |
| `skill` | Named instruction packages loaded from `SKILL.md`; a `Skill` tool injects one on demand. | `tool`, `llm` |
| `transcript` | Append-only JSONL persistence and reconstruction for resume. | `llm` |
| `fold` | An opt-in, model-driven context-compression engine: a pure core that decides *when* and *what* to compress while the model writes the summaries. | `llm` |
| `foldadapter` | Wires `fold` into a `Session` (a `Compactor` + a `Compress` tool + resident prompt). | `fold`, `agentcore`, `tool` |
| `agentcore` | The public `Session` / `Options` / `Run` entry point that aggregates all of the above. | everything above |

`cmd/agentcore` is a runnable CLI demo; `examples/` holds scenario wiring. Neither is
imported by the library.

---

## 3. The agent loop

The loop lives in `harness/query.go` (`loop.run`). One iteration is a **turn**: an
optional compaction pass, one model call, then — if the model asked for tools — a
tool-execution phase and another iteration. The loop ends when the model stops
asking for tools (and no gate forces continuation) or when a budget or error path
terminates it.

```mermaid
flowchart TD
    Start([Prompt / Query]) --> Pre["Compactor.Pre<br/>(MicroCompact → AutoCompact)"]
    Pre --> StreamCall["stream a model turn<br/>streamAndExecute"]
    StreamCall --> Err{stream error?}
    Err -->|overflow, first time| React["Compactor.Reactive<br/>force one AutoCompact, retry"]
    React --> StreamCall
    Err -->|other| ModelErr([finish: model_error])
    Err -->|no| Append["append assistant message<br/>emit KindUsage"]
    Append --> HasTools{tool calls?}

    HasTools -->|no| MaxTok{max_tokens<br/>truncation?}
    MaxTok -->|yes| Escalate["escalate / resume-recovery"]
    Escalate --> StreamCall
    MaxTok -->|no| StopHooks["Stop hooks · task notifications · token budget"]
    StopHooks -->|continue| StreamCall
    StopHooks -->|done| Done([finish: completed])

    HasTools -->|yes| RunTools["execute tools<br/>append tool_result message"]
    RunTools --> Budget{MaxTurns /<br/>MaxDuration hit?}
    Budget -->|yes| Settle["optional wrap-up phase"]
    Settle --> Done2([finish: max_turns / timeout])
    Budget -->|no| NextTurn["turnCount++"]
    NextTurn --> Pre
```

Key invariants, each enforced in `loop.run`:

- **Non-destructive context.** The loop keeps the full history in `l.messages` and
  sends only the API-visible projection to the model. Compaction inserts boundaries
  and summaries; it never deletes history.
- **Every `tool_use` gets a `tool_result`.** A denied, blocked, or aborted call
  still produces a synthetic error result (`harness/tools.go: toolFailure`) so the
  conversation the provider sees is always well-formed.
- **Budgets are checked at the turn boundary, never mid-stream.** `MaxTurns` and
  `MaxDuration` are evaluated after a tool phase completes, so a turn is never cut
  in flight. The one exception is a tool that overruns `MaxDuration`: its
  `toolCtx` deadline interrupts it, and the loop then enters the wrap-up phase on
  the live parent context rather than aborting.
- **Termination is typed.** Every exit carries a `harness.TerminalReason`
  (`completed`, `max_turns`, `timeout`, `model_error`, `prompt_too_long`,
  `aborted_streaming`, `aborted_tools`, `stop_hook_prevented`, `hook_stopped`,
  `blocking_limit`, `image_error`) on the final `KindResult` event (`harness/event.go`).

### Continuation gates

When the model stops asking for tools, the loop does not immediately finish. In
order (`loop.run`): Stop hooks may force continuation with injected feedback;
background-task completions are drained in and provoke another turn; and if a
`TokenBudget` is set, the agent is nudged to keep working until roughly 90% of the
budget or diminishing returns. Only when none fire does the loop finish `completed`.

---

## 4. Tools and the permission pipeline

A tool is anything satisfying `tool.CoreTool` (`tool/tool.go`): a name, a
description, a JSON-Schema input, two behavioral predicates (`IsReadOnly`,
`IsConcurrencySafe`), a permission self-check (`CheckPermissions`), and `Call`.
Most tools are built with `tool.Build(Spec{...})`, which fills unset fields with
fail-closed defaults.

### Executing one call

`harness/tools.go: loop.execOne` is the single choke point every tool call passes
through:

```mermaid
flowchart TD
    Call([tool_use block]) --> Resolve{tool in registry?}
    Resolve -->|no| Unknown[error: unknown tool + hint]
    Resolve -->|yes| Coerce["coerce + validate input<br/>(unless RawInput)"]
    Coerce -->|invalid| BadInput[error: invalid input]
    Coerce -->|ok| Eval["permission.Evaluate(...)"]
    Eval -->|not allow| Denied["synthetic error result<br/>CodePermissionDenied"]
    Eval -->|allow| PreHook["PreToolUse hook"]
    PreHook -->|block| Blocked[error: blocked by hook]
    PreHook -->|ok / rewrite| Run["tool.Call"]
    Run --> Cap["cap / spill oversized output"]
    Cap --> Result([tool_result block])
```

Two details make this robust: input is **coerced** before validation (common
mistypings like a number sent as a string are repaired, `tool.CoerceInput`), and
**every** tool's output — built-in, MCP, custom, or host — passes through one output
cap so no single tool can overflow the context window.

### The four-stage pipeline

`permission.Evaluate` (`permission/permission.go`) combines the tool's own verdict
with the host's rules and the active mode. The order encodes **deny-priority**: an
explicit deny wins over every mode, including bypass.

```mermaid
flowchart TD
    In([Request]) --> S1{Stage 1 — deny}
    S1 -->|disallow rule matches| D1[DENY]
    S1 -->|tool self-denies| D1
    S1 -->|no| S2{Stage 2 — mode}
    S2 -->|bypassPermissions| A2[ALLOW]
    S2 -->|plan + not read-only| D2[DENY]
    S2 -->|plan + read-only| A2
    S2 -->|else| S3{Stage 3 — allow}
    S3 -->|allow rule matches| A3[ALLOW]
    S3 -->|acceptEdits + edit tool| A3
    S3 -->|tool is read-only| A3
    S3 -->|tool self-allows| A3
    S3 -->|no| S4{Stage 4 — ask}
    S4 -->|dontAsk / auto| D4[DENY]
    S4 -->|CanUseTool callback| Ask[ALLOW / DENY / ASK result]
    S4 -->|no handler| D4
```

The six modes (`permission.Mode`): `default` (ask for anything not read-only unless
a rule allows), `acceptEdits` (auto-allow Write/Edit/NotebookEdit), `bypassPermissions`
(allow all except deny rules), `plan` (read-only only), `dontAsk` (never prompt —
rules + read-only only), and `auto` (like `dontAsk` but honors a tool's own allow
verdict).

### The built-in toolset

`tool.DefaultTools()` returns **Read, Write, Edit, MultiEdit, LS, Glob, Grep, Bash**,
plus **Sleep** (a context-cancellable wait that pairs with background execution).
Everything else is opt-in through its own constructor or `Options` flag: network
tools (**WebFetch**, **WebSearch**), stateful planning (**TodoWrite**), structured
questions (**AskUserQuestion**), the background-task tools (**TaskOutput, TaskStop,
TaskList, Monitor**), and **deferred tools** — whose schemas are withheld from the
model to save context and discovered on demand via `SearchExtraTools` /
`ExecuteExtraTool`.

---

## 5. Context and compaction

Corvid keeps a conversation inside the model's window **non-destructively**: history
is retained in full; only the projection sent to the provider shrinks. There are two
independent, mutually exclusive context managers — a session holds exactly one.

### Built-in compaction (`compaction`)

Three mechanisms run from `Compactor.Pre` (and `Reactive`) in `compaction/compaction.go`:

```mermaid
flowchart TD
    Pre([Compactor.Pre]) --> Micro{tokens > 60% window?}
    Micro -->|no| Keep[send as-is]
    Micro -->|yes| MC["MicroCompact:<br/>clear old compactable tool results<br/>(keep most recent 5)"]
    MC --> Auto{tokens ≥ threshold<br/>or predictively over?}
    Auto -->|no| Keep
    Auto -->|yes| AC["AutoCompact:<br/>summarize working head,<br/>insert boundary + summary"]
    AC --> Trip{summarizer failed<br/>3× in a row?}
    Trip -->|yes| Breaker[circuit breaker trips:<br/>stop trying to summarize]

    Overflow([prompt-too-long error]) --> Reactive["Reactive:<br/>force one AutoCompact, retry once"]
```

- **MicroCompact** clears the bodies of stale *compactable* tool results
  (file reads, shell, grep/glob, web, edit/write), keeping the most recent five. It
  is non-destructive: a cleared result can be re-read.
- **AutoCompact** summarizes the working head behind a `compact_boundary` marker at
  the token threshold, or predictively when the next turn would overflow. The full
  history stays in the array; only messages after the last boundary are sent
  (`llm.MessagesForAPI`). Thresholds are derived from the window minus a summary
  reservation and a tiered buffer.
- **Reactive** handles a `prompt-too-long` (413) error by forcing one AutoCompact and
  retrying the turn exactly once (`loop.run`, guarded by `hasAttemptedReactiveCompact`).
- A **circuit breaker** (`maxAutoCompactFails = 3`) stops the compactor from
  retrying a summarizer that keeps failing.

> Note: `Snip` and `Context-Collapse` are **intentionally absent** — see the package
> doc comment in `compaction/compaction.go` for why. (Earlier descriptions that list
> them are stale.)

**Skill re-injection.** When a boundary summarizes over a skill invocation, the
summary is lossy for procedural instructions. AutoCompact re-injects the latest
invocation of each affected skill verbatim just after the summary, newest-first,
under a per-skill and total token budget, so standing guidance survives compaction
(`compaction.reinjectSkills`).

### Model-driven compression (`fold` / `foldadapter`)

`fold` is an opt-in alternative that inverts the usual model: instead of the harness
summarizing, the **model** writes the summaries through a `Compress` tool, and `fold`
decides *when* to compress, *which* range, tracks state, and prunes. The core is a
pure library with no I/O and no model calls — the summarizer is reduced to a string
argument. It plugs in as a `harness.ContextView` that projects the request view per
turn without mutating history.

`foldadapter.Enable(opts, …)` is the single, all-or-nothing entry point: it attaches a
`Compactor`, adds the `Compress` tool, and appends the resident prompt sections
together (they must live or die together). Enabling it disables the built-in
`compaction` — the two are mutually exclusive, enforced in `agentcore.NewSession`.
Originals are preserved in a host-specified archive directory, and compression state
can be inherited across forks and subagents.

See the [compaction guide](guides/compaction.md) for the full walkthrough.

---

## 6. Subagents and coordination

A **subagent** (`subagent/subagent.go`) is the same harness loop run with its own
prompt, tool subset, and permission mode — giving context isolation. It is exposed
to the parent as an `Agent` tool; recursion is bounded by a depth guard carried on
the context (`MaxDepth`, default 3).

**Coordinator mode** (`coordinator/coordinator.go`) is a pattern on top of subagents:
a top-level agent that decomposes work and delegates each unit to isolated `worker`
subagents rather than doing it itself. Two paths:

```mermaid
flowchart TD
    subgraph LLM["LLM-driven"]
        C1[coordinator model] -->|Agent tool| W1[worker]
        C1 -->|Agent tool| W2[worker]
    end
    subgraph Prog["Programmatic"]
        RP["RunParallel(tasks)"] --> P1[worker]
        RP --> P2[worker]
        RP --> P3[worker]
        P1 & P2 & P3 --> Agg[collected results]
    end
```

The LLM-driven path exposes the `Agent` tool plus a system segment so the model
spawns workers itself; the programmatic `RunParallel` fans tasks out concurrently
(bounded by `MaxParallel`, default 4) and returns their results.

---

## 7. MCP, skills, memory, plan mode

- **MCP** (`mcp/mcp.go`) speaks JSON-RPC 2.0 over newline-delimited stdio to external
  servers and also supports in-process servers (plain Go functions). Every MCP tool
  is adapted to a `tool.CoreTool` named `mcp__<server>__<tool>`, so the rest of the
  system treats it like any other tool.
- **Skills** (`skill/skill.go`) are named instruction packages: a `SKILL.md` with
  YAML frontmatter (`name`, `description`, optional `license` / `compatibility` /
  `mcps`) plus a Markdown body. Invoking one injects its procedure as an independent
  message that becomes standing guidance (and is re-injected across compaction
  boundaries). A skill may declare MCP servers to unlock when it loads.
- **Memory** (`memory/memory.go`) stores facts as Markdown-with-frontmatter files
  indexed by `MEMORY.md`. The agent recalls relevant memories and records new ones
  via tools; relevant memories can be auto-injected into each prompt.
- **Plan mode** (`plan/plan.go`) starts the agent read-only (mode `plan`). It explores,
  drafts a plan, and calls `ExitPlanMode` to present it. On approval a `Controller`
  flips the live permission mode to the target (default `acceptEdits`); the harness
  reads the mode through `QueryInput.PermissionModeFunc` so the change takes effect
  mid-loop.

---

## 8. Background jobs

When background tasks are enabled, a per-session `tool.Manager` runs long commands
out of band, and the loop drains their completion notifications into the conversation
between turns (`loop.injectTaskNotifications`) so the model learns when work finishes
— including provoking an extra turn so it can react. The **TaskOutput / TaskStop /
TaskList / Monitor** tools read and control them. On Linux every spawned task is also
bounded by a per-task memory cap that kills the process group of a leaking command.
Background tasks can be disabled globally
(`AGENT_CORE_DISABLE_BACKGROUND_TASKS`) or per-session (`Options.DisableBackgroundTasks`).

---

## 9. Streaming and the event model

`harness.Query` yields `harness.Event` values (`harness/event.go`). A host ranges over
the iterator and reacts to each kind; the final event is always `KindResult`.

```mermaid
sequenceDiagram
    participant Host
    participant Loop as harness loop
    participant Model as llm.Provider
    participant Tool

    Host->>Loop: range Session.Prompt(ctx, input)
    Loop->>Model: stream turn
    Model-->>Loop: text / thinking / tool_use deltas
    Loop-->>Host: KindThinking, KindText
    Loop-->>Host: KindToolUse
    Loop->>Tool: execute (parallel if concurrency-safe)
    Tool-->>Loop: result (+ progress)
    Loop-->>Host: KindProgress, KindToolResult
    Loop-->>Host: KindUsage (cumulative)
    Loop->>Model: next turn (results appended)
    Model-->>Loop: final text, no tools
    Loop-->>Host: KindResult (Terminal)
```

Tools begin executing the moment their `tool_use` block finishes streaming
(`harness/streaming.go`), subject to a **FIFO + exclusivity** rule: concurrency-safe
tools run alongside each other, a non-safe tool requires exclusive access, and
results are surfaced in FIFO order. The default concurrency bound is 10
(`MaxConcurrency`).

A `NonStreaming` option runs each model call as a single request instead of a token
stream; the host receives the same event sequence, just delivered when the full
response arrives.

---

## 10. Transcript and resume

With a `transcript.Store` configured, the loop persists the conversation as an
append-only JSONL log through a `Recorder` (`harness/query.go`). It records genuine
message births in order and compaction boundaries (for observability), but never the
ephemeral working-set edits MicroCompact makes — so the recorded transcript is the
raw, full-fidelity history. `Session.Resume(id)` reloads it, rebuilds the message
history and cumulative usage, and drops any trailing tool call whose result never
landed (a crash mid-tool) so the provider never sees a dangling `tool_use`. Subagents
write their own sidechain files under the same store.

---

## Where to read next

- [docs/README.md](README.md) — the documentation index and reading paths.
- The per-subsystem guides under [docs/guides/](guides/).
- The package doc comments — every package opens with a precise summary, and they are
  the canonical source this document is built from.
