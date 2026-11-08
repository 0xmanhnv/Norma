package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// CoerceInput repairs the common ways a model mistypes tool arguments, by the
// declared schema, before the input is validated:
//
//   - a string where an integer or number is declared, when the whole string
//     is that number (no partial parses), within minimum/maximum when set;
//   - "true" or "false" where a boolean is declared;
//   - a string holding JSON of the declared array or object type (an array
//     sent JSON-encoded as a string), applied again inside the parsed value;
//   - a single object where an array of objects is declared, as a one-element
//     array.
//
// Nothing else changes: a float is never truncated to an integer and free
// text is never read as a number. It returns the input to use (raw itself
// when nothing changed) and one note per repair, such as
// "limit: string \"20\" read as integer 20".
func CoerceInput(schema map[string]any, raw json.RawMessage) (json.RawMessage, []string) {
	if schema == nil || len(raw) == 0 {
		return raw, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw, nil
	}
	var notes []string
	out := coerceValue(schema, v, "", &notes)
	if len(notes) == 0 {
		return raw, nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw, nil
	}
	return b, notes
}

func coerceValue(schema map[string]any, v any, path string, notes *[]string) any {
	typ, _ := schema["type"].(string)
	switch typ {
	case "integer", "number":
		s, ok := v.(string)
		if !ok {
			return v
		}
		n, ok := exactNumber(s, typ == "integer")
		if !ok || !inRange(schema, n) {
			return v
		}
		*notes = append(*notes, fmt.Sprintf("%s: string %q read as %s %s", at(path), s, typ, strconv.FormatFloat(n, 'f', -1, 64)))
		return n
	case "boolean":
		s, ok := v.(string)
		if !ok {
			return v
		}
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true":
			*notes = append(*notes, fmt.Sprintf("%s: string %q read as boolean true", at(path), s))
			return true
		case "false":
			*notes = append(*notes, fmt.Sprintf("%s: string %q read as boolean false", at(path), s))
			return false
		}
		return v
	case "array":
		if s, ok := v.(string); ok {
			var parsed any
			if json.Unmarshal([]byte(strings.TrimSpace(s)), &parsed) == nil {
				if arr, ok := parsed.([]any); ok {
					*notes = append(*notes, fmt.Sprintf("%s: JSON array sent as a string, read as the array", at(path)))
					v = arr
				}
			}
		}
		items, _ := schema["items"].(map[string]any)
		if m, ok := v.(map[string]any); ok && items != nil && items["type"] == "object" {
			*notes = append(*notes, fmt.Sprintf("%s: single object read as a one-element array", at(path)))
			v = []any{m}
		}
		arr, ok := v.([]any)
		if !ok || items == nil {
			return v
		}
		for i, el := range arr {
			arr[i] = coerceValue(items, el, fmt.Sprintf("%s[%d]", path, i), notes)
		}
		return arr
	case "object", "":
		if s, ok := v.(string); ok && typ == "object" {
			var parsed any
			if json.Unmarshal([]byte(strings.TrimSpace(s)), &parsed) == nil {
				if m, ok := parsed.(map[string]any); ok {
					*notes = append(*notes, fmt.Sprintf("%s: JSON object sent as a string, read as the object", at(path)))
					v = m
				}
			}
		}
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		props, _ := schema["properties"].(map[string]any)
		for key, val := range m {
			if ps, ok := props[key].(map[string]any); ok {
				m[key] = coerceValue(ps, val, join(path, key), notes)
			}
		}
		return m
	}
	return v
}

// exactNumber parses s as a whole number (integer=true) or a finite number;
// the whole string must be the number.
func exactNumber(s string, integer bool) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if integer {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n > 1<<53 || n < -(1<<53) {
			return 0, false
		}
		return float64(n), true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// inRange honours minimum/maximum when the schema declares them.
func inRange(schema map[string]any, n float64) bool {
	if lo, ok := asFloat(schema["minimum"]); ok && n < lo {
		return false
	}
	if hi, ok := asFloat(schema["maximum"]); ok && n > hi {
		return false
	}
	return true
}

// asFloat reads a schema bound written in JSON (float64) or Go (int).
func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

type coercionsKey struct{}

// WithInputCoercions records on a tool call's context the repairs
// CoerceInput made to its input, so a wrapper can count them or tell the
// model.
func WithInputCoercions(ctx context.Context, notes []string) context.Context {
	if len(notes) == 0 {
		return ctx
	}
	return context.WithValue(ctx, coercionsKey{}, notes)
}

// InputCoercions returns the repairs made to the input of the call ctx
// belongs to (nil when none).
func InputCoercions(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	n, _ := ctx.Value(coercionsKey{}).([]string)
	return n
}
