package tool

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

// ValidateInput checks a raw JSON input against a minimal subset of JSON Schema
// sufficient for tool inputs: object type, required properties, per-property
// primitive type checks (string/number/integer/boolean/array/object) and enum
// membership. It never panics; a non-nil error means the input is invalid (FR-04.5, NFR-07).
func ValidateInput(schema map[string]any, raw json.RawMessage) error {
	if schema == nil {
		return nil
	}
	var v any
	if len(raw) == 0 {
		v = map[string]any{}
	} else if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("input is not valid JSON: %w", err)
	}
	return validateValue(schema, v, "")
}

func validateValue(schema map[string]any, v any, path string) error {
	typ, _ := schema["type"].(string)
	switch typ {
	case "object", "":
		m, ok := v.(map[string]any)
		if !ok {
			if typ == "" {
				break // no type declared: anything goes, but an enum still applies
			}
			return wrongType(path, typ, v)
		}
		for _, req := range toStrings(schema["required"]) {
			if _, present := m[req]; !present {
				return fmt.Errorf("%s: missing required property %q", at(path), req)
			}
		}
		props, _ := schema["properties"].(map[string]any)
		for key, val := range m {
			ps, ok := props[key].(map[string]any)
			if !ok {
				continue // unknown property — tolerate
			}
			if err := validateValue(ps, val, join(path, key)); err != nil {
				return err
			}
		}
	case "string":
		if _, ok := v.(string); !ok {
			return wrongType(path, typ, v)
		}
	case "integer":
		if f, ok := v.(float64); !ok || f != float64(int64(f)) {
			return wrongType(path, typ, v)
		}
	case "number":
		if _, ok := v.(float64); !ok {
			return wrongType(path, typ, v)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return wrongType(path, typ, v)
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return wrongType(path, typ, v)
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, el := range arr {
				if err := validateValue(items, el, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	return checkEnum(schema, v, path)
}

// checkEnum enforces a schema's "enum": the value must equal one of the listed
// values. The list is compared as JSON, so it may be written as []any, as a
// typed Go slice ([]string{"a", "b"}), or come decoded from a JSON schema. An
// enum that is not a list is ignored, like any other keyword this validator
// does not understand.
func checkEnum(schema map[string]any, v any, path string) error {
	raw, ok := schema["enum"]
	if !ok || raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var allowed []any
	if json.Unmarshal(b, &allowed) != nil {
		return nil
	}
	for _, a := range allowed {
		if reflect.DeepEqual(a, v) {
			return nil
		}
	}
	list := strings.TrimSuffix(strings.TrimPrefix(string(b), "["), "]")
	got, _ := json.Marshal(v)
	return fmt.Errorf("%s: must be one of %s, got %s", at(path), strings.ReplaceAll(list, ",", ", "), got)
}

func toStrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func at(path string) string {
	if path == "" {
		return "input"
	}
	return strings.TrimPrefix(path, ".")
}

// wrongType explains a type mismatch so the model can fix the call: the type
// expected, what came instead, and the field with a corrected value built
// from what was sent, e.g. `limit must be an integer, got string "20 rows":
// use "limit": 20`. The leading "<path>: expected <type>" stays for callers
// that match on it.
func wrongType(path, want string, v any) error {
	key := path
	if i := strings.LastIndexAny(key, ".]"); i >= 0 && i+1 < len(key) {
		key = key[i+1:]
	}
	field := ""
	if key != "" && !strings.HasSuffix(path, "]") {
		field = fmt.Sprintf("%q: ", key)
	}
	return fmt.Errorf("%s: expected %s, got %s: use %s%s", at(path), want, describe(v), field, example(want, v))
}

func describe(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		if r := []rune(t); len(r) > 40 {
			t = string(r[:40]) + "…"
		}
		return fmt.Sprintf("string %q", t)
	case float64:
		return "number " + strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return "boolean " + strconv.FormatBool(t)
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

var reDigits = regexp.MustCompile(`-?\d+`)

// example is a value of type want built from v where it can be.
func example(want string, v any) string {
	switch want {
	case "integer":
		switch t := v.(type) {
		case float64:
			return strconv.FormatInt(int64(math.Round(t)), 10) + " (a whole number)"
		case string:
			if d := reDigits.FindString(t); d != "" {
				return d
			}
		}
		return "1"
	case "number":
		if s, ok := v.(string); ok {
			if d := reDigits.FindString(s); d != "" {
				return d
			}
		}
		return "1"
	case "boolean":
		if s, ok := v.(string); ok && strings.Contains(strings.ToLower(s), "false") {
			return "false"
		}
		return "true"
	case "string":
		b, _ := json.Marshal(fmt.Sprint(v))
		if f, ok := v.(float64); ok {
			b, _ = json.Marshal(strconv.FormatFloat(f, 'f', -1, 64))
		}
		return string(b)
	case "array":
		if _, ok := v.(string); ok {
			return "[ ... ] (the JSON array itself, not a string holding it)"
		}
		return "[ ... ]"
	case "object":
		if _, ok := v.(string); ok {
			return "{ ... } (the JSON object itself, not a string holding it)"
		}
		return "{ ... }"
	}
	return want
}
