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
// (Result.Extra — e.g. the Skill tool's instructions message). It never panics;
// failures become error results. It is safe to call from the streaming
// executor's goroutines (read-only access to loop config; permission/hook
// callbacks are the host's responsibility to make safe).
func (l *loop) execOne(toolCtx context.Context, use llm.ContentBlock, emitProgress func(tool.ProgressInfo)) (llm.ContentBlock, []llm.Message) {
	t, ok := l.in.Tools.Get(use.Name)
	if !ok {
		return toolFailure(use, tool.UnknownToolText(l.in.Tools, use.Name), tool.CodeUnknownTool), nil
	}
	input := use.Input
	if msg := l.settleRefusal(use); msg != "" {
		return toolFailure(use, msg, tool.CodeToolUnavailable), nil
	}

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
			return toolFailure(use, "Error: invalid tool input: "+err.Error(), tool.CodeInvalidInput), nil
		}
	}

	mode := l.in.PermissionMode
	if l.in.PermissionModeFunc != nil {
		mode = l.in.PermissionModeFunc()
	}
	pc := permission.Context{
		Mode:       mode,
		WorkingDir: l.in.WorkingDir,
		Allowed:    l.in.Allowed,
		Disallowed: l.in.Disallowed,
	}
	dec := permission.Evaluate(l.ctx, permission.Request{
		ToolName:     use.Name,
		Input:        input,
		IsReadOnly:   t.IsReadOnly(input),
		ToolDecision: t.CheckPermissions(l.ctx, input, pc),
		Ctx:          pc,
		Ask:          l.in.CanUseTool,
	})
	if dec.Behavior != permission.Allow {
		msg := dec.Message
		if msg == "" {
			msg = "denied"
		}
		return toolFailure(use, "Tool call was not permitted: "+msg, tool.CodePermissionDenied), nil
	}
	if len(dec.UpdatedInput) > 0 {
		input = dec.UpdatedInput
	}

	// Pre-tool hook.
	if l.in.Hooks != nil {
		if block, msg, updated := l.in.Hooks.PreToolUse(l.ctx, use.Name, input); block {
			return toolFailure(use, "Blocked by hook: "+msg, tool.CodeHookBlocked), nil
		} else if len(updated) > 0 {
			input = updated
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
	}
	// toolCtx carries the run's MaxDuration deadline, so a tool that overruns the
	// wall-clock budget is interrupted here (the turn loop then enters wrap-up). During
	// the wrap-up phase toolCtx is the live parent ctx, so settlement tools run freely.
	callCtx := tool.WithInputCoercions(context.WithValue(toolCtx, parentKey{}, l.parentRun(mode)), coerced)
	var endSpan func(error, map[string]any)
	if l.in.Tracer != nil {
		callCtx, endSpan = l.in.Tracer.Start(callCtx, "tool.call", map[string]any{"tool": use.Name, "tool_use_id": use.ID})
	}
	res, err := callTool(callCtx, t, input, tc)
	if endSpan != nil {
		endSpan(err, map[string]any{"is_error": err != nil || res.IsError})
	}
	if err != nil {
		res = errorResult(err)
	}
	if l.in.RewriteToolResult != nil {
		rw := l.in.RewriteToolResult(l.ctx, use.ID, use.Name, input, res)
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
		l.in.OnToolResult(l.ctx, use.ID, use.Name, input, res)
	}
	if l.in.Hooks != nil {
		raw, _ := json.Marshal(res.Flatten())
		l.in.Hooks.PostToolUse(l.ctx, use.Name, input, raw, res.IsError)
	}

	content := res.Content
	if len(content) == 0 {
		content = []llm.ContentBlock{llm.TextBlock("(no output)")}
	}
	block := llm.ContentBlock{Type: llm.BlockToolResult, ToolUseID: use.ID, Content: content, IsError: res.IsError}
	if res.IsError && res.ErrorCode != "" {
		block.ToolError = &llm.ToolError{Name: use.Name, Code: res.ErrorCode}
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
