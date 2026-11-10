# Skills

> Package `skill` · `skill/skill.go`

A **skill** is a named, reusable instruction package the agent can invoke on demand:
a procedure for a recurring kind of task, kept out of the base prompt until it is
needed. The format is a `SKILL.md` file — YAML frontmatter followed by a Markdown
body.

## Format

```markdown
---
name: review-pr
description: Review a pull request for correctness and style. Use when asked to review code.
license: Apache-2.0        # optional
compatibility: ...         # optional
mcps: [github]             # optional: MCP servers this skill unlocks when invoked
---

1. Read the diff.
2. Check for ...
```

- `name` and `description` are required; the description doubles as the "when to use"
  signal the model reads.
- `license` and `compatibility` are optional standard fields.
- `mcps` names MCP servers to reveal and unlock when the skill loads.

## How invocation works

Load skills with `skill.LoadDir(dir)` (each skill is a subdirectory holding a
`SKILL.md`, or a bare `.md` file) or build them in code with `skill.NewRegistry`. A
`skill.Registry` exposes a single `Skill` tool (added via `Options.Skills`). When the
model invokes a skill, its instructions are injected into the conversation as an
**independent message** — not buried in a tool result — so they become standing
guidance for subsequent turns (this uses `tool.Result.Extra`).

## Surviving compaction

Because a skill's instructions are standing guidance, losing them to a summary would
quietly change the agent's behavior. So when a compaction boundary summarizes over a
skill invocation, the latest invocation of each affected skill is re-injected verbatim
just after the summary (see the [compaction guide](compaction.md#skills-survive-boundaries)).
That is why skills are reliable even in very long sessions.

## Unlocking tools

A skill that declares `mcps:` can unlock those servers' deferred tools on invocation.
The registry does not do this by itself: set `Registry.OnInvoke`, which runs when a
skill is invoked, and have it `Add` the servers' tools to the session's `UnlockSet`
(`Session.UnlockSet()`, or the set you passed in `Options.UnlockSet`). Its return
string is appended to the tool output, which is the place to list the newly available
tool names. The model can then call them only once the relevant skill is active,
which keeps a large tool surface gated behind the skill that justifies it.
