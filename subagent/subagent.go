// Package subagent lets the agent delegate to isolated child agents (FR-09).
// A subagent reuses the same harness loop with its own prompt, tool subset, and
// permission mode, giving context isolation. It is exposed to the parent as an
// "Agent" CoreTool; recursion is bounded by a depth guard carried on the
// context.
package subagent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/0xmanhnv/corvid/harness"
	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
	"github.com/0xmanhnv/corvid/transcript"
)

// Definition configures a named subagent (FR-09.1).
type Definition struct {
	Description    string          // shown to the parent model in the Agent tool prompt
	Prompt         []string        // the subagent's system prompt
	Tools          []string        // tool names it may use (empty = inherit all)
	PermissionMode permission.Mode // can only tighten the parent's mode; empty = the parent's
	MaxTurns       int
	// ParseOutput, when set, turns the subagent's final text into a typed
	// value: decode the JSON it was asked for, pull a verdict out of its
	// report. The value is carried on ChildSummary.Output for the host
	// (harness.QueryInput.OnToolResult, coordinator.Result); the model that
	// called the Agent tool still reads the text. A parse error is carried on
	// ChildSummary.OutputErr and does not fail the call. It is not called for
	// a run that ended in an error.
	ParseOutput func(finalText string) (any, error)
}

// Config wires the Agent tool.
type Config struct {
	Provider    llm.Provider
	ParentTools *tool.Registry
	Agents      map[string]Definition
	MaxDepth    int // default 3
	CanUseTool  permission.CanUseTool
	Hooks       harness.HookRunner
	// Transcript, when non-nil, persists each subagent run to its own sidechain
	// file under the parent session (whose id is read from the context). Subagent
	// tokens are recorded there, separate from the parent's totals. Where the
	// file is comes back with the result (ChildSummary.SessionID, AgentID and
	// TranscriptPath), so the child's whole conversation can be read later.
	Transcript *transcript.Store
	// ModelSettings are the cache lifetime, thinking mode and effort of every
	// subagent request. Left zero, a subagent started from a tool call uses
	// the calling run's (harness.ParentRun), so it caches the way its parent
	// does. Prompt caching itself is on by default either way.
	ModelSettings llm.ModelSettings
	// Budget is the spending limit of a subagent started with no calling run
	// (Run, RunWithSummary, the coordinator). One started from a tool call
	// charges its parent's Budget (harness.ParentRun) instead.
	Budget harness.Budget
}

type ctxKey int

const depthKey ctxKey = 0

func depth(ctx context.Context) int {
	v, _ := ctx.Value(depthKey).(int)
	return v
}

// ToolName is the Agent tool's name as the model sees it.
const ToolName = "Agent"

