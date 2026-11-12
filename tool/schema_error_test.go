package tool

import (
	"errors"
	"strings"
	"testing"
)

// A validation failure says where and which rule without the value: the
// fields of SchemaError never hold what was sent, while Error() still
// describes it for the model.
func TestSchemaErrorFields(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []any{"title"},
		"properties": map[string]any{
			"title": map[string]any{"type": "string"},
			"steps": map[string]any{"type": "array", "items": map[string]any{
				"type":       "object",
				"properties": map[string]any{"state": map[string]any{"enum": []any{"open", "done"}}},
			}},
		},
	}
	cases := []struct {
		input, path, keyword, want, secret string
	}{
		{`{"steps":[]}`, "", "required", "title", ""},
		{`{"title":42}`, "title", "type", "string", "42"},
		{`{"title":"x","steps":[{"state":"secret-value"}]}`, "steps[0].state", "enum", `"open", "done"`, "secret-value"},
	}
	for _, c := range cases {
		err := ValidateInput(schema, []byte(c.input))
		var se *SchemaError
		if !errors.As(err, &se) {
			t.Fatalf("%s: err %v is not a *SchemaError", c.input, err)
		}
		if se.Path != c.path || se.Keyword != c.keyword || se.Want != c.want {
			t.Fatalf("%s: got %+v", c.input, *se)
		}
		if c.secret != "" && !strings.Contains(se.Error(), c.secret) {
			t.Fatalf("%s: Error() %q no longer describes the value", c.input, se.Error())
		}
	}
}
