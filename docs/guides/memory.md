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
    AutoInject:           true, // add relevant memories to each prompt
    MaxInject:            3,    // cap (default 3)
    IncludeIndexInPrompt: true, // add the MEMORY.md index to the system prompt
}
```

This adds two tools — **RecallMemory** and **RecordMemory** — so the agent can look up
memories relevant to the task and write new ones. With `AutoInject`, `Session.Prompt`
picks the memories most relevant to each prompt (`Store.Relevant`) and appends them,
rendered by `memory.RenderForInjection`, to the user's message. The index can also be
surfaced as a system-prompt segment so the model knows what it has stored even before
recalling.

## Relevance and staleness

`Store.Relevant` scores memories by keyword match against their name, description
and content, most relevant first, with recency breaking ties. Rendered memories carry
their age ("today", "yesterday", "5 days ago"), and anything two days old or more
gets a note that it is a point-in-time record to verify before relying on it.
Writes create the directory on first use; the `MEMORY.md` index is maintained
alongside the individual files.
