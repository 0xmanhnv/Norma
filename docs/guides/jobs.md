# Background jobs

> Package `tool` (the task `Manager`) · `tool/task.go`, `agentcore/agentcore.go`

Some work is too slow to block a turn — a long build, a test run, a watch. Background
jobs let the agent start such work, keep going, and be told when it finishes.

## The manager

When background tasks are enabled, each session gets a per-session `tool.Manager`
(`agentcore.NewSession`) that runs long commands out of band and collects their
output and completion notifications. **Bash** starts a background task when the model
sets `run_in_background`, and the session adds four tools that start, read and control
tasks:

| Tool | Does |
|---|---|
| **Monitor** | Start a long-running streaming command (`tail -f`, a dev server, a polling loop) in the background and return at once with a task id. |
| **TaskOutput** | Read a running (or finished) task's accumulated output. |
| **TaskStop** | Stop a task. |
| **TaskList** | List the session's tasks and their status. |

(The default **sleep** tool, a context-cancellable wait, pairs with these for polling.)

## How completions reach the model

The loop drains the manager's completion notifications into the conversation at two
points: between tool turns and when the model would otherwise stop
(`loop.injectTaskNotifications`). If any landed, the loop takes an extra turn so the
model can react to the finished work — a finished background job is one of the
[continuation gates](agent-loop.md#continuation-gates). Notifications that arrive
between one `Prompt` returning and the next starting are surfaced ahead of the new
user message (`Session.Prompt`).

## Safety

Every command the manager runs — foreground Bash included, since Bash goes through the
manager whenever background tasks are on — is started as the leader of its own process
group, and two watchdogs bound it:

- **Output cap.** A task whose output file grows past 50 MiB is killed, so a runaway
  process cannot fill the disk.
- **Memory cap (Linux).** One sampler per manager checks every running task every 2 s.
  It sums the resident memory (RSS) of each task's whole process group — so a child
  started with `cmd &` that outlives its shell is still counted — and re-checks any
  group over the cap with Pss, which splits shared copy-on-write pages across their
  holders, so a forking worker pool is not mistaken for a leak. A group over the cap on
  both measures is killed. The cap is a guard, not a wall: a process can overshoot by
  whatever it allocates between samples.

A watchdog kill names the cap that fired: the Bash result carries
`killed_by` (`memory_cap` / `output_cap`) and `cap`, the background notification says
which cap was exceeded, and TaskList / TaskOutput show it in place of the exit code.

### Choosing the memory cap

| `AGENT_CORE_TASK_MEM_CAP` | Effect |
|---|---|
| unset (default) | 80% of the memory budget: the cgroup limit (v2 `memory.max`, or v1) when one is set, otherwise total RAM. Floor 512 MiB; 2 GiB if the budget cannot be read. |
| a whole number with an optional unit: `512m`, `2g`, `1536mib`, `4294967296` (`b`/`k`/`m`/`g`/`t` and their `kb`/`kib` spellings, all binary) | That cap, per task. |
| `0` or `off` | Memory guard disabled (the output cap still applies). |
| anything unparseable | The default — a typo never disables the guard. |

The cap applies **per task**, not to the sum of all tasks. The default exists to stop
one runaway command from taking the host down with it; a long-running host that runs
several heavy commands at once, or that shares the box with other services, should set
an explicit cap sized to its workload. Set it in the environment before the process
starts, or with `os.Setenv` before the first `Session` is created — the value is read
when the cap is checked, so it can also be changed later. The Bash tool description
tells the model the active cap, and only where the guard is actually enforced (Linux,
background tasks on, cap not disabled). With background tasks disabled Bash runs
synchronously and neither watchdog applies; off Linux the memory guard stands down.

`Session.Close` kills any still-running tasks and removes their temporary output
directory — long-lived hosts should `defer s.Close()` (the one-shot `Run` does this
automatically).

## Turning it off

Background tasks are on by default. Disable them globally with the
`AGENT_CORE_DISABLE_BACKGROUND_TASKS` environment variable, or per session with
`Options.DisableBackgroundTasks` — useful for focused single-purpose agents that
should only see the tools you explicitly provide.
