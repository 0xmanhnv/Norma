package harness

import (
	"context"
	"encoding/json"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// ParentRun is what an agent started from inside a tool call (a subagent)
// inherits from the run that called the tool: the safety settings that must not
// be lost on the way down, and the plumbing that keeps the child observable.
// The run's deadline is not here; it travels on the call's context.
type ParentRun struct {
	AgentID            string
	PermissionMode     permission.Mode
	Allowed            []string
	Disallowed         []string
	BashEnv            []string
	ToolOutputDir      string
	MaxToolOutputChars int
	TaskOutputTailOnly bool
	SpillStore         tool.SpillStore
	Tracer             Tracer
	OnToolResult       func(ctx context.Context, toolUseID, name string, input json.RawMessage, res tool.Result)
	RewriteToolResult  func(ctx context.Context, toolUseID, name string, input json.RawMessage, res tool.Result) tool.Rewrite
	Settlement         *Settlement
	// ModelSettings are the calling run's cache lifetime, thinking mode and
	// effort, so a child is sent the same way unless it is configured itself.
	ModelSettings llm.ModelSettings
}

type parentKey struct{}

// SubagentUsageReporter is implemented by a tool result's Data when the tool ran
// other agents; the harness adds the usage to Terminal.SubagentUsage.
type SubagentUsageReporter interface {
	SubagentUsage() llm.Usage
}

func (l *loop) addSubagentUsage(u llm.Usage) {
	l.subMu.Lock()
	l.subUsage.Add(u)
	l.subMu.Unlock()
}

func (l *loop) subagentUsage() llm.Usage {
	l.subMu.Lock()
	defer l.subMu.Unlock()
	return l.subUsage
}

// ParentRunFrom returns the calling run's settings when ctx is a tool call's
// context.
func ParentRunFrom(ctx context.Context) (ParentRun, bool) {
	p, ok := ctx.Value(parentKey{}).(ParentRun)
	return p, ok
}

func (l *loop) parentRun(mode permission.Mode) ParentRun {
	return ParentRun{
		AgentID:            l.in.AgentID,
		PermissionMode:     mode,
		Allowed:            l.in.Allowed,
		Disallowed:         l.in.Disallowed,
		BashEnv:            l.in.BashEnv,
		ToolOutputDir:      l.in.ToolOutputDir,
		MaxToolOutputChars: l.in.MaxToolOutputChars,
		TaskOutputTailOnly: l.in.TaskOutputTailOnly,
		SpillStore:         l.in.SpillStore,
		Tracer:             l.in.Tracer,
		OnToolResult:       l.in.OnToolResult,
		RewriteToolResult:  l.in.RewriteToolResult,
		Settlement:         l.in.Settlement,
		ModelSettings:      l.in.ModelSettings,
	}
}

// modeRank orders modes from most to least restrictive, for picking the
// stricter of a parent's and a child's mode.
var modeRank = map[permission.Mode]int{
	permission.ModePlan:        0,
	permission.ModeDontAsk:     1,
	permission.ModeAuto:        2,
	permission.ModeDefault:     3,
	permission.ModeAcceptEdits: 4,
	permission.ModeBypass:      5,
}

// StricterMode returns the more restrictive of two modes. An unknown or empty
// mode counts as ModeDefault.
func StricterMode(a, b permission.Mode) permission.Mode {
	ra, ok := modeRank[a]
	if !ok {
		a, ra = permission.ModeDefault, modeRank[permission.ModeDefault]
	}
	rb, ok := modeRank[b]
	if !ok {
		b, rb = permission.ModeDefault, modeRank[permission.ModeDefault]
	}
	if rb < ra {
		return b
	}
	return a
}
