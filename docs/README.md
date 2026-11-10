# Norma documentation

Norma is a general-purpose **agent harness SDK in Go**: the engine that turns a
language model into an autonomous agent — a streaming tool-use loop where the model
reads, runs, and reasons its way through a task while the host keeps control of the
prompt, the tools, and the safety boundary.

This is the documentation index. Start with a reading path, then dive into the
architecture overview or a per-subsystem guide.

## Start here

**If you want to *use* Norma to build an agent:**

1. [Top-level README](../README.md) — what Norma is, the quickstart, and the CLI demo.
2. [Agent loop guide](guides/agent-loop.md) — what one `Prompt` actually does, and the
   events you range over.
3. [Tools guide](guides/tools.md) — the built-in toolset and how to add your own.
4. [Permissions guide](guides/permissions.md) — the safety boundary you configure.
5. [Compaction guide](guides/compaction.md) — keeping a long conversation inside the
   context window.
6. The runnable [`examples/`](../examples/) — four scenarios wired end to end.

**If you want to *understand or contribute to* Norma:**

1. [ARCHITECTURE.md](ARCHITECTURE.md) — the whole system with diagrams and a package map.
2. [CONTRIBUTING.md](../CONTRIBUTING.md) — build, test, layout, and conventions.
3. The per-subsystem guides below, each next to its package.
4. The package doc comments — every package opens with a precise summary; they are the
   canonical source these docs are built from.

## Architecture

- [ARCHITECTURE.md](ARCHITECTURE.md) — big picture, package map, the agent loop, the
  permission pipeline, compaction, subagents, the event model, and transcript/resume,
  with verified Mermaid diagrams.
- [CAPABILITIES.md](CAPABILITIES.md) — the capability matrix: what Norma can do, which
  package owns it, and how to turn it on.

## Subsystem guides

One page per major subsystem. Deep but accurate; each cites the code.

| Guide | Package | What it covers |
|---|---|---|
| [Agent loop](guides/agent-loop.md) | `harness` | The turn lifecycle, recovery paths, continuation gates, termination reasons. |
| [Tools](guides/tools.md) | `tool` | The `CoreTool` contract, fail-closed defaults, the built-in set, typed output, deferred tools. |
| [Permissions](guides/permissions.md) | `permission` | Decisions, the six modes, deny-priority, the four-stage pipeline. |
| [Compaction](guides/compaction.md) | `compaction`, `fold` | Non-destructive context management; built-in vs model-driven. |
| [Subagents](guides/subagents.md) | `subagent`, `coordinator` | Context-isolated delegation and multi-agent coordination. |
| [MCP](guides/mcp.md) | `mcp` | External and in-process Model Context Protocol tools. |
| [Skills](guides/skills.md) | `skill` | Named instruction packages that survive compaction. |
| [Memory](guides/memory.md) | `memory` | File-backed long-term memory and auto-injection. |
| [Background jobs](guides/jobs.md) | `tool` (Manager) | Out-of-band tasks and their notifications. |

## Conventions in these docs

- **Code wins.** Every architectural claim is checked against the source and cites a
  package or file. If a doc and the code disagree, the code is right — please open an
  issue.
- **Domain-neutral.** Norma is a harness, not an application. These docs describe the
  engine, never a specific product built on it.
- **Stable vocabulary.** The same word means the same thing everywhere: *turn*, *tool
  call*, *boundary*, *mode*, *terminal reason*.
