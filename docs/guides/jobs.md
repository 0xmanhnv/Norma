# Background jobs

> Package `tool` (the task `Manager`) · `tool/task.go`, `agentcore/agentcore.go`

Some work is too slow to block a turn — a long build, a test run, a watch. Background
jobs let the agent start such work, keep going, and be told when it finishes.

## The manager

When background tasks are enabled, each session gets a per-session `tool.Manager`
(`agentcore.NewSession`) that runs long commands out of band and collects their
output and completion notifications. Four tools read and control it:

| Tool | Does |
|---|---|
| **TaskOutput** | Read a running (or finished) task's accumulated output. |
| **TaskStop** | Stop a task. |
| **TaskList** | List the session's tasks and their status. |
| **Monitor** | Wait on a task / condition. |

(The default **Sleep** tool, a context-cancellable wait, pairs with these for polling.)

## How completions reach the model

The loop drains the manager's completion notifications into the conversation at two
points: between tool turns and when the model would otherwise stop
(`loop.injectTaskNotifications`). If any landed, the loop takes an extra turn so the
model can react to the finished work — a finished background job is one of the
[continuation gates](agent-loop.md#continuation-gates). Notifications that arrive
between one `Prompt` returning and the next starting are surfaced ahead of the new
user message (`Session.Prompt`).

## Safety

On Linux every spawned task is bounded by a per-task memory cap
(`AGENT_CORE_TASK_MEM_CAP`, defaulting to a share of the cgroup or host memory
budget); a command whose process tree exceeds it has its process group killed, and
the cap is named in the resulting error and notification. `Session.Close` kills any
still-running tasks and removes their temporary output directory — long-lived hosts
should `defer s.Close()` (the one-shot `Run` does this automatically).

## Turning it off

Background tasks are on by default. Disable them globally with the
`AGENT_CORE_DISABLE_BACKGROUND_TASKS` environment variable, or per session with
`Options.DisableBackgroundTasks` — useful for focused single-purpose agents that
should only see the tools you explicitly provide.
