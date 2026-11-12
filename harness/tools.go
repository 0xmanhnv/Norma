package harness

import (
	"context"
	"encoding/json"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// rawInputTool is the optional opt-out from schema validation. Discovered by
// assertion rather than declared on tool.CoreTool, so hosts implementing that
// interface are unaffected.
type rawInputTool interface{ AcceptsRawInput() bool }

func acceptsRawInput(t tool.CoreTool) bool {
	r, ok := t.(rawInputTool)
	return ok && r.AcceptsRawInput()
}

// execOne resolves, permission-checks, and runs a single tool call, returning a
// tool_result block and any extra messages the tool injects after its result
// (Result.Extra — e.g. the Skill tool's instructions message). It never panics:
// a panic anywhere in the call (permission check, hook, rewriter, tool) becomes
// that call's error result. It is safe to call from the streaming executor's
// goroutines (read-only access to loop config; permission/hook callbacks are the
// host's responsibility to make safe).
//
// Every tool runs behind a permission decision. The deferred tool that
// ExecuteExtraTool hands over (ToolContext.RunDeferred) goes through the same
// checks under its own name (runDeferred); it is never run unchecked.
func (l *loop) execOne(toolCtx context.Context, use llm.ContentBlock, emitProgress func(tool.ProgressInfo)) (block llm.ContentBlock, extra []llm.Message) {
	pol := l.in.Policy.Resolved()
	defer func() {
		if pol.CallPanics != permission.CallPanicsContain {
			return // "tool_only": only callTool recovers, as before
		}
		if v := recover(); v != nil {
			block, extra = toolFailure(use, "Error: "+llm.Recovered(v).Error(), llm.CodeUnknown), nil
		}
	}()
	// A call queued behind others may reach here after the run was cancelled or
	// its time budget ran out: it must not start then.
	if err := toolCtx.Err(); err != nil && pol.CallsAfterCancel == permission.CallsAfterCancelRefuse {
		return toolFailure(use, "Error: tool call not started: "+err.Error(), tool.CodeInterrupted), nil
	}
	name := use.Name
	t, ok := l.in.Tools.Get(name)
	if !ok {
		return toolFailure(use, tool.UnknownToolText(l.in.Tools, name), tool.CodeUnknownTool), nil
	}
	input := use.Input
	if msg := l.settleRefusal(use); msg != "" {
		return toolFailure(use, msg, tool.CodeToolUnavailable), nil
	}
	fail := func(text string, code llm.Code) llm.ContentBlock { return toolFailure(use, text, code) }

	// Schema validation (FR-04.5).
	//
	// A tool may opt out by reporting AcceptsRawInput: it parses its own
	// arguments and would rather repair a malformed call than have it rejected
	// here, where the only possible answer is a generic error. The schema is
	// still advertised to the model either way — see tool.Spec.RawInput.
	// Before the check, the common mistypings are repaired by the schema (a
	// number or a JSON array sent as a string, see tool.CoerceInput); the
	// repairs travel on the call's context so the host can count them.
	var coerced []string
	if !acceptsRawInput(t) {
		input, coerced = tool.CoerceInput(t.InputSchema(), input)
		if err := tool.ValidateInput(t.InputSchema(), input); err != nil {
			return fail("Error: invalid tool input: "+err.Error(), tool.CodeInvalidInput), nil
		}
	}

	mode := l.in.PermissionMode
	if l.in.PermissionModeFunc != nil {
		mode = l.in.PermissionModeFunc()
	}
	pc := l.permissionContext(mode)
	decide := func(input json.RawMessage) permission.Decision {
		return permission.Evaluate(l.ctx, permission.Request{
			ToolName:     name,
			Input:        input,
			IsReadOnly:   t.IsReadOnly(input),
			ToolDecision: t.CheckPermissions(l.ctx, input, pc),
			Ctx:          pc,
			Ask:          l.in.CanUseTool,
		})
	}
	denied := func(dec permission.Decision) llm.ContentBlock {
		msg := dec.Message
		if msg == "" {
			msg = "denied"
		}
		return fail("Tool call was not permitted: "+msg, tool.CodePermissionDenied)
	}
	dec := decide(input)
	if dec.Behavior != permission.Allow {
		return denied(dec), nil
	}
	if len(dec.UpdatedInput) > 0 {
		input = dec.UpdatedInput
	}

	// Pre-tool hook. Input a hook rewrites is checked again, schema and
	// permission both: the decision above was made for the input as it was.
	if l.in.Hooks != nil {
		if blocked, msg, updated := l.in.Hooks.PreToolUse(l.ctx, name, input); blocked {
			return fail("Blocked by hook: "+msg, tool.CodeHookBlocked), nil
		} else if len(updated) > 0 {
			input = updated
			if pol.RewrittenInput == permission.RewrittenInputRecheck {
				if !acceptsRawInput(t) {
					if err := tool.ValidateInput(t.InputSchema(), input); err != nil {
						return fail("Error: input rewritten by a hook is invalid: "+err.Error(), tool.CodeInvalidInput), nil
					}
				}
				if dec := decide(input); dec.Behavior != permission.Allow {
					return denied(dec), nil
				} else if len(dec.UpdatedInput) > 0 {
					input = dec.UpdatedInput
				}
			}
		}
	}

	tc := &tool.ToolContext{
		WorkingDir:     l.in.WorkingDir,
		AgentID:        l.in.AgentID,
		ToolUseID:      use.ID,
		Emit:           emitProgress,
		OutputDir:      l.in.ToolOutputDir,
		Spill:          l.in.SpillStore,
		MaxOutputChars: l.in.MaxToolOutputChars,
		TailOnly:       l.in.TaskOutputTailOnly,
		Tasks:          l.in.Tasks,
		Env:            l.in.BashEnv,

		ChildEnvAllowlist: pol.ChildEnv == permission.ChildEnvAllowlist,
	}
	tc.RunDeferred = func(ctx context.Context, inner tool.CoreTool, innerInput json.RawMessage) (tool.Result, error) {
		return l.runDeferred(ctx, pc, inner, innerInput, tc)
	}
	// toolCtx carries the run's MaxDuration deadline, so a tool that overruns the
	// wall-clock budget is interrupted here (the turn loop then enters wrap-up). During
	// the wrap-up phase toolCtx is the live parent ctx, so settlement tools run freely.
	callCtx := tool.WithInputCoercions(context.WithValue(toolCtx, parentKey{}, l.parentRun(mode)), coerced)
	var endSpan func(error, map[string]any)
	if l.in.Tracer != nil {
		callCtx, endSpan = l.in.Tracer.Start(callCtx, "tool.call", map[string]any{"tool": name, "tool_use_id": use.ID})
	}
	res, err := callTool(callCtx, t, input, tc)
	if endSpan != nil {
		endSpan(err, map[string]any{"is_error": err != nil || res.IsError})
	}
	if err != nil {
		res = errorResult(err)
	}
	if l.in.RewriteToolResult != nil {
		rw := l.in.RewriteToolResult(l.ctx, use.ID, name, input, res)
		if res, err = tool.ApplyRewrite(tool.OutputOf(t), input, res, rw); err != nil {
			res = errorResult(err) // fail closed: never fall back to the original
		}
	}
	// Global output cap at the single post-tool choke point: EVERY tool —
	// built-in, MCP, custom, host — is bounded here regardless of whether it
	// remembered to Capture itself, so no tool can overflow the context window
	// (the failure mode that motivated this net). CaptureOnce is idempotent:
	// output a tool already spilled/truncated carries a marker and is left
	// untouched, so this never double-truncates or double-spills.
	res = capOutput(tc, res)
	if r, ok := res.Data.(SubagentUsageReporter); ok {
		l.addSubagentUsage(r.SubagentUsage())
	}

	if l.in.OnToolResult != nil {
		l.in.OnToolResult(l.ctx, use.ID, name, input, res)
	}
	if l.in.Hooks != nil {
		raw, _ := json.Marshal(res.Flatten())
		l.in.Hooks.PostToolUse(l.ctx, name, input, raw, res.IsError)
	}

	content := res.Content
	if len(content) == 0 {
		content = []llm.ContentBlock{llm.TextBlock("(no output)")}
	}
	block = llm.ContentBlock{Type: llm.BlockToolResult, ToolUseID: use.ID, Content: content, IsError: res.IsError}
	if res.IsError && res.ErrorCode != "" {
		block.ToolError = &llm.ToolError{Name: name, Code: res.ErrorCode}
	}
	return block, res.Extra
}

// callTool runs the tool, turning a panic into an error so that one broken
// tool fails its own call instead of the process. The error carries
// llm.CodeUnknown with the panic value as its cause (llm.Recovered).
func callTool(ctx context.Context, t tool.CoreTool, input json.RawMessage, tc *tool.ToolContext) (res tool.Result, err error) {
	defer func() {
		if v := recover(); v != nil {
			res, err = tool.Result{}, llm.Recovered(v)
		}
	}()
	return t.Call(ctx, input, tc)
}

// errorResult is the result for a call that ended in err: the text the model
// has always read, classified with the error's code.
func errorResult(err error) tool.Result {
	res := tool.Errorf("Error: " + err.Error())
	res.ErrorCode = llm.CodeOf(err)
	return res
}

// toolFailure is the error tool_result for a call the harness refused or cut
// short, classified with code. The text is what the model reads.
func toolFailure(use llm.ContentBlock, text string, code llm.Code) llm.ContentBlock {
	b := llm.ToolResultText(use.ID, text, true)
	b.ToolError = &llm.ToolError{Name: use.Name, Code: code}
	return b
}

// capOutput runs every text block of a tool result through the idempotent
// output cap (tool.CaptureOnce). Non-text blocks pass through untouched. It is
// the one place that guarantees no tool's output — however constructed — can
// blow past the context budget.
func capOutput(tc *tool.ToolContext, res tool.Result) tool.Result {
	for i, b := range res.Content {
		if b.Type == llm.BlockText {
			res.Content[i].Text = tool.CaptureOnce(tc, b.Text)
		}
	}
	return res
}

// permissionContext is what tools and the pipeline are shown for a call.
func (l *loop) permissionContext(mode permission.Mode) permission.Context {
	return permission.Context{
		Mode:       mode,
		WorkingDir: l.in.WorkingDir,
		Allowed:    l.in.Allowed,
		Disallowed: l.in.Disallowed,
		Policy:     l.in.Policy.Resolved(),
	}
}

// runDeferred runs the deferred tool ExecuteExtraTool resolved, through the
// checks a direct call gets, under the deferred tool's own name: schema
// validation, the permission pipeline (deny rules, the tool's own check, the
// mode, the approval callback) and, when Policy.DeferredHooks is "own_name",
// the PreToolUse and PostToolUse hooks. A refusal is the call's error result.
// The ExecuteExtraTool call around it has already been through the pipeline
// and its hooks under its own name.
func (l *loop) runDeferred(ctx context.Context, pc permission.Context, t tool.CoreTool, input json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
	name := t.Name()
	refuse := func(text string, code llm.Code) (tool.Result, error) {
		r := tool.Errorf(text)
		r.ErrorCode = code
		return r, nil
	}
	pol := pc.Policy
	recheck := pol.RewrittenInput == permission.RewrittenInputRecheck
	if err := ctx.Err(); err != nil && pol.CallsAfterCancel == permission.CallsAfterCancelRefuse {
		return refuse("Error: tool call not started: "+err.Error(), tool.CodeInterrupted)
	}
	validate := func(in json.RawMessage) error {
		if acceptsRawInput(t) {
			return nil
		}
		return tool.ValidateInput(t.InputSchema(), in)
	}
	if pol.DeferredPermissions == permission.DeferredPermissionsWrapper {
		// As before: a schema check, then the tool, with no permission check
		// of its own (only ExecuteExtraTool's).
		if err := tool.ValidateInput(t.InputSchema(), input); err != nil {
			return refuse("Error: invalid params for "+name+": "+err.Error(), tool.CodeInvalidInput)
		}
		return l.callDeferred(ctx, pol, t, name, input, tc, nil)
	}
	if !acceptsRawInput(t) {
		input, _ = tool.CoerceInput(t.InputSchema(), input)
	}
	if err := validate(input); err != nil {
		return refuse("Error: invalid input for "+name+": "+err.Error(), tool.CodeInvalidInput)
	}
	decide := func(in json.RawMessage) permission.Decision {
		return permission.Evaluate(l.ctx, permission.Request{
			ToolName:     name,
			Input:        in,
			IsReadOnly:   t.IsReadOnly(in),
			ToolDecision: t.CheckPermissions(l.ctx, in, pc),
			Ctx:          pc,
			Ask:          l.in.CanUseTool,
		})
	}
	denied := func(dec permission.Decision) (tool.Result, error) {
		msg := dec.Message
		if msg == "" {
			msg = "denied"
		}
		return refuse("Tool call was not permitted: "+msg, tool.CodePermissionDenied)
	}
	dec := decide(input)
	if dec.Behavior != permission.Allow {
		return denied(dec)
	}
	if len(dec.UpdatedInput) > 0 {
		input = dec.UpdatedInput
		if err := validate(input); err != nil && recheck {
			return refuse("Error: rewritten input for "+name+" is invalid: "+err.Error(), tool.CodeInvalidInput)
		}
	}
	return l.callDeferred(ctx, pol, t, name, input, tc, &deferredChecks{decide, denied, validate})
}

// deferredChecks re-run the checks of runDeferred on input a hook rewrote.
type deferredChecks struct {
	decide   func(json.RawMessage) permission.Decision
	denied   func(permission.Decision) (tool.Result, error)
	validate func(json.RawMessage) error
}

// callDeferred runs the PreToolUse hook under the deferred tool's name (when
// Policy.DeferredHooks is "own_name"), the tool, and the PostToolUse hook.
func (l *loop) callDeferred(ctx context.Context, pol permission.Policy, t tool.CoreTool, name string, input json.RawMessage, tc *tool.ToolContext, ck *deferredChecks) (tool.Result, error) {
	refuse := func(text string, code llm.Code) (tool.Result, error) {
		r := tool.Errorf(text)
		r.ErrorCode = code
		return r, nil
	}
	recheck := pol.RewrittenInput == permission.RewrittenInputRecheck && ck != nil
	hooks := l.in.Hooks != nil && pol.DeferredHooks == permission.DeferredHooksOwnName
	if hooks {
		if blocked, msg, updated := l.in.Hooks.PreToolUse(l.ctx, name, input); blocked {
			return refuse("Blocked by hook: "+msg, tool.CodeHookBlocked)
		} else if len(updated) > 0 {
			input = updated
			if recheck {
				if err := ck.validate(input); err != nil {
					return refuse("Error: input rewritten by a hook is invalid: "+err.Error(), tool.CodeInvalidInput)
				}
				if dec := ck.decide(input); dec.Behavior != permission.Allow {
					return ck.denied(dec)
				} else if len(dec.UpdatedInput) > 0 {
					input = dec.UpdatedInput
				}
			}
		}
	}
	inner := *tc
	inner.RunDeferred = nil // a deferred tool cannot hand over another
	res, err := callTool(ctx, t, input, &inner)
	if err != nil {
		res = errorResult(err)
	}
	if hooks {
		raw, _ := json.Marshal(res.Flatten())
		l.in.Hooks.PostToolUse(l.ctx, name, input, raw, res.IsError)
	}
	return res, nil
}
