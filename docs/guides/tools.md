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
    Prompt() string                               // extended guidance, merged into the schema description
    InputSchema() map[string]any                  // JSON Schema
    IsReadOnly(input json.RawMessage) bool        // read-only calls are allowed without asking
    IsConcurrencySafe(input json.RawMessage) bool // may run in parallel?
    CheckPermissions(ctx context.Context, input json.RawMessage, pc permission.Context) permission.Decision
    Call(ctx context.Context, input json.RawMessage, tc *ToolContext) (Result, error)
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
    Schema: map[string]any{
        "type":       "object",
        "properties": map[string]any{"a": map[string]any{"type": "number"}, "b": map[string]any{"type": "number"}},
        "required":   []any{"a", "b"},
    },
    ReadOnly:   func(json.RawMessage) bool { return true },
    Concurrent: func(json.RawMessage) bool { return true },
    Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
        return permission.Allowed()
    },
    Run: func(_ context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
        var a struct{ A, B float64 }
        if err := json.Unmarshal(in, &a); err != nil {
            return tool.Result{}, err
        }
        return tool.Text(fmt.Sprintf("%g", a.A+a.B)), nil
    },
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

**Read, Write, Edit, MultiEdit, LS, Glob, Grep, Bash** — plus **sleep**, a
context-cancellable wait that pairs with background execution.

A `Session` adds the background-task tools on its own unless background tasks are
disabled. Everything else is opt-in:

| Tool(s) | Enabled by | Notes |
|---|---|---|
| WebFetch | `Options.EnableWebFetch` | Network; permission-gated; proxy / CA options. |
| web_search | `Options.EnableWebSearch` | Pluggable backend (`WebSearchBackend`: DuckDuckGo by default, Brave, Tavily, DeepSeek). |
| TodoWrite | `Options.Todos` | Stateful plan the host can observe via `Todos.List()`. |
| AskUserQuestion | `Options.AskUser` | Pause a long task to ask a structured multiple-choice question. |
| TaskOutput / TaskStop / TaskList / Monitor | on by default; `Options.DisableBackgroundTasks` removes them | See the [jobs guide](jobs.md). |
| shell_open / shell_send / shell_read / shell_list / shell_close | add `tool.ShellSessionTools()` to `Options.Tools` | Interactive PTY sessions for programs that expect a terminal, such as a REPL. They need background tasks on; `AGENT_CORE_DISABLE_INTERACTIVE_SHELL` turns them off. |

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
limit itself. The head the model sees is `Options.MaxToolOutputChars` long (default
30000). With `Options.ToolOutputDir` set, oversized output spills to a file (full
content preserved) and the model receives a head plus a pointer instead of the
overflow being discarded. `Options.SpillStore` swaps the local files for any store
that implements `tool.SpillStore`; if the store fails, the output is truncated to a
head and tail rather than failing the call.

## Typed output

A tool may declare a **typed output** via `Spec.Output` (`tool/output.go`), built
with `tool.DeclareOutput(tool.OutputSpec[T]{Schema, Render, Meta})`. `Run` returns the
value in `Result.Data` with `Content` empty, and `Call` validates it against the
declared schema, freezes a copy, and renders the content the model reads with the
pure `Render` function. The structured value stays in the call — visible to
`Options.OnToolResult` (`QueryInput.OnToolResult`) and to a direct caller — and is
never serialized into the conversation or the transcript. The optional `Meta`
projector fills `Result.Meta` with presentation metadata for the host. `Read` and
`Bash` declare their outputs (`tool.FileLines`, `tool.CommandOutput`); tools that build
`Content` themselves keep working unchanged.

`Options.RewriteToolResult` is the post-execute policy point: it sees every result and
may return `tool.ReplaceContent` (change only what the model reads; the value still
reaches `OnToolResult`, so this is presentation, not confidentiality) or
`tool.ReplaceValue` (change the value, which is validated and rendered again).

A failed call also carries a machine-readable `Result.ErrorCode` (an `llm.Code`) for
hosts, hooks, and the transcript (`ContentBlock.ToolError`); the model never sees it.

## Deferred tools

To keep a large tool catalog from eating context, name tools in
`Options.DeferredTools`: their schemas are withheld from the model's tool list, but
they stay callable. The model discovers them on demand via `SearchExtraTools` and
invokes them through `ExecuteExtraTool` (both auto-added when `DeferredTools` is
non-empty). An `UnlockSet` (`Options.UnlockSet`, or one the session seeds with every
deferred tool) gates which deferred tools are currently callable. Hosts that reveal
tools per skill pass their own set and `Add` to it from `skill.Registry.OnInvoke`.
Only the names reach the model, through `tool.RenderDeferredToolsBlock` placed in the
system prompt.
