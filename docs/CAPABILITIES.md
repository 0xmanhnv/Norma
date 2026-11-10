# Capability matrix

What Corvid can do, which package owns it, how you turn it on, and where to read more.
Everything here is verified against the source on `dev`. "Default" means it is
active without any configuration; "opt-in" means you set the listed `Options` field.

## Core (always on)

| Capability | Package | How | Guide |
|---|---|---|---|
| Streaming tool-use loop | `harness` | `Session.Prompt` / `agentcore.Run` | [agent loop](guides/agent-loop.md) |
| Dual wire formats (Anthropic, OpenAI, OpenAI-Responses) | `llm` | `llm.NewProvider(Config{Format})` | [architecture](ARCHITECTURE.md#2-package-map) |
| Non-streaming mode | `harness` | `Options.NonStreaming` | [agent loop](guides/agent-loop.md#streaming-vs-non-streaming) |
| Host-owned system prompt | `agentcore` | `Options.SystemPrompt` (SDK injects none) | [architecture](ARCHITECTURE.md#1-the-big-picture) |
| Fail-closed tool system | `tool` | `tool.Build` defaults | [tools](guides/tools.md#fail-closed-defaults) |
| Default file/shell toolset | `tool` | `tool.DefaultTools()` (used when `Options.Tools` unset) | [tools](guides/tools.md#the-built-in-toolset) |
| Four-stage permission pipeline | `permission` | `Options.PermissionMode`, `AllowedTools`, `DisallowedTools`, `CanUseTool` | [permissions](guides/permissions.md) |
| Input coercion + schema validation | `tool` | automatic (opt out with `Spec.RawInput`) | [tools](guides/tools.md#input-handling) |
| Bounded tool output (+ optional spill) | `tool`, `harness` | automatic; `Options.ToolOutputDir` to spill | [tools](guides/tools.md#output-is-always-bounded) |
| Stable error taxonomy | `llm` | `llm.CodeOf`, `llm.IsCode`, `ContentBlock.ToolError` | [architecture](ARCHITECTURE.md#2-package-map) |
| Typed tool output | `tool` | `Spec.Output` → `Result.Data` / `Result.Meta` | [tools](guides/tools.md#typed-output) |
| Budgets + wrap-up (settlement) | `harness` | `Options.MaxTurns`, `MaxDuration`, `Settlement` | [agent loop](guides/agent-loop.md#budgets-and-the-wrap-up-phase) |
| Token-budget continuation | `harness` | `Options.TokenBudget` | [agent loop](guides/agent-loop.md#continuation-gates) |
| Cancellation | `harness` | pass a `context.Context` | [agent loop](guides/agent-loop.md) |

## Context management (choose one)

| Capability | Package | How | Guide |
|---|---|---|---|
| Built-in compaction (MicroCompact / AutoCompact / Reactive + circuit breaker) | `compaction` | `Options.Compaction` | [compaction](guides/compaction.md#built-in-compaction) |
| Model-driven compression | `fold`, `foldadapter` | `foldadapter.Enable(&opts, ...)` | [compaction](guides/compaction.md#model-driven-compression-fold) |
| Custom context manager | `harness` | `Options.Compactor` (optionally `ContextView`) | [compaction](guides/compaction.md#the-harness-contract) |

> A session holds exactly one context manager; setting `Compactor` ignores `Compaction`.

## Opt-in subsystems

| Capability | Package | How | Guide |
|---|---|---|---|
| Lifecycle hooks (8 points) | `hook` | `Options.Hooks` | [architecture](ARCHITECTURE.md#7-mcp-skills-memory-plan-mode) |
| Subagents (context isolation) | `subagent` | add the `Agent` tool from `subagent.Config` | [subagents](guides/subagents.md) |
| Coordinator / multi-agent | `coordinator` | `coordinator.Config` (`AgentTool`+`SystemSegment` or `RunParallel`) | [subagents](guides/subagents.md#coordinator-mode) |
| MCP tools (stdio + in-process) | `mcp` | add adapted tools to `Options.Tools` | [MCP](guides/mcp.md) |
| Plan mode | `plan` | `Options.Plan` | [architecture](ARCHITECTURE.md#7-mcp-skills-memory-plan-mode) |
| Long-term memory | `memory` | `Options.Memory` | [memory](guides/memory.md) |
| Skills | `skill` | `Options.Skills` | [skills](guides/skills.md) |
| Background jobs | `tool` (Manager) | on by default; `Options.DisableBackgroundTasks` to turn off; per-task memory cap on Linux via `AGENT_CORE_TASK_MEM_CAP` (default 80% of the cgroup/host memory budget, `off` disables) | [jobs](guides/jobs.md) |
| Deferred tools | `tool` | `Options.DeferredTools` (+ `UnlockSet`) | [tools](guides/tools.md#deferred-tools) |
| Transcript persistence + resume | `transcript` | `Options.Transcript`; `Session.Resume(id)` | [architecture](ARCHITECTURE.md#10-transcript-and-resume) |
| Web fetch | `tool` | `Options.EnableWebFetch` (+ proxy/CA options) | [tools](guides/tools.md#the-built-in-toolset) |
| Web search (DuckDuckGo / Brave / Tavily / DeepSeek) | `tool` | `Options.EnableWebSearch` (+ `WebSearchBackend`) | [tools](guides/tools.md#the-built-in-toolset) |
| Todo planning | `tool` | `Options.Todos` | [tools](guides/tools.md#the-built-in-toolset) |
| Ask-the-user questions | `tool` | `Options.AskUser` | [tools](guides/tools.md#the-built-in-toolset) |

## Observability

| Capability | Package | How |
|---|---|---|
| Live token usage | `harness` | `KindUsage` events; `Session.Usage()` |
| Tool-result callback (incl. host-only `Result.Data`) | `agentcore` | `Options.OnToolResult` |
| Configuration warnings | `agentcore` | `Options.OnWarn` |
| Typed termination reason | `harness` | `Terminal.Reason` on the final `KindResult` event |
