package harness

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// sideEffectTool is a non-read-only tool that needs approval (the tool.Build
// default) and records whether it ran.
func sideEffectTool(name string, ran *atomic.Bool) tool.CoreTool {
	return tool.Build(tool.Spec{
		Name: name,
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
		},
		Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			ran.Store(true)
			return tool.Text("ran"), nil
		},
	})
}

// hooksFunc is a HookRunner built from a PreToolUse function.
type hooksFunc struct {
	pre  func(name string, input []byte) (bool, string, []byte)
	post func(name string)
}

func (h hooksFunc) PreToolUse(_ context.Context, name string, input []byte) (bool, string, []byte) {
	if h.pre == nil {
		return false, "", nil
	}
	return h.pre(name, input)
}
func (h hooksFunc) PostToolUse(_ context.Context, name string, _, _ []byte, _ bool) {
	if h.post != nil {
		h.post(name)
	}
}
func (hooksFunc) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return false, nil, ""
}

// wrappedTool is how a host decorates tools (panic guards, output shaping):
// it embeds the tool and overrides Call. Nothing but CoreTool survives it.
type wrappedTool struct{ tool.CoreTool }

func (w wrappedTool) Call(ctx context.Context, in json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
	return w.CoreTool.Call(ctx, in, tc)
}

// A deferred tool called through ExecuteExtraTool goes through the same
// permission pipeline as a direct call, under its own name, whatever the
// policy: a deny rule on it holds and the approval callback is asked about
// it. Hooks see it under its own name with DeferredHooks "own_name".
func TestDeferredToolGoesThroughPermissionsAndHooks(t *testing.T) {
	deferredCall := `{"tool_name":"danger","params":{"command":"x"}}`
	newTools := func(ran *atomic.Bool, wrap bool) *tool.Registry {
		reg := tool.NewRegistry(sideEffectTool("danger", ran))
		exec := tool.NewExecuteExtraToolFor(reg, nil, []string{"danger"})
		if wrap {
			exec = wrappedTool{exec}
		}
		reg.Add(exec)
		return reg
	}

	for _, wrap := range []bool{false, true} {
		for _, pol := range []permission.Policy{{}, permission.StrictPolicy()} {
			var ran atomic.Bool
			in := QueryInput{Tools: newTools(&ran, wrap), PermissionMode: permission.ModeBypass, Disallowed: []string{"danger"}, Policy: pol}
			b, _, _ := runOneCall(t, in, tool.ExecuteExtraToolName, deferredCall)
			if ran.Load() {
				t.Fatalf("wrapped=%v policy=%+v: deferred tool ran despite a deny rule on its name", wrap, pol)
			}
			wantToolError(t, "deny rule", b, tool.ExecuteExtraToolName, tool.CodePermissionDenied)

			ran.Store(false)
			var asked []string
			in = QueryInput{
				Tools:          newTools(&ran, wrap),
				PermissionMode: permission.ModeDefault,
				Policy:         pol,
				CanUseTool: func(_ context.Context, name string, _ json.RawMessage, _ permission.Context) permission.Decision {
					asked = append(asked, name)
					return permission.Denied("no")
				},
			}
			runOneCall(t, in, tool.ExecuteExtraToolName, deferredCall)
			if ran.Load() || len(asked) != 1 || asked[0] != "danger" {
				t.Fatalf("wrapped=%v: ran=%v, approval callback asked about %v, want [danger]", wrap, ran.Load(), asked)
			}

			ran.Store(false)
			in = QueryInput{Tools: newTools(&ran, wrap), PermissionMode: permission.ModeDefault, Policy: pol}
			runOneCall(t, in, tool.ExecuteExtraToolName, deferredCall)
			if ran.Load() {
				t.Fatalf("wrapped=%v: deferred tool ran in default mode with no approval handler", wrap)
			}

			ran.Store(false)
			in = QueryInput{Tools: newTools(&ran, wrap), PermissionMode: permission.ModeDefault, Allowed: []string{"danger"}, Policy: pol}
			if b, _, _ := runOneCall(t, in, tool.ExecuteExtraToolName, deferredCall); !ran.Load() || b.IsError {
				t.Fatalf("wrapped=%v: allowed deferred call did not run: %+v", wrap, b)
			}
		}
	}

	// DeferredPermissions "wrapper" is the behaviour before: the deferred tool
	// runs after its schema check, and a deny rule on its name does not reach
	// it (only ExecuteExtraTool's own check applies).
	var ran atomic.Bool
	in := QueryInput{Tools: newTools(&ran, true), PermissionMode: permission.ModeBypass, Disallowed: []string{"danger"},
		Policy: permission.Policy{DeferredPermissions: permission.DeferredPermissionsWrapper}}
	runOneCall(t, in, tool.ExecuteExtraToolName, deferredCall)
	if !ran.Load() {
		t.Fatal(`DeferredPermissions "wrapper" must run the deferred tool as before`)
	}

	hookRun := func(pol permission.Policy) (seen, posted []string, ran bool) {
		var r atomic.Bool
		in := QueryInput{
			Tools:          newTools(&r, true),
			PermissionMode: permission.ModeBypass,
			Policy:         pol,
			Hooks: hooksFunc{
				pre: func(name string, _ []byte) (bool, string, []byte) {
					seen = append(seen, name)
					return name == "danger", "not here", nil
				},
				post: func(name string) { posted = append(posted, name) },
			},
		}
		runOneCall(t, in, tool.ExecuteExtraToolName, deferredCall)
		return seen, posted, r.Load()
	}
	// Default ("wrapper", as before): hooks see ExecuteExtraTool only.
	if seen, posted, ran := hookRun(permission.Policy{}); !ran || len(seen) != 1 || seen[0] != tool.ExecuteExtraToolName || len(posted) != 1 {
		t.Fatalf("default: PreToolUse saw %v, PostToolUse %v, ran=%v", seen, posted, ran)
	}
	// "own_name": hooks also see the deferred tool, and can block it.
	seen, posted, hran := hookRun(permission.Policy{DeferredHooks: permission.DeferredHooksOwnName})
	if hran || len(seen) != 2 || seen[1] != "danger" || len(posted) != 1 {
		t.Fatalf("own_name: PreToolUse saw %v, PostToolUse %v, ran=%v", seen, posted, hran)
	}
}

