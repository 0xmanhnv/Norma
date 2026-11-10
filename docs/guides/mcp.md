# MCP (Model Context Protocol)

> Package `mcp` · `mcp/mcp.go`

Model Context Protocol lets an agent use tools exposed by a separate program. Corvid
integrates MCP tools as ordinary `tool.CoreTool`s, so the loop, scheduler, and
permission pipeline treat them exactly like built-in tools. Every MCP tool is named
`mcp__<server>__<tool>` (`mcp.ToolName`).

## Two kinds of server

**External (stdio).** Corvid speaks JSON-RPC 2.0 over a newline-delimited stdio
transport to an external MCP server process, lists its tools, and adapts each into a
`CoreTool`. This is the standard way to pull in third-party MCP servers.

**In-process.** `mcp.InProcessServer(name, tools)` adapts a set of plain Go functions
to the same `mcp__<server>__<tool>` naming, with no subprocess:

```go
tools := mcp.InProcessServer("math", []mcp.InProcessTool{{
    Name:        "add",
    Description: "Adds two numbers.",
    Schema:      map[string]any{ /* JSON Schema */ },
    Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
        // ...
        return "result", nil
    },
}})
// append tools to Options.Tools
```

## Permissions

An adapted MCP tool follows the normal fail-closed path: by default it asks for
approval (`AskUser("call MCP tool mcp__<server>__<tool>?")`). Host allow/deny rules and
modes apply to it like any other tool, matched on its `mcp__...` name.

## Deferred disclosure

MCP tools pair naturally with [deferred tools](tools.md#deferred-tools): list many
servers' tools in `Options.DeferredTools` so their schemas stay out of the model's
context until needed, and let a [skill](skills.md) unlock a server's tools when the
skill that needs them is invoked (a skill's `mcps:` frontmatter).
