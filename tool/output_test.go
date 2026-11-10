package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
)

type point struct {
	X    int      `json:"x"`
	Y    int      `json:"y"`
	Tags []string `json:"tags,omitempty"`
}

var pointSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"x":    map[string]any{"type": "integer"},
		"y":    map[string]any{"type": "integer"},
		"tags": map[string]any{"type": "array"},
	},
	"required": []any{"x", "y"},
}

func pointOutput(render func(json.RawMessage, point) ([]llm.ContentBlock, error)) *Output {
	if render == nil {
		render = func(_ json.RawMessage, p point) ([]llm.ContentBlock, error) {
			return []llm.ContentBlock{llm.TextBlock(strings.Repeat("*", p.X) + "|" + strings.Join(p.Tags, ","))}, nil
		}
	}
	return DeclareOutput(OutputSpec[point]{
		Schema: pointSchema,
		Render: render,
		Meta: func(_ json.RawMessage, p point) map[string]any {
			return map[string]any{"title": "point"}
		},
	})
}

func valueTool(out *Output, run func() (Result, error)) CoreTool {
	return Build(Spec{
		Name:   "Value",
		Schema: map[string]any{"type": "object"},
		Run:    func(context.Context, json.RawMessage, *ToolContext) (Result, error) { return run() },
		Output: out,
	})
}

// Call validates the value, keeps a frozen copy and renders the content from
// that copy; the tool keeping (and mutating) its own value changes nothing.
func TestDeclaredOutputRendersFrozenValue(t *testing.T) {
	orig := point{X: 3, Y: 1, Tags: []string{"a", "b"}}
	res, err := valueTool(pointOutput(nil), func() (Result, error) { return Result{Data: orig}, nil }).
		Call(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Flatten() != "***|a,b" {
		t.Fatalf("content = %q", res.Flatten())
	}
	orig.Tags[0] = "MUTATED"
	frozen, ok := res.Data.(point)
	if !ok || frozen.Tags[0] != "a" {
		t.Fatalf("Data = %#v: not a frozen copy", res.Data)
	}
	if res.Meta["title"] != "point" {
		t.Fatalf("meta = %v", res.Meta)
	}
}

func TestDeclaredOutputRejectsInvalidValues(t *testing.T) {
	type other struct{ X int }
	cases := map[string]Result{
		"a map, not the declared type": {Data: map[string]any{"x": 1, "y": 1}},
		"another struct":               {Data: other{X: 1}},
		"content and value":            {Data: point{X: 1, Y: 1}, Content: []llm.ContentBlock{llm.TextBlock("both")}},
	}
	for name, ret := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := valueTool(pointOutput(nil), func() (Result, error) { return ret, nil }).
				Call(context.Background(), json.RawMessage(`{}`), nil)
			if !llm.IsCode(err, CodeInvalidOutput) {
				t.Fatalf("err = %v (code %q), want %q", err, llm.CodeOf(err), CodeInvalidOutput)
			}
		})
	}
}

// A value that type-checks but breaks the schema is rejected before rendering.
func TestDeclaredOutputSchemaIsEnforced(t *testing.T) {
	out := DeclareOutput(OutputSpec[map[string]any]{
		Schema: pointSchema,
		Render: func(json.RawMessage, map[string]any) ([]llm.ContentBlock, error) {
			t.Fatal("rendered an invalid value")
			return nil, nil
		},
	})
	_, err := out.Project(nil, map[string]any{"x": "not a number", "y": 1})
	if !llm.IsCode(err, CodeInvalidOutput) || !strings.Contains(err.Error(), "x") {
		t.Fatalf("err = %v", err)
	}
}

// A renderer that errors or panics fails the call with CodeRenderFailed; the
// cause stays reachable.
func TestRenderFailureIsContained(t *testing.T) {
	boom := errors.New("boom")
	for name, render := range map[string]func(json.RawMessage, point) ([]llm.ContentBlock, error){
		"error": func(json.RawMessage, point) ([]llm.ContentBlock, error) { return nil, boom },
		"panic": func(json.RawMessage, point) ([]llm.ContentBlock, error) { panic(boom) },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := valueTool(pointOutput(render), func() (Result, error) { return Result{Data: point{X: 1, Y: 1}}, nil }).
				Call(context.Background(), json.RawMessage(`{}`), nil)
			if !llm.IsCode(err, CodeRenderFailed) || !errors.Is(err, boom) {
				t.Fatalf("err = %v (code %q)", err, llm.CodeOf(err))
			}
		})
	}
}

// The shim: a tool that answers with content keeps working unchanged, with or
// without a declared output, and Data without a declaration stays opaque.
func TestContentResultsPassThrough(t *testing.T) {
	direct := Result{Content: []llm.ContentBlock{llm.TextBlock("as before")}, IsError: true}
	res, err := valueTool(pointOutput(nil), func() (Result, error) { return direct, nil }).
		Call(context.Background(), nil, nil)
	if err != nil || res.Flatten() != "as before" || !res.IsError {
		t.Fatalf("declared tool answering directly: %+v, %v", res, err)
	}

	type hostPayload struct{ anything chan int }
	payload := &hostPayload{}
	res, err = valueTool(nil, func() (Result, error) {
		return Result{Content: []llm.ContentBlock{llm.TextBlock("summary")}, Data: payload}, nil
	}).Call(context.Background(), nil, nil)
	if err != nil || res.Flatten() != "summary" || res.Data != payload {
		t.Fatalf("undeclared tool: %+v, %v", res, err)
	}
}

func TestApplyRewrite(t *testing.T) {
	out := pointOutput(nil)
	base, err := out.Apply(nil, Result{Data: point{X: 1, Y: 1}})
	if err != nil {
		t.Fatal(err)
	}

	// Presentation only: the value is kept.
	got, err := ApplyRewrite(out, nil, base, ReplaceContent([]llm.ContentBlock{llm.TextBlock("redacted")}))
	if err != nil || got.Flatten() != "redacted" || got.Data.(point).X != 1 {
		t.Fatalf("content replacement: %+v, %v", got, err)
	}

	// The value: revalidated and rendered again.
	got, err = ApplyRewrite(out, nil, base, ReplaceValue(point{X: 2, Y: 0}))
	if err != nil || got.Flatten() != "**|" || got.Data.(point).X != 2 {
		t.Fatalf("value replacement: %+v, %v", got, err)
	}

	// An invalid replacement value fails; so does replacing the value of a tool
	// that declares no output.
	if _, err := ApplyRewrite(out, nil, base, ReplaceValue("nope")); !llm.IsCode(err, CodeInvalidOutput) {
		t.Fatalf("invalid replacement: %v", err)
	}
	if _, err := ApplyRewrite(nil, nil, Text("x"), ReplaceValue(point{})); !llm.IsCode(err, CodeInvalidOutput) {
		t.Fatalf("replacement without a declaration: %v", err)
	}

	if got, err := ApplyRewrite(out, nil, base, KeepResult()); err != nil || got.Flatten() != base.Flatten() {
		t.Fatalf("keep: %+v, %v", got, err)
	}
}

func TestOutputOf(t *testing.T) {
	if OutputOf(NewRead()) == nil || OutputOf(NewBash()) == nil {
		t.Fatal("the migrated built-ins declare an output")
	}
	if OutputOf(NewWrite()) != nil {
		t.Fatal("Write declares no output")
	}
}
