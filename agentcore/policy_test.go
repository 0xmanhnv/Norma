package agentcore

import (
	"reflect"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/permission"
)

// policyWarningPrefix starts every warning about a permission.Policy item
// that is not at its strict value.
const policyWarningPrefix = "agentcore: permission.Policy."

// appendNonPolicy collects warnings other than the policy ones, for tests
// about other configuration warnings.
func appendNonPolicy(ws []string, m string) []string {
	if strings.HasPrefix(m, policyWarningPrefix) {
		return ws
	}
	return append(ws, m)
}

// Every Policy item not at its strict value is reported once at session
// start, naming the field and the value to set; the strict preset is silent.
func TestPolicyWarnings(t *testing.T) {
	count := func(p permission.Policy) []string {
		var ws []string
		s := NewSession(Options{Policy: p, OnWarn: func(m string) {
			if strings.HasPrefix(m, policyWarningPrefix) {
				ws = append(ws, m)
			}
		}})
		_ = s.Close()
		return ws
	}
	if ws := count(permission.StrictPolicy()); len(ws) != 0 {
		t.Fatalf("strict policy warned: %v", ws)
	}
	compat := count(permission.CompatiblePolicy())
	if n := reflect.TypeOf(permission.Policy{}).NumField(); len(compat) != n {
		t.Fatalf("compatible policy: %d warnings, want one per field (%d): %v", len(compat), n, compat)
	}
	for _, w := range compat {
		if !strings.Contains(w, "StrictPolicy()") {
			t.Errorf("warning does not name the strict option: %q", w)
		}
	}
	// The defaults: exactly the fields whose default is not the strict value.
	resolved := reflect.ValueOf(permission.Policy{}.Resolved())
	strict := reflect.ValueOf(permission.StrictPolicy())
	var want []string
	for i := 0; i < resolved.NumField(); i++ {
		if resolved.Field(i).String() != strict.Field(i).String() {
			want = append(want, resolved.Type().Field(i).Name)
		}
	}
	def := count(permission.Policy{})
	if len(def) != len(want) {
		t.Fatalf("default policy warnings = %v, want one each for %v", def, want)
	}
	for i, f := range want {
		if !strings.Contains(def[i], "Policy."+f+" ") {
			t.Fatalf("warning %d = %q, want it about %s", i, def[i], f)
		}
	}
}