// Input a PreToolUse hook rewrites is checked again: a rewrite cannot turn an
// allowed call into one a deny rule forbids.
func TestHookRewrittenInputIsReevaluated(t *testing.T) {
	var ran atomic.Bool
	in := QueryInput{
		Tools:          tool.NewRegistry(sideEffectTool("run", &ran)),
		PermissionMode: permission.ModeDefault,
		Allowed:        []string{"run(ls)"},
		Disallowed:     []string{"run(rm:*)"},
		Hooks: hooksFunc{pre: func(string, []byte) (bool, string, []byte) {
			return false, "", []byte(`{"command":"rm -rf /tmp/x"}`)
		}},
	}
	b, _, _ := runOneCall(t, in, "run", `{"command":"ls"}`)
	if ran.Load() {
		t.Fatal("the tool ran with hook-rewritten input that a deny rule forbids")
	}
	wantToolError(t, "rewrite", b, "run", tool.CodePermissionDenied)

	// A rewrite that breaks the schema is refused too.
	ran.Store(false)
	in.Hooks = hooksFunc{pre: func(string, []byte) (bool, string, []byte) {
		return false, "", []byte(`{"command":42}`)
	}}
	b, _, _ = runOneCall(t, in, "run", `{"command":"ls"}`)
	if ran.Load() {
		t.Fatal("the tool ran with hook-rewritten input that fails its schema")
	}
	wantToolError(t, "rewrite schema", b, "run", tool.CodeInvalidInput)
}

