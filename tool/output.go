package tool

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/0xmanhnv/norma/llm"
)

// Codes for a declared output that could not be turned into a tool_result.
const (
	CodeInvalidOutput llm.Code = "invalid_output" // the value is not what the output declares
	CodeRenderFailed  llm.Code = "render_failed"  // the renderer or the metadata projector failed
)

// OutputSpec declares a tool's typed output: the value Run returns, the schema
// it must satisfy, and how it is shown to the model.
//
// A tool with a declared output returns its value in Result.Data and leaves
// Content empty. Call then snapshots the value, validates it against Schema,
// keeps a frozen copy, and renders that copy into Content. The model reads only
// the rendered content and the transcript stores only that; the value stays in
// the call (Result.Data, seen by QueryInput.OnToolResult and by a direct
// caller of Call) and is never serialized into the conversation.
//
// Declaring an output does not force it on every answer: a Run that returns
// Content itself (an error, an edge case) is passed through as before.
type OutputSpec[T any] struct {
	// Schema is the JSON Schema of T as encoded by encoding/json, checked with
	// the same validator as tool inputs.
	Schema map[string]any
	// Render projects the value to the content the model reads. It must be
	// pure: the same args and value give the same blocks, with no side effects.
	// An empty result becomes "(no output)".
	Render func(args json.RawMessage, value T) ([]llm.ContentBlock, error)
	// Meta optionally projects presentation metadata for the host (a title, a
	// summary line, a badge). It is returned in Result.Meta and never reaches
	// the model or the transcript. Nil means none.
	Meta func(args json.RawMessage, value T) map[string]any
}

// Output is a declared output, built with DeclareOutput. It is safe for
// concurrent use.
type Output struct {
	schema  map[string]any
	project func(args json.RawMessage, value any) (Projection, error)
}

// Projection is a validated, frozen value with the content and metadata
// rendered from it.
type Projection struct {
	// Value is the frozen copy: a fresh T decoded from the snapshot, sharing no
	// memory with what the tool returned. Treat it as read-only; it is handed to
	// every observer of the call.
	Value   any
	Content []llm.ContentBlock
	Meta    map[string]any
}

// DeclareOutput builds an Output from its spec. T must round-trip through
// encoding/json: the snapshot is its JSON encoding, so what survives that
// encoding is exactly what the schema checks and the renderer sees.
func DeclareOutput[T any](s OutputSpec[T]) *Output {
	if s.Render == nil {
		panic("tool: DeclareOutput needs a Render function")
	}
	want := reflect.TypeFor[T]()
	return &Output{
		schema: s.Schema,
		project: func(args json.RawMessage, value any) (p Projection, err error) {
			v, ok := value.(T)
			if !ok {
				return p, llm.NewError(CodeInvalidOutput, fmt.Sprintf("output value is %T, want %s", value, want))
			}
			raw, err := json.Marshal(v)
			if err != nil {
				return p, llm.WrapError(CodeInvalidOutput, "output value cannot be encoded", err)
			}
			if err := ValidateInput(s.Schema, raw); err != nil {
				return p, llm.WrapError(CodeInvalidOutput, "output value does not match its schema", err)
			}
			var frozen T
			if err := json.Unmarshal(raw, &frozen); err != nil {
				return p, llm.WrapError(CodeInvalidOutput, "output value cannot be decoded", err)
			}
			defer func() {
				if r := recover(); r != nil {
					p, err = Projection{}, llm.WrapError(CodeRenderFailed, "rendering the output panicked", llm.Recovered(r))
				}
			}()
			content, err := s.Render(args, frozen)
			if err != nil {
				return Projection{}, llm.WrapError(CodeRenderFailed, "rendering the output failed", err)
			}
			if len(content) == 0 {
				content = []llm.ContentBlock{llm.TextBlock("(no output)")}
			}
			var meta map[string]any
			if s.Meta != nil {
				meta = s.Meta(args, frozen)
			}
			return Projection{Value: frozen, Content: content, Meta: meta}, nil
		},
	}
}

