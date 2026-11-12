# Embedding corvid in a service

This guide is for a host that links corvid as a library inside an unattended
service, such as a platform's job worker. In that setting no person watches the run,
and the model must reach only the tools the host registers. The profile for it is
`agentcore.Options.Embedded`. The [permissions guide](permissions.md) and the
[agent loop guide](agent-loop.md) describe the mechanisms it relies on.

## The embedded profile

```go
sess := agentcore.NewSession(agentcore.Options{
    Embedded:     true,
    Provider:     provider,                    // any llm.Provider
    SystemPrompt: []string{systemPrompt},
    Tools:        []tool.CoreTool{lookupTool, submitTool}, // the only tools the model sees
    AllowedTools: []string{"lookup", "submit"},            // what runs without a person
    MaxDuration:  5 * time.Minute,
})
defer sess.Close()
```

`Embedded: true` changes five defaults:

| Default | Without `Embedded` | With `Embedded` |
|---|---|---|
| `Tools` left nil | `tool.DefaultTools()`: Read, Write, Edit, MultiEdit, LS, Glob, Grep, Bash, sleep | no tools |
| Background tasks | TaskOutput, TaskStop, TaskList and Monitor plus a task manager | none (`DisableBackgroundTasks`) |
| `bypassPermissions` | allows everything that no deny rule names | never in effect: the session, and plan mode's target, run as `dontAsk` and `OnWarn` is told |
| `MaxTurns` 0 | unlimited | `agentcore.EmbeddedMaxTurns` (50) |
| `Policy` | as given; zero value = the defaults | always `permission.StrictPolicy()`; a looser `Policy` is ignored and `OnWarn` is told (see the [permissions guide](permissions.md#strict-and-compatible-permissionpolicy)) |

Everything else stays the host's explicit choice. A built-in tool the host passes in
`Tools`, deferred tools, WebFetch and web_search, memory, skills and MCP servers all
remain available when the host turns them on. In `default` or `dontAsk` mode with no
approval callback, a tool that changes something runs only when an allow rule or its
own permission check allows it (see the [permissions guide](permissions.md)).

## What a host links

`agentcore` imports `llm`, `permission`, `tool`, `compaction`, `harness`, `memory`,
`plan`, `skill` and `transcript`. It does not import `fold` or `foldadapter`. A test
(`agentcore/embedded_test.go`, `TestAgentcoreImportGraph`) keeps it that way, and it
keeps `subagent`, `coordinator` and `mcp` out as well. A host that never imports
`foldadapter` does not link the acp-kernel port. The attribution term of that code
([THIRD-PARTY-LICENSES.md](../../THIRD-PARTY-LICENSES.md)) applies only to a product
that links it.

The `tool` package holds the built-in tools' code even when none is registered, so
it is linked, but the model cannot call a tool that is not in the session's registry.

## Extension points

These are the places where a host adds its own policy. Each one is an interface or a
callback, so the host's code stays outside corvid.

| Need | Where | Notes |
|---|---|---|
| Cancel a run | the `context.Context` passed to `Session.Prompt` | Cancelling stops the stream, interrupts running tools, and (strict `CallsAfterCancel`) keeps queued calls from starting. |
| Turn and time limits | `Options.MaxTurns`, `Options.MaxDuration` | `Settlement` adds a bounded wrap-up phase when one of them is hit. |
| Token or cost cap | `Options.Budget` (`harness.NewBudget`), `Options.StopCheck` | The budget is charged after every model response and asked before every model call, by the run and its subagents alike (see the [agent loop guide](agent-loop.md#a-hard-spending-limit)). `StopCheck` sees a `harness.RunState` with the usage so far. |
| Validate tool arguments | `tool.Spec.Schema` (JSON Schema, checked before every call), the tool's `Permissions`, `Options.CanUseTool`, a `PreToolUse` hook | A hook may block a call or rewrite its input. |
| Mark tool output as untrusted data | `Options.RewriteToolResult` returning `tool.ReplaceContent` | It sees every tool result (built-in, MCP, host) before the output cap, so the host can wrap what the model reads in its own delimiters or drop fields. |
| Structured final output | `Options.FinalOutput` | A JSON Schema the final answer must satisfy, with bounded repair turns; see [A structured final answer](#a-structured-final-answer). A `hook.Stop` hook remains the place for checks a schema cannot express. |
| Audit and transcript | the `harness.Event` stream from `Session.Prompt`, `Options.OnToolResult`, `Options.Tracer`, `harness.QueryInput.Recorder` | The event stream carries every text, tool call, tool result and the final `Terminal`. `OnToolResult` also sees the host-only `tool.Result.Data`. |
| Model providers | the `llm.Provider` interface (`Stream`, `Complete`) | A provider written outside this repository plugs in as `Options.Provider`. |
| Child processes | `tool.SetChildProcess` | Only matters if the host registers a tool that starts processes (see the [permissions guide](permissions.md)). |

## A structured final answer

A host that needs a structured answer, such as a record to store or to pass to
the next step, sets `Options.FinalOutput`:

```go
sess := agentcore.NewSession(agentcore.Options{
    Embedded:     true,
    Provider:     provider,
    SystemPrompt: []string{"Answer with one JSON object: the city, the high in °C and the sky."},
    Tools:        []tool.CoreTool{forecastTool},
    AllowedTools: []string{"forecast"},
    FinalOutput: &agentcore.FinalOutput{
        Schema: json.RawMessage(`{
            "type": "object",
            "required": ["city", "high_c"],
            "properties": {
                "city":   {"type": "string"},
                "high_c": {"type": "number"},
                "sky":    {"enum": ["clear", "cloudy", "rain"]}
            }
        }`),
        MaxRepairs: 1, // 0 also means 1; a negative value means no repair
    },
})
if err := sess.Err(); err != nil { // the schema is not a JSON object
    return err
}
```

When the model ends its turn, the loop checks the text:

1. The text, trimmed, must be one JSON document. One Markdown code fence around it
   (```` ```json ```` … ```` ``` ````) is accepted and removed; any other text around
   the document is not.
2. The document must satisfy the schema. The checked keywords are those of tool
   inputs (`tool.ValidateInput`): `type`, `required`, `properties`, `items` and `enum`.
   Other keywords are not checked, and properties the schema does not name are allowed.
3. A valid answer ends the run with `completed`; the document is in `Terminal.Output`
   and the text as written in `Terminal.Text`.
4. An invalid answer is sent back as the next user turn. It says where the document is
   wrong (`done: must be of type boolean`, `the document: missing required property
   "title"`, a syntax error's byte offset) and never repeats what the model wrote, so
   the answer's content is not copied back into the conversation. The loop takes
   another turn; the model may call tools in it.
5. After `MaxRepairs` repair turns in one `Prompt`, an answer that is still invalid
   ends the run with `harness.ReasonInvalidOutput`. `Terminal.Err` carries
   `tool.CodeInvalidOutput`, and `errors.As` finds a `*harness.OutputError` with the
   last problem and the number of repairs. `agentcore.Run` returns that error.

What does not change:

- With no `FinalOutput`, any final text completes the run, as before.
- The schema is not shown to the model. The host says what to answer in its prompt;
  the repair turn names the problem, not the whole schema.
- Streaming is the same: every attempt reaches the host as `KindText` events, so a
  host that shows the stream shows the invalid attempts too. The checked document is
  only in the final `Terminal`.
- Only a run that is about to end with `completed` is checked, after the Stop hooks,
  background-task notifications and the token budget have had their turn. A run that
  ends any other way (a budget and its wrap-up phase, a refusal, a provider error)
  ends as before.
- Repair turns are model calls: they count against `Budget` and `MaxDuration`. A
  repair answer that calls tools takes tool turns, which count against `MaxTurns`.
- The setting works with and without `Embedded`, and each session decodes its own
  copy of the schema, so sessions built from the same `Options` can run at once.

A check that a schema cannot express (a date in the future, an id that must exist)
can stay in a Stop hook, which runs before the schema check:

```go
hooks := hook.NewRegistry().On(hook.Stop, func(ctx context.Context, ev hook.Event) hook.Result {
    final := lastAssistantText(ev.Messages)
    if err := checkDueDate(final); err != nil { // a rule the schema cannot express
        return hook.Result{Decision: "block", Message: "Fix the final answer: " + err.Error()}
    }
    return hook.Result{}
})
```
