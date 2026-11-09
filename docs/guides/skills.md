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

A `skill.Registry` exposes a single `Skill` tool (added via `Options.Skills`). When
the model invokes a skill, its instructions are injected into the conversation as an
**independent message** — not buried in a tool result — so they become standing
guidance for subsequent turns (this uses `tool.Result.Extra`).

## Surviving compaction

Because a skill's instructions are standing guidance, losing them to a summary would
quietly change the agent's behavior. So when a compaction boundary summarizes over a
skill invocation, the latest invocation of each affected skill is re-injected verbatim
just after the summary (see the [compaction guide](compaction.md#skills-survive-boundaries)).
That is why skills are reliable even in very long sessions.

## Unlocking tools

A skill that declares `mcps:` can unlock those servers' deferred tools on invocation:
the registry's hook adds them to the session's `UnlockSet` so the model may call them
only once the relevant skill is active. This keeps a large tool surface gated behind
the skill that justifies it.
