package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

var coerceSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
		"ratio":   map[string]any{"type": "number"},
		"verbose": map[string]any{"type": "boolean"},
		"q":       map[string]any{"type": "string"},
		"filter":  map[string]any{"type": "object", "properties": map[string]any{"top": map[string]any{"type": "integer"}}},
		"tags":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"intents": map[string]any{"type": "array", "items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"summary":  map[string]any{"type": "string"},
				"priority": map[string]any{"type": "integer"},
				"steps":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
		}},
	},
}

func TestCoerceInput(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"integer from string", `{"limit":"20"}`, `{"limit":20}`},
		{"integer with spaces", `{"limit":" 7 "}`, `{"limit":7}`},
		{"number from string", `{"ratio":"0.25"}`, `{"ratio":0.25}`},
		{"boolean true", `{"verbose":"true"}`, `{"verbose":true}`},
		{"boolean False", `{"verbose":"False"}`, `{"verbose":false}`},
		{"object from string", `{"filter":"{\"top\":\"3\"}"}`, `{"filter":{"top":3}}`},
		{"array from string", `{"tags":"[\"a\",\"b\"]"}`, `{"tags":["a","b"]}`},
		{"one object as array", `{"intents":{"summary":"x"}}`, `{"intents":[{"summary":"x"}]}`},
		{"nested array in string", `{"intents":"[{\"summary\":\"a\",\"priority\":\"2\",\"steps\":\"[\\\"s1\\\"]\"},{\"summary\":\"b\"}]"}`,
			`{"intents":[{"priority":2,"steps":["s1"],"summary":"a"},{"summary":"b"}]}`},
	}
	for _, c := range cases {
		got, notes := CoerceInput(coerceSchema, json.RawMessage(c.in))
		if string(got) != c.want || len(notes) == 0 {
			t.Errorf("%s: %s (notes %v), want %s", c.name, got, notes, c.want)
		}
		if err := ValidateInput(coerceSchema, got); err != nil {
			t.Errorf("%s: coerced input still invalid: %v", c.name, err)
		}
	}
}

// What would change the meaning is left alone, so the check still fails.
func TestCoerceInputRefuses(t *testing.T) {
	for _, in := range []string{
		`{"limit":"20 rows"}`,  // partial number
		`{"limit":"twenty"}`,   // free text
		`{"limit":2.5}`,        // a float is never truncated
		`{"limit":"2.5"}`,      // not a whole number
		`{"limit":"0"}`,        // below the minimum
		`{"limit":"1000"}`,     // above the maximum
		`{"ratio":"1e400"}`,    // not finite
		`{"verbose":"yes"}`,    // not a boolean word
		`{"tags":"a,b"}`,       // not JSON
		`{"tags":"{\"a\":1}"}`, // JSON of the wrong type
		`{"tags":{"a":1}}`,     // one object where strings are expected
		`{"filter":"[1]"}`,     // an array where an object is expected
	} {
		got, notes := CoerceInput(coerceSchema, json.RawMessage(in))
		if string(got) != in || len(notes) != 0 {
			t.Errorf("%s was coerced to %s (%v)", in, got, notes)
		}
		if ValidateInput(coerceSchema, got) == nil {
			t.Errorf("%s passed validation", in)
		}
	}
	// A valid input is returned as it came.
	in := json.RawMessage(`{"limit":5,"q":"x"}`)
	if got, notes := CoerceInput(coerceSchema, in); string(got) != string(in) || notes != nil {
		t.Errorf("valid input changed: %s %v", got, notes)
	}
}

// The error for what cannot be repaired says what to send.
func TestValidateInputTeaches(t *testing.T) {
	cases := map[string]string{
		`{"limit":"20 rows"}`:               `limit: expected integer, got string "20 rows": use "limit": 20`,
		`{"limit":2.5}`:                     `limit: expected integer, got number 2.5: use "limit": 3 (a whole number)`,
		`{"verbose":"yes"}`:                 `verbose: expected boolean, got string "yes": use "verbose": true`,
		`{"tags":"a,b"}`:                    `tags: expected array, got string "a,b": use "tags": [ ... ] (the JSON array itself, not a string holding it)`,
		`{"q":20}`:                          `q: expected string, got number 20: use "q": "20"`,
		`{"intents":[{"priority":"high"}]}`: `intents[0].priority: expected integer, got string "high": use "priority": 1`,
	}
	for in, want := range cases {
		err := ValidateInput(coerceSchema, json.RawMessage(in))
		if err == nil || err.Error() != want {
			t.Errorf("%s: %v\n want %s", in, err, want)
		}
	}
}

func TestInputCoercionsContext(t *testing.T) {
	ctx := WithInputCoercions(context.Background(), []string{"limit: fixed"})
	if n := InputCoercions(ctx); len(n) != 1 || !strings.HasPrefix(n[0], "limit") {
		t.Fatalf("%v", n)
	}
	if InputCoercions(WithInputCoercions(context.Background(), nil)) != nil {
		t.Fatal("no notes, no value")
	}
}