// NewTool builds the "Agent" CoreTool that dispatches to a named subagent.
func NewTool(cfg Config) tool.CoreTool {
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = 3
	}
	var names string
	for n, d := range cfg.Agents {
		names += fmt.Sprintf("\n- %s: %s", n, d.Description)
	}
	return tool.Build(tool.Spec{
		Name:        ToolName,
		Description: "Delegates a self-contained sub-task to an isolated subagent that runs with its own prompt and tools, returning only its final answer. Use it to parallelize independent work or to keep large intermediate output out of the main context.",
		Prompt:      "Available agent_type values:" + names,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_type": map[string]any{"type": "string", "description": "Which subagent to run."},
				"prompt":     map[string]any{"type": "string", "description": "The task for the subagent."},
			},
			"required": []any{"agent_type", "prompt"},
		},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed() // the subagent's own tool calls are gated internally
		},
		Run: func(ctx context.Context, input json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
			var in struct {
				AgentType string `json:"agent_type"`
				Prompt    string `json:"prompt"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			if depth(ctx) >= cfg.MaxDepth {
				return tool.Errorf(fmt.Sprintf("Error: subagent recursion depth %d exceeded", cfg.MaxDepth)), nil
			}
			def, ok := cfg.Agents[in.AgentType]
			if !ok {
				return tool.Errorf(fmt.Sprintf("Error: unknown agent_type %q", in.AgentType)), nil
			}
			text, sum, err := run(ctx, cfg, def, in.AgentType, in.Prompt, tc.WorkingDir)
			if err != nil {
				res := tool.Errorf("Error: subagent failed: " + err.Error())
				res.Data = sum
				return res, nil
			}
			res := tool.Text(text)
			res.Data = sum
			return res, nil
		},
	})
}

// Run executes the named agent from cfg.Agents to completion and returns its
// final text. It is the programmatic entry point (the Agent tool uses the same
// machinery); callers like the coordinator use it to fan out work directly.
func Run(ctx context.Context, cfg Config, agentType, prompt, workDir string) (string, error) {
	def, ok := cfg.Agents[agentType]
	if !ok {
		return "", fmt.Errorf("subagent: unknown agent_type %q", agentType)
	}
	text, _, err := run(ctx, cfg, def, agentType, prompt, workDir)
	return text, err
}

// RunWithSummary is Run that also returns the ChildSummary: what the run cost
// and how it ended, where its sidechain transcript is (when cfg.Transcript is
// set) and its typed output (when the definition has a ParseOutput). The
// summary is filled in as far as the run got even when err is non-nil.
func RunWithSummary(ctx context.Context, cfg Config, agentType, prompt, workDir string) (string, ChildSummary, error) {
	def, ok := cfg.Agents[agentType]
	if !ok {
		return "", ChildSummary{AgentType: agentType}, fmt.Errorf("subagent: unknown agent_type %q", agentType)
	}
	return run(ctx, cfg, def, agentType, prompt, workDir)
}

// ChildSummary is the Data of an Agent tool result: what the subagent cost and
// how it ended, where its conversation was recorded, and its typed output. Its
// usage includes the subagent's own subagents.
type ChildSummary struct {
	AgentType string
	Usage     llm.Usage
	Turns     int
	Reason    harness.TerminalReason

	// The locator of the sidechain transcript, set when Config.Transcript is:
	// SessionID is the session the run was recorded under (the parent's),
	// AgentID names this run within it, and TranscriptPath is the file, as
	// transcript.Store.AgentPath(SessionID, AgentID) gives it. Load it with
	// transcript.Store.Load to read the child's whole conversation, of which
	// the parent only ever saw the final text. All empty without a store.
	SessionID      string
	AgentID        string
	TranscriptPath string

	// Output is the value Definition.ParseOutput made of the final text, nil
	// when the definition has none, the run failed, or parsing did
	// (OutputErr then says why).
	Output    any
	OutputErr error
}

// SubagentUsage lets the calling run add the child's usage to its totals.
func (c ChildSummary) SubagentUsage() llm.Usage { return c.Usage }

// run executes a subagent to completion and returns its final text.
//
// When it is started from a tool call, the child inherits the calling run's
// safety settings and plumbing (harness.ParentRun): its deny and allow rules,
// BashEnv, tool-output spill settings and store, tracer, OnToolResult,
// RewriteToolResult and Settlement. Its
// permission mode is the stricter of the parent's and the definition's, so a
// child never runs with fewer restrictions than its parent.
func run(ctx context.Context, cfg Config, def Definition, agentType, prompt, workDir string) (string, ChildSummary, error) {
	sum := ChildSummary{AgentType: agentType}
	if cfg.Hooks != nil {
		if hs, ok := cfg.Hooks.(interface {
			FireSubagentStart(context.Context, string)
		}); ok {
			hs.FireSubagentStart(ctx, agentType)
		}
	}
	registry := filterTools(cfg.ParentTools, def.Tools)
	childCtx := context.WithValue(ctx, depthKey, depth(ctx)+1)

	seed := llm.UserText(prompt)
	in := harness.QueryInput{
		Provider:       cfg.Provider,
		System:         def.Prompt,
		Messages:       []llm.Message{seed},
		Tools:          registry,
		MaxTurns:       def.MaxTurns,
		WorkingDir:     workDir,
		AgentID:        agentType,
		PermissionMode: def.PermissionMode,
		CanUseTool:     cfg.CanUseTool,
		Hooks:          cfg.Hooks,
		ModelSettings:  cfg.ModelSettings,
		Budget:         cfg.Budget,
	}
	if p, ok := harness.ParentRunFrom(ctx); ok {
		if in.ModelSettings == (llm.ModelSettings{}) {
			in.ModelSettings = p.ModelSettings
		}
		mode := p.PermissionMode
		if def.PermissionMode != "" {
			mode = harness.StricterMode(p.PermissionMode, def.PermissionMode)
		}
		in.PermissionMode = mode
		in.Allowed = p.Allowed
		in.Disallowed = p.Disallowed
		in.BashEnv = p.BashEnv
		in.ToolOutputDir = p.ToolOutputDir
		in.MaxToolOutputChars = p.MaxToolOutputChars
		in.TaskOutputTailOnly = p.TaskOutputTailOnly
		in.SpillStore = p.SpillStore
		in.Tracer = p.Tracer
		in.OnToolResult = p.OnToolResult
		in.RewriteToolResult = p.RewriteToolResult
		in.Settlement = p.Settlement
		in.Policy = p.Policy
		if p.Budget != nil {
			in.Budget = p.Budget
		}
		if p.AgentID != "" {
			in.AgentID = p.AgentID + "/" + agentType
		}
	}
	// Persist this run to its own sidechain under the parent session.
	if cfg.Transcript != nil {
		parentSID := transcript.SessionIDFrom(ctx)
		if parentSID == "" {
			parentSID = transcript.NewSessionID()
		}
		agentID := transcript.NewAgentID()
		sum.SessionID, sum.AgentID = parentSID, agentID
		sum.TranscriptPath = cfg.Transcript.AgentPath(parentSID, agentID)
		w := cfg.Transcript.NewWriter(parentSID, agentID)
		w.RecordMessage(seed, llm.Usage{})
		in.Recorder = w
	}
	var final string
	var rerr error
	for ev, err := range harness.Query(childCtx, in, harness.QueryDeps{}) {
		if err != nil {
			return "", sum, err
		}
		if ev.Kind == harness.KindResult && ev.Terminal != nil {
			final = ev.Terminal.Text
			rerr = ev.Terminal.Err
			sum.Usage = ev.Terminal.Usage
			sum.Usage.Add(ev.Terminal.SubagentUsage)
			sum.Turns = ev.Terminal.Turns
			sum.Reason = ev.Terminal.Reason
		}
	}
	if rerr == nil && def.ParseOutput != nil {
		sum.Output, sum.OutputErr = def.ParseOutput(final)
	}
	return final, sum, rerr
}

// filterTools returns a registry with only the named tools (all if names empty).
func filterTools(parent *tool.Registry, names []string) *tool.Registry {
	if parent == nil {
		return tool.NewRegistry()
	}
	if len(names) == 0 {
		return parent
	}
	keep := map[string]bool{}
	for _, n := range names {
		keep[n] = true
	}
	var sel []tool.CoreTool
	for _, t := range parent.List() {
		if keep[t.Name()] {
			sel = append(sel, t)
		}
	}
	return tool.NewRegistry(sel...)
}
