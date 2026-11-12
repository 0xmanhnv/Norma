package agentcore

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/0xmanhnv/corvid/memory"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/skill"
	"github.com/0xmanhnv/corvid/subagent"
	"github.com/0xmanhnv/corvid/tool"
)

// selfAllowed lists the built-in tools that change something yet allow
// themselves without asking, each with the reason it is safe. A tool that is
// added to this list needs the same review: what it can change, and why that
// needs no approval.
var selfAllowed = map[string]string{
	tool.ExecuteExtraToolName: "runs nothing itself: the loop checks the deferred tool it names under that tool's name (ToolContext.RunDeferred)",
	tool.TodoWriteName:        "changes only the session's in-memory todo list",
	tool.AskUserQuestionName:  "asks the person a question; the answer is theirs",
	tool.TaskStopName:         "stops a background task this session started",
	tool.ShellCloseName:       "closes an interactive session this session opened",
	subagent.ToolName:         "the subagent's own tool calls go through the same permission pipeline",
}

// sampleInput is an input each tool's schema accepts, for tools whose
// permission check reads its input.
var sampleInput = map[string]string{
	tool.BashName:      `{"command":"ls"}`,
	tool.ShellOpenName: `{"command":"bash"}`,
	tool.ShellSendName: `{"session_id":"s1","text":"ls"}`,
	tool.WriteName:     `{"file_path":"/tmp/x","content":"x"}`,
	tool.EditName:      `{"file_path":"/tmp/x","old_string":"a","new_string":"b"}`,
	tool.WebFetchName:  `{"url":"https://example.com/"}`,
}

// allToolsSession builds a session with every built-in tool corvid can add.
func allToolsSession(t *testing.T) *Session {
	t.Helper()
	tools := append(tool.DefaultTools(), tool.ShellSessionTools()...)
	tools = append(tools, subagent.NewTool(subagent.Config{}))
	s := NewSession(Options{
		Tools:           tools,
		Plan:            &PlanOptions{},
		Memory:          &MemoryOptions{Store: memory.NewStore(t.TempDir())},
		Skills:          skill.NewRegistry(skill.Skill{Name: "s", Description: "d"}),
		EnableWebFetch:  true,
		EnableWebSearch: true,
		Todos:           tool.NewTodoStore(),
		AskUser:         func(context.Context, tool.Question) tool.Answer { return tool.Answer{} },
		DeferredTools:   []string{tool.BashName},
	})
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// compatibleExtra is what CompatiblePolicy adds to selfAllowed: the tools
// that allowed themselves before their item of permission.Policy existed.
// Nothing else may allow itself under it.
var compatibleExtra = map[string]string{
	tool.ShellOpenName:      "Policy.ShellSessions allow",
	tool.ShellSendName:      "Policy.ShellSessions allow",
	memory.RecordMemoryName: "Policy.MemoryWrites allow",
}

// Every tool a session can hold either is read-only, or does not allow itself
// (so the pipeline asks, and denies without an approval handler), or is listed
// with a reason: in selfAllowed under the strict policy and the defaults, and
// in selfAllowed plus compatibleExtra (exactly those) under CompatiblePolicy.
// A new tool that allows itself fails here until someone reviews it.
func TestEveryToolNeedsAPermissionDecision(t *testing.T) {
	for _, preset := range []struct {
		name   string
		policy permission.Policy
		extra  map[string]string
	}{
		{"default", permission.Policy{}, nil},
		{"strict", permission.StrictPolicy(), nil},
		{"compatible", permission.CompatiblePolicy(), compatibleExtra},
	} {
		t.Run(preset.name, func(t *testing.T) {
			checkSelfAllowing(t, preset.policy, preset.extra)
		})
	}
}

func checkSelfAllowing(t *testing.T, policy permission.Policy, extra map[string]string) {
	s := allToolsSession(t)
	pc := permission.Context{Mode: permission.ModeDefault, Policy: policy}
	exempt := func(name string) bool { return selfAllowed[name] != "" || extra[name] != "" }
	var names []string
	selfAllowing := map[string]bool{}
	for _, tl := range s.registry.List() {
		name := tl.Name()
		names = append(names, name)
		in := json.RawMessage(`{}`)
		if v, ok := sampleInput[name]; ok {
			in = json.RawMessage(v)
		}
		if tl.IsReadOnly(in) {
			continue
		}
		d := tl.CheckPermissions(context.Background(), in, pc)
		if d.Behavior == permission.Allow {
			selfAllowing[name] = true
			if !exempt(name) {
				t.Errorf("%s changes something and allows itself without asking; make it ask, or list it with a reason", name)
			}
		}
		final := permission.Evaluate(context.Background(), permission.Request{
			ToolName: name, Input: in, IsReadOnly: false, ToolDecision: d, Ctx: pc,
		})
		if final.Behavior == permission.Allow && !exempt(name) {
			t.Errorf("%s is allowed in default mode with no approval handler", name)
		}
	}
	// Exactly the documented exceptions: every listed tool exists, and every
	// extra the preset adds really allows itself under it.
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for n := range selfAllowed {
		if !have[n] {
			t.Errorf("selfAllowed lists %q, which the all-tools session does not hold", n)
		}
	}
	for n := range extra {
		if !selfAllowing[n] {
			t.Errorf("the preset lists %q as an exception, but it does not allow itself", n)
		}
	}
	sort.Strings(names)
	if len(names) < 25 {
		t.Fatalf("the all-tools session holds only %d tools: %v", len(names), names)
	}
}
