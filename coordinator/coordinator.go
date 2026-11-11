// Package coordinator implements coordinator mode: a top-level agent that
// decomposes work and delegates each unit to isolated "worker" subagents
// instead of doing the work itself. It offers two paths:
// an LLM-driven one (expose AgentTool + SystemSegment so the model spawns
// workers via the Agent tool) and a programmatic one (RunParallel fans tasks
// out to workers concurrently and returns their results).
package coordinator

import (
	"context"
	"strings"
	"sync"

	"github.com/0xmanhnv/corvid/harness"
	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/subagent"
	"github.com/0xmanhnv/corvid/tool"
)

// DefaultWorkerPrompt is the default system prompt for worker subagents.
const DefaultWorkerPrompt = `You are a worker agent spawned by a coordinator. Complete the task described in the prompt thoroughly and report back a concise summary of what you did and found.

Guidelines:
- Complete the task fully — don't leave it half-done, but don't gold-plate either.
- Use tools proactively: read files, search code, run commands, edit files.
- Be thorough in research: check multiple locations and naming conventions.
- For implementation: make targeted changes and run tests to verify.
- Report back with actionable findings — the coordinator will synthesize your results.
- If you hit errors, investigate and try to fix them before reporting failure.`

// Config configures coordinator mode.
type Config struct {
	Provider       llm.Provider
	WorkerTools    *tool.Registry  // tools available to workers (e.g. the core toolset)
	WorkerPrompt   []string        // worker system prompt (default DefaultWorkerPrompt)
	PermissionMode permission.Mode // workers' permission mode (default acceptEdits)
	WorkingDir     string
	MaxTurns       int // per-worker turn cap
	MaxDepth       int // subagent recursion cap (default 3)
	MaxParallel    int // bounded concurrency for RunParallel (default 4)
	CanUseTool     permission.CanUseTool
	Hooks          harness.HookRunner
	// ModelSettings are the workers' cache lifetime, thinking mode and effort
	// (see subagent.Config.ModelSettings). Workers started by the coordinator
	// model inherit its session's settings when this is zero; workers started
	// by RunParallel have no calling run, so set it here.
	ModelSettings llm.ModelSettings

	// OnResult, when set, is called once per task from RunParallel as that
	// worker settles — in addition to the result being stored — so a host can
	// surface progress while the fleet is still running. Calls can arrive from
	// several worker goroutines at once, so it must be safe to call
	// concurrently; RunParallel serializes the calls but does not order them by
	// task index.
	OnResult func(index int, r Result)

	// StopOnFatal, when true, makes a worker that fails with a credential- or
	// configuration-level provider error trip a circuit breaker: RunParallel
	// stops launching the workers that have not started yet instead of driving
	// the whole fleet into the same failure, and those tasks settle with the
	// cancellation error. Transient failures (rate-limit, unavailable) and
	// task-specific errors never trip it. Default false preserves the
	// launch-everything behavior.
	StopOnFatal bool
}

func (c *Config) workerPrompt() []string {
	if len(c.WorkerPrompt) > 0 {
		return c.WorkerPrompt
	}
	return []string{DefaultWorkerPrompt}
}

func (c *Config) mode() permission.Mode {
	if c.PermissionMode != "" {
		return c.PermissionMode
	}
	return permission.ModeAcceptEdits
}

// subagentConfig wires a subagent.Config with a single "worker" definition.
func (c *Config) subagentConfig() subagent.Config {
	return subagent.Config{
		Provider:    c.Provider,
		ParentTools: c.WorkerTools,
		MaxDepth:    c.MaxDepth,
		CanUseTool:  c.CanUseTool,
		Hooks:       c.Hooks,

		ModelSettings: c.ModelSettings,
		Agents: map[string]subagent.Definition{
			"worker": {
				Description:    "Executes a research, implementation, or verification task autonomously and reports back.",
				Prompt:         c.workerPrompt(),
				PermissionMode: c.mode(),
				MaxTurns:       c.MaxTurns,
			},
		},
	}
}

// AgentTool returns the Agent tool the coordinator LLM uses to spawn workers.
// Add it (and nothing else mutating) to the coordinator session's tools.
func (c *Config) AgentTool() tool.CoreTool {
	return subagent.NewTool(c.subagentConfig())
}

// SystemSegment returns coordinator guidance to append to the system prompt.
func (c *Config) SystemSegment() string {
	var names []string
	if c.WorkerTools != nil {
		for _, t := range c.WorkerTools.List() {
			names = append(names, t.Name())
		}
	}
	return "# Coordinator mode\n" +
		"You orchestrate work by delegating to worker subagents — you do NOT implement directly. " +
		"Break the request into independent units and spawn a worker for each via the Agent tool " +
		"(agent_type \"worker\"), then synthesize their reports into a final answer. " +
		"Prefer parallel, independent units. Workers have these tools: " + strings.Join(names, ", ") + "."
}

// Result is the outcome of one programmatic worker run.
type Result struct {
	Task   string
	Output string
	Err    error
}

// RunParallel fans tasks out to workers concurrently (bounded by MaxParallel)
// and returns results in task order. When OnResult is set it reports each
// result as that worker settles; when StopOnFatal is set a credential- or
// configuration-level failure short-circuits the remaining launches (see the
// Config fields).
func (c *Config) RunParallel(ctx context.Context, tasks []string) []Result {
	cfg := c.subagentConfig()
	results := make([]Result, len(tasks))
	limit := c.MaxParallel
	if limit <= 0 {
		limit = 4
	}
	// A private child context lets the breaker cancel the fleet without
	// disturbing the caller's context. The defer catches the normal return.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// report stores a result and, under a mutex, forwards it to OnResult. Each
	// task owns a distinct slice index, so only the callback needs guarding.
	var cbMu sync.Mutex
	report := func(i int, r Result) {
		results[i] = r
		if c.OnResult != nil {
			cbMu.Lock()
			c.OnResult(i, r)
			cbMu.Unlock()
		}
	}

	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, task := range tasks {
		if ctx.Err() != nil {
			// The breaker has tripped; settle the rest without launching them.
			report(i, Result{Task: task, Err: ctx.Err()})
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, task string) {
			defer wg.Done()
			defer func() { <-sem }()
			out, err := subagent.Run(ctx, cfg, "worker", task, c.WorkingDir)
			report(i, Result{Task: task, Output: out, Err: err})
			if c.StopOnFatal && fatalFleetError(err) {
				cancel()
			}
		}(i, task)
	}
	wg.Wait()
	return results
}

// fatalFleetError reports whether err is a provider failure that will defeat
// every worker equally — rejected credentials or an unusable configuration —
// so there is no point launching the rest of the fleet into it. Transient
// codes (rate_limited, unavailable) and task-specific failures are not fatal.
func fatalFleetError(err error) bool {
	switch llm.CodeOf(err) {
	case llm.CodeAuth, llm.CodeInvalidConfig:
		return true
	default:
		return false
	}
}
