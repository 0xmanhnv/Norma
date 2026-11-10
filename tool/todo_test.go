package tool

import (
	"encoding/json"
	"strings"
	"testing"
)

// Old todos were written without a ref. They must still decode (to the zero
// TodoRef) and keep working exactly as before — the Ref field is additive.
func TestTodoDecodeBackwardCompatible(t *testing.T) {
	const old = `{"content":"map the API","status":"pending"}`
	var td Todo
	if err := json.Unmarshal([]byte(old), &td); err != nil {
		t.Fatalf("decode old todo: %v", err)
	}
	if td.Content != "map the API" || td.Status != TodoPending {
		t.Fatalf("old todo decoded wrong: %+v", td)
	}
	if !td.Ref.Empty() {
		t.Fatalf("old todo (no ref) should have an empty ref, got %+v", td.Ref)
	}
}

func TestTodoRefDecodes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want TodoRef
	}{
		{"intent", `{"content":"c","status":"completed","ref":{"intent_id":"I42"}}`, TodoRef{IntentID: "I42"}},
		{"cell", `{"content":"c","status":"pending","ref":{"coverage_cell":"svc:example.com|XSS"}}`, TodoRef{CoverageCell: "svc:example.com|XSS"}},
		{"empty-ref-object", `{"content":"c","status":"pending","ref":{}}`, TodoRef{}},
		{"no-ref", `{"content":"c","status":"pending"}`, TodoRef{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var td Todo
			if err := json.Unmarshal([]byte(tc.in), &td); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if td.Ref != tc.want {
				t.Fatalf("ref = %+v, want %+v", td.Ref, tc.want)
			}
		})
	}
}

// A ref-less todo must marshal to exactly {"content","status"} — no "ref":{} —
// so consumers that re-emit the list and strict decoders keep working.
func TestTodoMarshalOmitsEmptyRef(t *testing.T) {
	b, err := json.Marshal(Todo{Content: "map the API", Status: TodoPending})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"content":"map the API","status":"pending"}` {
		t.Fatalf("empty-ref todo changed wire form: %s", got)
	}
	// decoded into a string-only shape (the self-heal path), it must not error.
	var shape struct {
		Content string `json:"content"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(b, &shape); err != nil {
		t.Fatalf("string-only decode of a ref-less todo failed: %v", err)
	}
}

// A linked todo still carries its ref on the wire, and round-trips.
func TestTodoMarshalKeepsRef(t *testing.T) {
	in := Todo{Content: "sqli", Status: TodoCompleted, Ref: TodoRef{CoverageCell: "svc:h|CWE-89"}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"ref":{"coverage_cell":"svc:h|CWE-89"}`) {
		t.Fatalf("linked todo dropped its ref: %s", b)
	}
	var out Todo
	if err := json.Unmarshal(b, &out); err != nil || out.Ref != in.Ref {
		t.Fatalf("round-trip lost ref: out=%+v err=%v", out, err)
	}
}

func TestTodoRefEmpty(t *testing.T) {
	if !(TodoRef{}).Empty() {
		t.Fatal("zero TodoRef should be Empty")
	}
	if (TodoRef{IntentID: "I1"}).Empty() {
		t.Fatal("ref with an intent id is not Empty")
	}
	if (TodoRef{CoverageCell: "svc:h|C"}).Empty() {
		t.Fatal("ref with a coverage cell is not Empty")
	}
}

// The TodoWrite tool still accepts a plain todo (no ref) and a reffed todo, and
// the stored list round-trips the ref.
func TestTodoStoreKeepsRef(t *testing.T) {
	s := NewTodoStore()
	s.set([]Todo{
		{Content: "plain", Status: TodoPending},
		{Content: "linked", Status: TodoCompleted, Ref: TodoRef{IntentID: "I7"}},
	})
	got := s.List()
	if len(got) != 2 {
		t.Fatalf("want 2 todos, got %d", len(got))
	}
	if !got[0].Ref.Empty() {
		t.Fatalf("plain todo should have empty ref, got %+v", got[0].Ref)
	}
	if got[1].Ref.IntentID != "I7" {
		t.Fatalf("linked todo lost its ref: %+v", got[1].Ref)
	}
}
