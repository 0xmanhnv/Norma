# Permissions

> Package `permission` · `permission/permission.go`

The permission package is Norma's safety boundary. It decides, for every tool call,
whether to **allow**, **deny**, or **ask** — combining the tool's own verdict, the
host's rules, and the active mode into one decision.

## Decisions

A `Decision` carries a `Behavior` (`Allow` / `Deny` / `Ask`), an optional rewritten
input (`UpdatedInput`, e.g. a stripped flag), a `Message` (shown to the model on
deny, to the user on ask), and optional "always allow" `Suggestions` for the host to
persist. Build them with `Allowed()`, `Denied(msg)`, `AskUser(msg)`.

## The six modes

```go
ModeDefault     // ask for anything not read-only, unless a rule allows it
ModeAcceptEdits // auto-allow file edits (Write / Edit / NotebookEdit)
ModeBypass      // allow everything except explicit deny rules
ModePlan        // read-only tools only
ModeDontAsk     // never prompt — rules + read-only only
ModeAuto        // like dontAsk, but a tool's own "allow" verdict is honored
```

## The four-stage pipeline

`permission.Evaluate(ctx, Request)` runs four stages in order. The order **is** the
policy: it encodes deny-priority, so an explicit deny beats every mode — including
bypass.

```mermaid
flowchart TD
    In([Request]) --> S1{1 · deny}
    S1 -->|disallow rule, or tool self-denies| D[DENY]
    S1 -->|no| S2{2 · mode}
    S2 -->|bypass| A[ALLOW]
    S2 -->|plan: read-only? else deny| A
    S2 -->|else| S3{3 · allow}
    S3 -->|allow rule / acceptEdits+edit / read-only / tool self-allows| A
    S3 -->|no| S4{4 · ask}
    S4 -->|dontAsk / auto| D
    S4 -->|CanUseTool callback| Res[callback's decision]
    S4 -->|no handler| D
```

1. **Deny.** A matching disallow rule, or a tool that hard-denies (e.g. failed
   self-validation), is absolute.
2. **Mode short-circuits.** `bypassPermissions` allows; `plan` allows read-only and
   denies the rest.
3. **Allow.** A matching allow rule, `acceptEdits` for an edit tool, a read-only tool,
   or the tool's own `Allow` verdict.
4. **Ask.** `dontAsk` and `auto` refuse to prompt (deny). Otherwise the host's
   `CanUseTool` callback is consulted; with no callback configured, the call is
   denied.

## Rule matching

Allow / deny rules are matched by `matchRule`: a bare tool name, `*` (any), or a
`Name(...)` form whose `Name` part matches the tool. Rules live in
`Options.AllowedTools` / `Options.DisallowedTools`.

## Where it plugs in

The harness calls `Evaluate` for every tool call inside `execOne`
(`harness/tools.go`), passing the tool's `IsReadOnly` result and its
`CheckPermissions` verdict. A non-allow decision becomes a synthetic error
`tool_result` (`CodePermissionDenied`) — so a denied call never breaks the
`tool_use`/`tool_result` pairing the provider requires.

Plan mode (see the [subagents and plan sections of the architecture](../ARCHITECTURE.md))
changes the active mode mid-loop through `QueryInput.PermissionModeFunc`, which the
pipeline reads fresh on each call.
