# Permissions

> Package `permission` · `permission/permission.go`

The permission package is Corvid's safety boundary. It decides, for every tool call,
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
ModeDontAsk     // never prompt: anything stage 3 does not allow is denied
ModeAuto        // never prompt; same outcome as dontAsk in Evaluate
```

`dontAsk` and `auto` reach the same decisions: both honor allow rules, read-only
tools and a tool's own `Allow` verdict (stage 3), and both deny whatever is left.
They differ only in the denial message and in `harness.StricterMode`, which ranks
`auto` as less strict when a subagent's mode is reconciled with its parent's.

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

Rules live in `Options.AllowedTools` / `Options.DisallowedTools` and are matched in
`permission/rules.go`. A rule is `*` (any tool), a bare tool name, or `Name(spec)`.
The spec is matched against the call's *subject*: the first non-empty string among
the input fields `command`, `file_path`, `notebook_path`, `path` and `url`
(`permission.SubjectOf`), unless the host sets `Request.Subject`.

- `Name()` or `Name(*)` matches every call of the tool.
- `prefix:*` matches the prefix itself or the prefix followed by a space
  (`Bash(git:*)` matches `git status`, not `gitk`).
- A spec with `*` is a glob in which `*` also crosses `/` (`Read(/src/*)` covers the
  whole tree).
- Anything else must match exactly.

Unclear cases fail safe. A spec on a call that has no subject never allows and always
denies. A shell command that chains or substitutes commands (`;`, `&`, `|`, `$(`,
backticks, redirects, newlines) never matches an allow spec, so `Bash(git:*)` does not
allow `git status; rm -rf ~`; a deny spec matches if any command in the chain does.

## Where it plugs in

The harness calls `Evaluate` for every tool call inside `execOne`
(`harness/tools.go`), passing the tool's `IsReadOnly` result and its
`CheckPermissions` verdict. A non-allow decision becomes a synthetic error
`tool_result` (`CodePermissionDenied`) — so a denied call never breaks the
`tool_use`/`tool_result` pairing the provider requires.

Plan mode (see [the architecture overview](../ARCHITECTURE.md#7-mcp-skills-memory-plan-mode-hooks))
changes the active mode mid-loop through `QueryInput.PermissionModeFunc`, which the
harness reads fresh for each call. A subagent runs in the stricter of its own mode and
its parent's, and inherits the parent's rules (see the
[subagents guide](subagents.md)).

## Strict and compatible: permission.Policy

Each check that corvid has tightened is a setting of one `permission.Policy`
(`agentcore.Options.Policy`, `harness.QueryInput.Policy`), with two values: the strict
one and the compatible one, which is how corvid behaved before. Every field's zero value
is its default, chosen so that a host which upgrades corvid without changing its code
keeps working. Subagents inherit the policy.

| Setting | Compatible value | Strict value | Default | What the strict value protects against |
|---|---|---|---|---|
| `DeferredPermissions` | `wrapper` | `own_name` | strict | A deferred tool called through `ExecuteExtraTool` skipping deny rules, its own permission check and the approval callback |
| `DeferredHooks` | `wrapper` | `own_name` | compatible | Hooks keyed on a deferred tool's name never seeing it (they see `ExecuteExtraTool` only) |
| `ShellSessions` | `allow` | `ask` | strict | `shell_open` and `shell_send` running a command or typing text with no approval, past the Bash safety floor and `Bash(...)` deny rules |
| `MemoryWrites` | `allow` | `ask` | strict | A prompt injection persisting through `RecordMemory` into every later session |
| `HookDecisions` | `ignore_unknown` | `block_unknown` | strict | A misspelt or unknown hook decision (`"Block"`, `"deny"`) letting the action through |
| `RewrittenInput` | `trust` | `recheck` | strict | A hook (or a permission decision) rewriting allowed input into something invalid or denied |
| `CallPanics` | `tool_only` | `contain` | strict | A panic in a permission check, hook or result rewriter ending the process |
| `CallsAfterCancel` | `start` | `refuse` | strict | A call queued behind others starting after the run was cancelled or ran out of time |
| `ChildEnv` | `process` | `allowlist` | strict | A command the model runs reading the host's provider keys and other secrets from its environment (see [Child processes](#child-processes)) |

`permission.StrictPolicy()` sets every field to its strict value and
`permission.CompatiblePolicy()` to its compatible one. `agentcore.NewSession` reports
each field that is not at its strict value once, through `Options.OnWarn`, naming the
field and the value to set. An `Embedded` session always runs `StrictPolicy()`.

To run everything strict, set `Options.Policy: permission.StrictPolicy()`. To keep one
check as it was while the rest is strict, start from `StrictPolicy()` and set that
field to its compatible value.
## Child processes

Every subprocess corvid starts (Bash, background tasks, interactive shell sessions,
Grep's ripgrep, stdio MCP servers) is launched through `tool.PrepareChild` and the
process-wide `tool.ChildProcess` settings (`tool.SetChildProcess`).

What a child's environment holds:

- For the tools of a session whose `permission.Policy.ChildEnv` is `allowlist` (the
  default), or with `ChildProcess.Allowlist`: an allowlist, `PATH`, `LANG`, `LANGUAGE`,
  `LC_*`, `TERM`, `TZ`, `TMPDIR`, `HOME`, `USER`, `LOGNAME` and `SHELL`, plus the parent
  variables named in `ChildProcess.KeepEnv`.
- Otherwise (`ChildEnv` `process` with no settings installed, or settings with neither
  `Allowlist` nor `CleanEnv`, and for stdio MCP servers without them): the whole
  environment of the host process, provider API keys and other secrets included, as
  before.
- With `ChildProcess.CleanEnv`, in either case: the allowlist without `HOME`, `USER`, `LOGNAME` and
  `SHELL`; `HOME` is `ChildProcess.Home` or the child's working directory.

Per-call variables are always added on top: `Options.BashEnv`, `ToolContext.Env`, and
the `env` map of `mcp.NewStdioClient`. A secret reaches a child under the allowlist
only when the host names it there or in `KeepEnv`.

`permission.Policy.ChildEnv` is per session; it covers the processes the session's
tools start. MCP servers are started by the host before any session, so they follow
the process-wide settings only. `ChildProcess.Deny` refuses every spawn, and
`ChildProcess.Credential` runs children as another uid/gid.
