# Examples

Each example is a self-contained `package main` that wires Norma for one scenario.
They share no framework code — the point is to show how little it takes to assemble an
agent from the library, and how the same loop drives very different behavior depending
only on the prompt, the tools, and the policy you hand it.

All examples read the model from the environment and need an API key for the provider
they use (`OPENAI_API_KEY` or `ANTHROPIC_API_KEY`). They compile as part of
`go build ./...`.

| Example | Demonstrates | Run |
|---|---|---|
| [`minimal`](minimal/) | The smallest useful agent: provider + host prompt + default toolset + the streaming loop. | `go run ./examples/minimal` |
| [`custom-llm`](custom-llm/) | Pointing the SDK at any model (OpenAI/Anthropic/DeepSeek), a custom non-coding system prompt, and a custom tool. | `go run ./examples/custom-llm` |
| [`coding-agent`](coding-agent/) | A full coding agent: interactive permission prompts, a `PreToolUse` hook, and compaction. | `go run ./examples/coding-agent` |
| [`security-audit`](security-audit/) | A read-only auditor that delegates deeper work to a subagent (context isolation). | `go run ./examples/security-audit` |
| [`planner-memory`](planner-memory/) | Plan mode (explore → approve → implement) plus persistent long-term memory. | `go run ./examples/planner-memory` |
| [`coordinator`](coordinator/) | A coordinator that decomposes work and fans it out to isolated worker subagents. | `go run ./examples/coordinator` |

## Reading order

If you are new to the library, start with **`minimal`** (the quickstart as a program),
then **`custom-llm`** (how the provider, prompt, and tools are the only things that
change between scenarios). From there pick the example closest to what you are building.

## A note on permissions

`minimal` and `custom-llm` use `permission.ModeBypass` so they run without prompting —
appropriate for a local demo, not for anything that touches real systems. The
`coding-agent` example shows the realistic path: a `CanUseTool` callback that asks
before every mutating action. See the [permissions guide](../docs/guides/permissions.md).
