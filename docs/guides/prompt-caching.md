# Keeping the prompt cache warm

> Packages `llm`, `harness`, `compaction`
> · `llm/anthropic.go`, `llm/openai.go`, `llm/settings.go`, `compaction/compaction.go`

An agent loop sends the whole conversation again on every turn. Without a prompt
cache you pay full input price for all of it, every time; with one, everything the
provider has already seen costs about a tenth. On a long tool loop that is most of the
bill. Corvid asks for the cache by default, but a cache is easy to break without
noticing. This page explains how it works and what keeps it working.

## How a prompt cache works

The rules below are the providers', not Corvid's. They are worth knowing because every
one of them has a consequence for how you build a session.

- **It is a prefix match.** The provider renders a request in a fixed order — tools,
  then system prompt, then messages — and can reuse a cached entry only if the new
  request starts with exactly the same bytes. Change one byte and everything after it
  is new.
- **You mark where a prefix ends.** A request may carry up to four markers
  (breakpoints). Each one says "cache everything up to here".
- **Writing costs more, reading costs much less.** Writing an entry costs about 1.25×
  the normal input price (2× for a one-hour entry). Reading it costs about 0.1×. A
  prefix that is written and never read again is a loss.
- **Entries expire.** Five minutes by default, refreshed each time they are read. One
  hour if you ask (and pay) for it.
- **A marker only looks back so far.** The provider checks about twenty content blocks
  before a marker for an earlier entry. A turn that adds more than that in one go needs
  a marker of its own closer to the old tail.
- **A cache belongs to one model.** A different model reads nothing, however identical
  the prompt.
- **An entry is readable once its first response has started.** Ten requests with the
  same prefix sent at the same moment all pay full price. Send one first.

## What Corvid does for you

On the Anthropic format (`llm.FormatAnthropic`) every request is marked, with no
configuration:

