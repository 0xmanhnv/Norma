# Contributing to Norma

Thanks for your interest in improving Norma. This guide covers how to build and test
the project, how it is laid out, and the conventions the code follows. For a mental
model of the system before you change it, read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Fork model

Norma is a maintained fork of [Autumn-27/norma](https://github.com/Autumn-27/norma).

- The **`patches`** branch is where fork development happens and holds the current API.
  Base your branches on `patches` and open pull requests against it.
- Releases are tagged `v0.4.4-p.N`.
- Keep the upstream copyright and `LICENSE` intact. Norma is Apache-2.0; contributions
  are accepted under the same license.
- Preserve third-party notices. Some paths are derived from other projects under their
  own license (e.g. `noa` / `noaadapter`, ported from acp-kernel under the MIT License).
  Keep their in-source attribution comments intact, and if you add or extend such code,
  record it in [`THIRD-PARTY-LICENSES.md`](THIRD-PARTY-LICENSES.md).

## Prerequisites

- **Go 1.26+** (the `go` directive in [`go.mod`](go.mod)). The agent loop streams
  through `iter`, which landed in Go 1.23.
- No other toolchain is required. There is no Makefile and no code generation —
  everything is plain `go`.

Dependencies are intentionally few (`go.mod`): a PTY library and a terminal emulator
(for the shell tools' tests), `golang.org/x/net`, and `gopkg.in/yaml.v3`. Please keep
the dependency footprint small.

## Build, test, and format

```bash
go build ./...     # compile everything (incl. examples and the CLI demo)
go vet ./...       # static checks
gofmt -l .         # list files that need formatting (should print nothing you touched)
go test ./...      # the full suite — offline, no API keys needed
```

Run all four before opening a pull request. The test suite is designed to run
**offline**: provider adapters are exercised over `httptest` SSE, the loop is driven
by a scripted provider through `harness.QueryDeps`, tools run in temp dirs, and the
MCP client talks over in-memory pipes. If a change needs the network to be tested, it
probably needs a seam instead.

Format any file you touch with `gofmt -w <file>` (or `gofmt -w .`). CI is not wired up,
so these local checks are the gate.

## Repository layout

```
agentcore/     Public entry point: Session, Options, Run, Resume
harness/       The agent loop: streaming, scheduling, recovery, gates
llm/           Message model, Provider interface, Anthropic/OpenAI/Responses adapters, error taxonomy
tool/          CoreTool contract, Registry, built-in tools, typed output, background-task Manager
permission/    The safety boundary: decisions, modes, the four-stage pipeline
compaction/    Built-in non-destructive context-window management
noa/           Opt-in model-driven compression core (pure library)
noaadapter/    Wires noa into a Session
hook/          Eight lifecycle callbacks
subagent/      Context-isolated child agents (the Agent tool)
coordinator/   Multi-agent coordination over subagents
mcp/           Model Context Protocol: stdio + in-process servers
plan/          Plan mode (read-only exploration → approval → implement)
memory/        File-backed long-term memory
skill/         SKILL.md instruction packages
transcript/    Append-only JSONL persistence + resume
cmd/agentcore/ A runnable CLI demo (not imported by the library)
examples/      Scenario wiring, end to end
docs/          Architecture and subsystem guides
```

Dependencies flow one way: `agentcore` composes everything; `llm` depends on nothing
else in the tree. See the package map in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#2-package-map).

## Conventions

- **Code is the source of truth.** Docs cite package/file paths and must match the
  code; when they drift, fix the doc (or the code) in the same spirit. Every package
  opens with a doc comment that states its job — keep it accurate.
- **The harness stays domain-neutral.** Norma is an engine, not an application. It
  ships no preset system prompt and no scenario logic; those belong in `examples/` or
  in the host. Don't bake a use case into the core.
- **Tools fail closed.** A new tool defaults to not-read-only, not-concurrency-safe,
  approval-required (`tool.Build`). Opt into `ReadOnly` / `Concurrent` / permissive
  `Permissions` deliberately, and add a test that proves the boundary.
- **Context management is non-destructive.** History is retained in full; only the
  projection sent to the provider shrinks. Don't delete messages from the loop's
  history.
- **Streaming is an iterator.** Public entry points return `iter.Seq2[Event, error]`;
  cancellation is a `context.Context`. Side effects are injected through `QueryDeps`
  so they can be tested without a network.
- **Test what you add.** The suite already covers both adapters, the loop end to end,
  the permission matrix, compaction, hooks, subagents, and MCP. New behavior comes
  with new tests in the same style.

### Adding things

- **A tool.** Build it with `tool.Build(tool.Spec{...})`, give it a JSON-Schema input,
  and set the behavioral predicates only if it is genuinely read-only / concurrency-safe.
  See `examples/custom-llm` and the [tools guide](docs/guides/tools.md).
- **A provider format.** Implement `llm.Provider` (`Stream` + `Complete`) translating
  to and from the vendor-neutral content-block model, and classify failures into the
  `llm.Code` taxonomy at the adapter boundary.
- **A subsystem tool pack.** Follow the existing pattern: a package that exposes a
  `Tools()`/`Tool()` constructor returning `CoreTool`s, wired in through `agentcore.Options`.

## Pull requests

- Keep each PR coherent and focused.
- Describe what changed and why; link the issue if there is one.
- Make sure `go build ./...`, `go vet ./...`, `gofmt -l .`, and `go test ./...` are
  clean for what you touched.
- Do not add automated authorship/attribution trailers to commits.
