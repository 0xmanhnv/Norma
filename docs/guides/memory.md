# Memory

> Package `memory` · `memory/memory.go`

Memory gives an agent **persistent, file-backed long-term memory** that survives
across sessions — distinct from the in-session conversation, which compaction
manages. Each memory is a Markdown file with YAML frontmatter, stored in a directory
and indexed by a `MEMORY.md` file.

## What a memory is

```go
type Memory struct {
    Name, Description string
    Type    Type      // user | feedback | project | reference
    Content string
    // Path, ModTime ...
}
```

The four types classify intent: `user` (facts about the user), `feedback` (how the
agent should work), `project` (ongoing work, goals, constraints), and `reference`
(pointers to external resources).

## Using it

Enable memory with a store and options:

```go
store := memory.NewStore("/path/to/memory")
opts.Memory = &agentcore.MemoryOptions{
    Store:                store,
    AutoInject:           true, // prepend relevant memories to each prompt
    MaxInject:            3,    // cap (default 3)
    IncludeIndexInPrompt: true, // add the MEMORY.md index to the system prompt
}
```

This adds two tools — **RecallMemory** and **RecordMemory** — so the agent can look up
memories relevant to the task and write new ones. With `AutoInject`, memories relevant
to each prompt are selected and prepended automatically (`Store.Relevant`), rendered
for injection. The index can also be surfaced as a system-prompt segment so the model
knows what it has stored even before recalling.

## Relevance and staleness

The store selects memories by relevance to a query and surfaces staleness information
so the agent can tell when a stored fact may be out of date. Writes create the
directory on first use; the `MEMORY.md` index is maintained alongside the individual
files.