| Marker | Where | Why |
|---|---|---|
| tools | the last tool schema | the tool list is cached on its own, so it survives a changed system prompt |
| system | the end of the static system prefix | see [the boundary](#the-system-prompt-boundary) |
| previous tail | the end of the message before the latest assistant turn | where the previous request put its tail, so the read is guaranteed |
| tail | the last block this request adds that the next one will send again | what the next turn reads |

That is the four a provider allows. The loop appends to the conversation and changes
nothing before the tail, so each turn reads everything the previous turn wrote and
writes only what is new.

To send a request with no markers at all — a one-off call whose prefix nothing will
repeat — set `CompletionRequest.DisableCache`, or `ModelSettings.DisableCache` for a
whole session.

### The system prompt boundary

`Options.DynamicBoundary` is the number of leading system segments that never change.
The marker goes at the end of them; segments after it are sent uncached.

```go
agentcore.Options{
    SystemPrompt:       []string{rules, style},     // never changes
    AppendSystemPrompt: []string{todaysNotes()},    // changes
    DynamicBoundary:    2,
}
```

Leave it at zero and the whole system prompt is treated as static, which is right when
it really is. If part of your system prompt changes between requests, put that part
last and set the boundary in front of it — otherwise each change rewrites the system
entry and the whole conversation behind it.

### OpenAI-compatible endpoints

OpenAI's own API caches automatically and takes no marker. Many gateways, though,
serve Claude and other explicitly cached models through the Chat Completions format
and read a `cache_control` marker from a content part. For those:

```go
llm.NewProvider(llm.Config{Format: llm.FormatOpenAI, BaseURL: gateway, Model: model,
    CacheControl: true})
```

The adapter then sends the system prompt and the marked messages as arrays of text
parts and marks the end of the tool list, the static system prefix, the previous tail
and the tail. Tools render first, so sessions with identical tools share that entry,
and sessions that also share the static system prompt share the next one. It is off by
default because a strict OpenAI-compatible endpoint rejects a field it does not know.

### Subagents

A subagent started by the `Agent` tool is sent with its parent's `ModelSettings`, and
caches by default like any other run. Workers started by `coordinator.RunParallel` have
no parent run: give them settings through `coordinator.Config.ModelSettings`.

Several workers with the same prompt started at the same instant cannot read each
other's entries. If they share a large prefix, start one, wait for its first event,
then start the rest.

## Session settings

`llm.ModelSettings` (on `agentcore.Options` and `harness.QueryInput`) holds what is
decided once per session:

```go
agentcore.Options{
    ModelSettings: llm.ModelSettings{
        CacheTTL:             "1h",   // tools and static system prefix
        ConversationCacheTTL: "1h",   // the conversation markers
        Thinking:             "adaptive",
        ReasoningEffort:      "medium",
    },
}
```

- **`CacheTTL`** is the lifetime of the tools and system entries. Use `"1h"` when
  sessions that share a system prompt start more than five minutes apart.
- **`ConversationCacheTTL`** is the lifetime of the conversation entries. The default
  five minutes is right for a loop that keeps moving. If your agent can sit idle for
  longer — a slow tool, a person who has to answer — the entry expires and the next
  request writes the whole history again; `"1h"` avoids that at twice the write price.
  A longer lifetime may not follow a shorter one, so one hour here puts the tools and
  system markers on one hour too.
- **`Thinking` and `ReasoningEffort`** are part of what a cache entry is keyed on.
  That is why they are session settings: change one in the middle of a conversation and
  the next request pays for the whole history. Pick them at the start.

A `Thinking` of `"disabled"` is only sent to an endpoint whose provider was configured
with a `ThinkingType`; otherwise the field is left out, which is how an endpoint that
never took the field is asked not to think.

## Things that break the cache

Each of these changes a byte early in the request. The turn that does it pays full
price for everything after that byte, and writes it all to the cache again.

| What | Why it breaks | What to do |
|---|---|---|
| A timestamp, request id or counter in the system prompt | the system prompt is near the front of every request | put it in the user message, or after the boundary |
| Tools added, removed or reordered mid-session | tools render first | fix the tool list for the session; to take a tool away, refuse its calls rather than hiding its schema (what `Settlement.DisabledTools` does by default) |
| Changing model, thinking mode or effort mid-session | part of the cache key | per session, see above |
| Editing or deleting an earlier message | everything after it is new | append instead |
| A gap longer than the cache lifetime | the entry is gone | `ConversationCacheTTL: "1h"` |
| Per-request context placed at the start of the conversation | it sits in front of the whole history | put it at the end: the system reminder and `Options.WorkingFrame` are appended to the last user message, after the last marker, so they can change on every request for free |
| A memory index in the cached system prompt | it changes on every saved memory | with no `DynamicBoundary` of yours, the boundary is placed before the index for you |

Compaction is the one thing that has to rewrite history, so it is built to do it
rarely and in large steps:

- **MicroCompact** clears old tool results in batches. A pass runs only when it frees
  at least `compaction.Config.MicroMinFree` tokens (a tenth of the window by default),
  and between passes the history is byte-identical.
- **The summary** is made over the run's own prefix: the same tools and system prompt,
  the conversation so far, then the instruction to summarize as the last message. That
  request starts with exactly what the previous turn sent, so the history is a cache
  read rather than fresh input. Its cost is added to the run's usage.
- A **separate, cheaper summarizer model** is possible
  (`compaction.Config.Summarizer = compaction.StandaloneSummarizer(provider, …)`), but
  it cannot read the session's cache — a cache is per model — so it pays for what it
  summarizes at full input price. Compare that with the price difference before
  choosing it.

See the [compaction guide](compaction.md) for the thresholds.

## Checking that it works

Usage tells you. `llm.Usage` reports, per request and summed over a run:

| Field | Meaning |
|---|---|
| `InputTokens` | the **total** input, cached or not |
| `CacheReadTokens` | the part served from the cache (about 0.1× price) |
| `CacheWriteTokens` | the part written to the cache (about 1.25× price) |

The cache counts are parts of `InputTokens`, not additions to it. The full-price input
is `InputTokens - CacheReadTokens - CacheWriteTokens`; do not add the three together.

In a healthy tool loop, from the second turn on, `CacheReadTokens` is close to the
previous turn's `InputTokens` and `CacheWriteTokens` is roughly what the last turn
added. If `CacheReadTokens` stays at zero, or `CacheWriteTokens` is the size of the
whole conversation turn after turn, something in the table above is changing the
prefix.

On OpenAI-compatible endpoints the adapter reads whichever of these the endpoint
reports and normalizes them to the fields above:

| Read from the cache | Written to the cache |
|---|---|
| `prompt_tokens_details.cached_tokens` | `prompt_tokens_details.cache_write_tokens` |
| `cache_read_input_tokens` | `cache_creation_input_tokens` |
| `prompt_cache_hit_tokens` | `prompt_tokens_details.cache_creation_tokens` |

A gateway that reports the same tokens under two names is counted once.
`prompt_tokens` is taken as the total; the cache counts are added to it only when they
are larger than it, which means the endpoint reported the uncached part alone.

## When the provider is busy

A failed request that is sent again reads the cache like any other, so retries are
cheap — as long as they wait long enough to succeed. `llm.Config`:

- `MaxRetries` (default 5) retries a request that could not be established: a
  connection error or reset before the response starts, or a 408, 429, 500, 502, 503,
  504 or 529. A response that has started is never sent again.
- A `Retry-After` from the server is honoured. Otherwise the wait doubles from half a
  second, with jitter. `RetryMaxDelay` (default one minute) caps any single wait.
- `StreamIdleTimeout` fails a stream that has gone silent, instead of waiting on it
  until the run's context ends. Off by default; a minute or two is a safe value.
