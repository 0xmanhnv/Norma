package tool

import (
	"strings"
	"testing"
)

func TestValidateInputEnum(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode":  map[string]any{"type": "string", "enum": []any{"fast", "safe"}},
			"typed": map[string]any{"type": "string", "enum": []string{"a", "b"}},
			"level": map[string]any{"type": "integer", "enum": []any{1, 2, 3}},
			"any":   map[string]any{"enum": []any{"on", false, nil, 2.5}},
			"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []any{"x", "y"}}},
			"nested": map[string]any{"type": "object", "properties": map[string]any{
				"kind": map[string]any{"type": "string", "enum": []any{"k1"}},
			}},
			"odd": map[string]any{"type": "string", "enum": "not a list"},
		},
	}
	cases := []struct {
		name    string
		input   string
		wantErr string // substring; "" = valid
	}{
		{"listed string", `{"mode":"fast"}`, ""},
		{"unlisted string", `{"mode":"reckless"}`, `mode: must be one of "fast", "safe", got "reckless"`},
		{"case matters", `{"mode":"Fast"}`, `mode: must be one of`},
		{"enum written as a typed Go slice", `{"typed":"c"}`, `typed: must be one of "a", "b", got "c"`},
		{"listed integer", `{"level":2}`, ""},
		{"unlisted integer", `{"level":4}`, `level: must be one of 1, 2, 3, got 4`},
		{"wrong type reports the type first", `{"mode":["fast"]}`, `mode: expected string`},
		{"untyped enum, listed values", `{"any":false}`, ""},
		{"untyped enum, null listed", `{"any":null}`, ""},
		{"untyped enum, unlisted value", `{"any":"off"}`, `any: must be one of`},
		{"array items, all listed", `{"tags":["x","y","x"]}`, ""},
		{"array items, one unlisted", `{"tags":["x","z"]}`, `tags[1]: must be one of "x", "y", got "z"`},
		{"nested property", `{"nested":{"kind":"k2"}}`, `nested.kind: must be one of "k1", got "k2"`},
		{"absent optional property", `{}`, ""},
		{"an enum that is not a list is ignored", `{"odd":"whatever"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateInput(schema, []byte(c.input))
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.wantErr != "" && err == nil:
				t.Fatalf("accepted %s, want an error containing %q", c.input, c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Fatalf("error %q does not contain %q", err, c.wantErr)
			}
		})
	}
}