// Schema returns the declared output schema, e.g. for advertising it.
func (o *Output) Schema() map[string]any { return o.schema }

// Project snapshots value, validates the snapshot against the schema, freezes
// a copy and renders it. A failure carries CodeInvalidOutput or
// CodeRenderFailed.
func (o *Output) Project(args json.RawMessage, value any) (Projection, error) {
	return o.project(args, value)
}

// Apply turns a Result whose Data is the declared value into the Result the
// model and the transcript get: Content rendered from the frozen value, Data
// replaced by that frozen value, Meta set. A Result with no Data, or with
// Content of its own and no Data, is returned unchanged (the tool answered
// directly). Content and Data together are rejected: the content would hide
// what the value says.
//
// Build-made tools get this from Call. A CoreTool written by hand that
// declares an output (Output() *Output) calls it at the end of its own Call.
func (o *Output) Apply(args json.RawMessage, res Result) (Result, error) {
	if o == nil || res.Data == nil {
		return res, nil
	}
	if len(res.Content) > 0 {
		return Result{}, llm.NewError(CodeInvalidOutput, "a tool with a declared output returned both content and a value")
	}
	p, err := o.Project(args, res.Data)
	if err != nil {
		return Result{}, err
	}
	res.Content, res.Data, res.Meta = p.Content, p.Value, p.Meta
	return res, nil
}

// OutputOf returns the output t declares, or nil. Discovered by assertion, like
// AcceptsRawInput, so CoreTool implementations that predate it are unaffected.
func OutputOf(t CoreTool) *Output {
	if d, ok := t.(interface{ Output() *Output }); ok {
		return d.Output()
	}
	return nil
}

// Rewrite is what a post-execute policy decides about a tool result
// (harness.QueryInput.RewriteToolResult). The zero value keeps the result.
//
// The two replacements are different on purpose:
//
//   - ReplaceContent changes only what the model reads. The value is kept and
//     still reaches OnToolResult and any programmatic caller, so this is a
//     presentation change, not a confidentiality mechanism: a policy that must
//     keep data from a host or a caller blocks the call or replaces the value.
//   - ReplaceValue changes the value itself. The new value is validated against
//     the tool's declared output and rendered again, so the content and the
//     value cannot disagree. It needs a declared output; on a tool without one,
//     or when the new value fails, the call fails closed with an error result.
type Rewrite struct {
	kind    rewriteKind
	content []llm.ContentBlock
	value   any
}

type rewriteKind uint8

const (
	rewriteKeep rewriteKind = iota
	rewriteContent
	rewriteValue
)

// KeepResult leaves the result as it is.
func KeepResult() Rewrite { return Rewrite{} }

// ReplaceContent replaces what the model reads and keeps the value.
func ReplaceContent(blocks []llm.ContentBlock) Rewrite {
	return Rewrite{kind: rewriteContent, content: blocks}
}

// ReplaceValue replaces the value; content and metadata are recomputed from it.
func ReplaceValue(v any) Rewrite { return Rewrite{kind: rewriteValue, value: v} }

// ApplyRewrite applies rw to res, the result of a call to a tool declaring out
// (nil when it declares none). A value replacement that cannot be honoured
// returns an error; the caller must not fall back to the original result.
func ApplyRewrite(out *Output, args json.RawMessage, res Result, rw Rewrite) (Result, error) {
	switch rw.kind {
	case rewriteContent:
		res.Content = rw.content
		return res, nil
	case rewriteValue:
		if out == nil {
			return Result{}, llm.NewError(CodeInvalidOutput, "the tool declares no output, so its value cannot be replaced")
		}
		p, err := out.Project(args, rw.value)
		if err != nil {
			return Result{}, err
		}
		res.Content, res.Data, res.Meta = p.Content, p.Value, p.Meta
		return res, nil
	}
	return res, nil
}
