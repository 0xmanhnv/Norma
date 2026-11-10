# Tools

> Package `tool` · `tool/tool.go`, `tool/registry.go`, `tool/output.go`

A tool is a capability the agent can invoke. Corvid defines a small contract, a
fail-closed factory, and a set of built-ins — and makes adding your own a matter of
filling in a `Spec`.

## The contract

`CoreTool` (`tool/tool.go`) is the interface every tool satisfies:

```go
type CoreTool interface {
    Name() string
    Description() string
    Prompt() string                                   // extended guidance, merged into the schema description
    InputSchema() map[string]any                      // JSON Schema
    IsReadOnly(input json.RawMessage) bool            // scheduler + permission hint
    IsConcurrencySafe(input json.RawMessage) bool     // may run in parallel?
    CheckPermissions(ctx, input, pc) permission.Decision
    Call(ctx, input, tc) (Result, error)
}
```

## Fail-closed defaults

Most tools are built with `tool.Build(Spec{...})`. `Build` fills **unset** behavioral
fields with the safe default:

- not read-only,
- not concurrency-safe,
- **approval required** (`CheckPermissions` defaults to `AskUser`).

A tool becomes parallelizable or auto-allowed only by *opting in* (setting
`ReadOnly` / `Concurrent` / `Permissions`). This is the core safety property: a new
tool cannot accidentally be more permissive than you intended.

```go
addTool := tool.Build(tool.Spec{
    Name:        "add",
    Description: "Adds two numbers and returns their sum.",
    Schema:      map[string]any{ /* JSON Schema */ },
    ReadOnly:    func(json.RawMessage) bool { return true },
    Concurrent:  func(json.RawMessage) bool { return true },
    Permissions: func(ctx, in, pc) permission.Decision { return permission.Allowed() },
    Run:         func(ctx, in, tc) (tool.Result, error) { /* ... */ },
})
```
(From `examples/custom-llm`.)

## The registry

`tool.Registry` (`tool/registry.go`) is an ordered, name-indexed collection.
`Registry.Schemas()` renders it into provider-facing definitions, merging each tool's
extended `Prompt()` into its description. Registering a tool with an existing name
replaces it.

## The built-in toolset

`tool.DefaultTools()` returns the core set every agent gets unless you override
`Options.Tools`:

**Read, Write, Edit, MultiEdit, LS, Glob, Grep, Bash** — plus **Sleep**, a
context-cancellable wait that pairs with background execution.

Everything else is opt-in:

| Tool(s) | Enabled by | Notes |
|---|---|---|
| WebFetch | `Options.EnableWebFetch` | Network; permission-gated; proxy / CA options. |
| WebSearch | `Options.EnableWebSearch` | Pluggable backend (DuckDuckGo default, Brave, Tavily, DeepSeek). |
| TodoWrite | `Options.Todos` | Stateful plan the host can observe via `Todos.List()`. |
| AskUserQuestion | `Options.AskUser` | Pause a long task to ask a structured multiple-choice question. |
| TaskOutput / TaskStop / TaskList / Monitor | background tasks enabled | See the [jobs guide](jobs.md). |

With background tasks enabled, **Bash** runs every command through the task manager,
so the per-task output cap (50 MiB) and, on Linux, the per-task memory cap
(`AGENT_CORE_TASK_MEM_CAP`) apply to foreground commands too. A command killed by
either reports `killed_by` and `cap` in its result instead of an exit code; see
[jobs → Safety](jobs.md#safety).

## Input handling

Before a tool runs, `harness/tools.go: execOne` **coerces then validates** the model's
arguments against the schema (`tool.CoerceInput`, `tool.ValidateInput`). Coercion
repairs common mistypings (a number or a JSON array sent as a string) so a nearly
right call is not rejected. A tool that would rather repair malformed input itself can
opt out of validation by reporting `RawInput` — the schema is still advertised to the
model.

## Output is always bounded

Every tool's result passes through one output cap at a single post-execute choke
point (`capOutput` in the harness), regardless of whether the tool remembered to
limit itself. With `Options.ToolOutputDir` set, oversized output spills to a file
(full content preserved) and the model receives a head plus a pointer instead of the
overflow being discarded.

## Typed output

A tool may declare a **typed output** via `Spec.Output` (`tool/output.go`): `Run`
returns the value in `Result.Data` with `Content` empty, and `Call` validates it
against the declared schema, freezes a copy, and renders the content the model reads.
The structured value stays in the call — visible to `QueryInput.OnToolResult` and to a
direct caller — and is never serialized into the conversation. A failed call also
carries a machine-readable `Result.ErrorCode` (an `llm.Code`) for hosts, hooks, and
the transcript; the model never sees it.

## Deferred tools

To keep a large tool catalog from eating context, name tools in
`Options.DeferredTools`: their schemas are withheld from the model's tool list, but
they stay callable. The model discovers them on demand via `SearchExtraTools` and
invokes them through `ExecuteExtraTool` (both auto-added when `DeferredTools` is
non-empty). An `UnlockSet` gates which deferred tools are currently callable — the
mechanism skills use to reveal their declared tools only once the skill loads.
