package harness

import (
	"context"
	"iter"

	"github.com/0xmanhnv/corvid/llm"
)

// Tracer receives spans for model calls and tool calls. It is a minimal,
// dependency-free seam: a host adapts it to OpenTelemetry or any other backend.
// Start opens a span and returns the context to run under plus an end function
// that must be called exactly once with the outcome and any final attributes.
//
// Span names: "model.call" (attrs: messages, tools, streaming; end: input_tokens,
// output_tokens, cache_read_tokens, cache_write_tokens, stop_reason) and
// "tool.call" (attrs: tool, tool_use_id; end: is_error).
type Tracer interface {
	Start(ctx context.Context, name string, attrs map[string]any) (context.Context, func(err error, attrs map[string]any))
}

// traced wraps the model dependencies so every call is a span.
func (d *QueryDeps) traced(tr Tracer) {
	if tr == nil {
		return
	}
	if inner := d.CallModel; inner != nil {
		d.CallModel = func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
			return func(yield func(llm.StreamEvent, error) bool) {
				ctx, end := tr.Start(ctx, "model.call", map[string]any{
					"messages": len(req.Messages), "tools": len(req.Tools), "streaming": true})
				acc := llm.NewAccumulator()
				var callErr error
				defer func() { end(callErr, usageAttrs(acc.Usage, acc.StopReason)) }()
				for ev, err := range inner(ctx, req) {
					if err != nil {
						callErr = err
					} else {
						acc.Add(ev)
					}
					if !yield(ev, err) {
						return
					}
				}
			}
		}
	}
	if inner := d.CallModelSync; inner != nil {
		d.CallModelSync = func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
			ctx, end := tr.Start(ctx, "model.call", map[string]any{
				"messages": len(req.Messages), "tools": len(req.Tools), "streaming": false})
			msg, stop, usage, err := inner(ctx, req)
			end(err, usageAttrs(usage, stop))
			return msg, stop, usage, err
		}
	}
}

func usageAttrs(u llm.Usage, stop string) map[string]any {
	return map[string]any{"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens,
		"cache_read_tokens": u.CacheReadTokens, "cache_write_tokens": u.CacheWriteTokens, "stop_reason": stop}
}