// A panic in a permission check or a hook fails that call, not the process.
func TestPanicOutsideToolCallFailsTheCall(t *testing.T) {
	var ran atomic.Bool
	in := QueryInput{
		Tools:          tool.NewRegistry(sideEffectTool("run", &ran)),
		PermissionMode: permission.ModeBypass,
		Hooks: hooksFunc{pre: func(string, []byte) (bool, string, []byte) {
			panic("hook bug")
		}},
	}
	b, _, _ := runOneCall(t, in, "run", `{"command":"ls"}`)
	if ran.Load() {
		t.Fatal("the tool ran although its pre-hook panicked")
	}
	if !b.IsError || !strings.Contains(b.Content[0].Text, "hook bug") {
		t.Fatalf("result = %+v", b)
	}
}

// A call that reaches execOne after the run's context ended does not start.
func TestCallAfterCancelDoesNotStart(t *testing.T) {
	var ran atomic.Bool
	l := &loop{ctx: context.Background(), in: QueryInput{
		Tools:          tool.NewRegistry(sideEffectTool("run", &ran)),
		PermissionMode: permission.ModeBypass,
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b, _ := l.execOne(ctx, llm.ContentBlock{Type: llm.BlockToolUse, ID: "c1", Name: "run", Input: json.RawMessage(`{}`)}, nil)
	if ran.Load() {
		t.Fatal("a call started after its context was cancelled")
	}
	if b.ToolError == nil || b.ToolError.Code != tool.CodeInterrupted {
		t.Fatalf("result = %+v", b)
	}
}

// With the compatible values the three call-level checks behave as before:
// hook-rewritten input is used as it is, a call after cancel starts, and a
// panic outside the tool itself is not contained.
func TestCallChecksCompatibleValues(t *testing.T) {
	var ran atomic.Bool
	in := QueryInput{
		Tools:          tool.NewRegistry(sideEffectTool("run", &ran)),
		PermissionMode: permission.ModeDefault,
		Allowed:        []string{"run(ls)"},
		Disallowed:     []string{"run(rm:*)"},
		Policy:         permission.Policy{RewrittenInput: permission.RewrittenInputTrust},
		Hooks: hooksFunc{pre: func(string, []byte) (bool, string, []byte) {
			return false, "", []byte(`{"command":"rm -rf /tmp/x"}`)
		}},
	}
	runOneCall(t, in, "run", `{"command":"ls"}`)
	if !ran.Load() {
		t.Fatal(`RewrittenInput "trust" must use the rewritten input without a new decision`)
	}

	ran.Store(false)
	l := &loop{ctx: context.Background(), in: QueryInput{
		Tools:          tool.NewRegistry(sideEffectTool("run", &ran)),
		PermissionMode: permission.ModeBypass,
		Policy:         permission.Policy{CallsAfterCancel: permission.CallsAfterCancelStart},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l.execOne(ctx, llm.ContentBlock{Type: llm.BlockToolUse, ID: "c1", Name: "run", Input: json.RawMessage(`{}`)}, nil)
	if !ran.Load() {
		t.Fatal(`CallsAfterCancel "start" must start the call as before`)
	}

	l = &loop{ctx: context.Background(), in: QueryInput{
		Tools:          tool.NewRegistry(sideEffectTool("run", &ran)),
		PermissionMode: permission.ModeBypass,
		Policy:         permission.Policy{CallPanics: permission.CallPanicsToolOnly},
		Hooks: hooksFunc{pre: func(string, []byte) (bool, string, []byte) {
			panic("hook bug")
		}},
	}}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal(`CallPanics "tool_only" must not contain a panic in a hook`)
			}
		}()
		l.execOne(context.Background(), llm.ContentBlock{Type: llm.BlockToolUse, ID: "c1", Name: "run", Input: json.RawMessage(`{}`)}, nil)
	}()
}
