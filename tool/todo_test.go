package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A plain todo has no metadata and encodes exactly as {"content","status"}:
// consumers that re-emit the list, and strict decoders, see no new key.
func TestTodoPlainWireForm(t *testing.T) {
	const plain = `{"content":"map the API","status":"pending"}`
	var td Todo
	if err := json.Unmarshal([]byte(plain), &td); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if td.Content != "map the API" || td.Status != TodoPending || td.Meta != nil {
		t.Fatalf("decoded %+v", td)
	}
	b, err := json.Marshal(td)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != plain {
		t.Fatalf("plain todo encodes as %s, want %s", b, plain)
	}
}

// Meta is free-form: any string keys round-trip, and the harness gives none of
// them a meaning.
func TestTodoMetaRoundTrips(t *testing.T) {
	in := Todo{Content: "close the ticket", Status: TodoCompleted, Meta: map[string]string{"ticket": "T-42", "row": "a|b"}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"content":"close the ticket","status":"completed","meta":{"row":"a|b","ticket":"T-42"}}`; string(b) != want {
		t.Fatalf("encoded %s, want %s", b, want)
	}
	var out Todo
	if err := json.Unmarshal(b, &out); err != nil || !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip: %+v, %v", out, err)
	}
}

// The deprecated "ref" object older versions wrote is still read: each of its
// string fields lands in Meta under the same key. It is never written back,
// "meta" wins over it for a key both carry, and values that are not strings
// are ignored.
func TestTodoReadsDeprecatedRefObject(t *testing.T) {
	cases := []struct {
		name string
		json string
		want map[string]string
	}{
		{"one key", `{"content":"c","status":"completed","ref":{"record_id":"R42"}}`, map[string]string{"record_id": "R42"}},
		{"two keys", `{"content":"c","status":"pending","ref":{"record_id":"R42","group":"g|1"}}`, map[string]string{"record_id": "R42", "group": "g|1"}},
		{"empty ref object", `{"content":"c","status":"pending","ref":{}}`, nil},
		{"empty and non-string values", `{"content":"c","status":"pending","ref":{"a":"","b":7,"c":"kept"}}`, map[string]string{"c": "kept"}},
		{"meta wins", `{"content":"c","status":"pending","meta":{"record_id":"new"},"ref":{"record_id":"old","group":"g"}}`,
			map[string]string{"record_id": "new", "group": "g"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var td Todo
			if err := json.Unmarshal([]byte(tc.json), &td); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(td.Meta, tc.want) {
				t.Fatalf("meta = %v, want %v", td.Meta, tc.want)
			}
			b, _ := json.Marshal(td)
			var back map[string]any
			_ = json.Unmarshal(b, &back)
			if _, has := back["ref"]; has {
				t.Fatalf("the deprecated key was written back: %s", b)
			}
		})
	}
}

// The TodoWrite tool takes plain todos, todos with meta, and todos in the old
// shape, and the store returns them with their metadata.
func TestTodoWriteKeepsMeta(t *testing.T) {
	s := NewTodoStore()
	in := `{"todos":[
		{"content":"plain","status":"pending"},
		{"content":"linked","status":"completed","meta":{"ticket":"T-7"}},
		{"content":"old shape","status":"in_progress","ref":{"record_id":"R1"}}]}`
	if err := ValidateInput(s.Tool().InputSchema(), []byte(in)); err != nil {
		t.Fatalf("the tool's schema rejects the input: %v", err)
	}
	if _, err := s.Tool().Call(context.Background(), []byte(in), &ToolContext{}); err != nil {
		t.Fatal(err)
	}
	got := s.List()
	if len(got) != 3 || got[0].Meta != nil || got[1].Meta["ticket"] != "T-7" || got[2].Meta["record_id"] != "R1" {
		t.Fatalf("stored %+v", got)
	}
}

// A persistent store saves every list the model writes and starts from the
// stored list, so a restart keeps the plan.
func TestPersistentTodoStoreSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "todos.json")

	first, err := NewPersistentTodoStore(TodoFile(path))
	if err != nil || len(first.List()) != 0 {
		t.Fatalf("a store over a missing file: %v, %v", first.List(), err)
	}
	in := `{"todos":[{"content":"write the report","status":"in_progress","meta":{"ticket":"T-7"}},{"content":"send it","status":"pending"}]}`
	if _, err := first.Tool().Call(context.Background(), []byte(in), &ToolContext{}); err != nil {
		t.Fatal(err)
	}
	if err := first.Err(); err != nil {
		t.Fatalf("save: %v", err)
	}

	// A new process: a new store over the same file.
	second, err := NewPersistentTodoStore(TodoFile(path))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second.List(), first.List()) || len(second.List()) != 2 {
		t.Fatalf("after a restart: %+v, want %+v", second.List(), first.List())
	}
	if got := RenderTodoReminder(second.List()); got != "1. [in_progress] write the report\n2. [pending] send it" {
		t.Fatalf("reminder from the restored list: %q", got)
	}

	// Clearing the list is saved too, and no temporary file is left behind.
	if _, err := second.Tool().Call(context.Background(), []byte(`{"todos":[]}`), &ToolContext{}); err != nil {
		t.Fatal(err)
	}
	third, _ := NewPersistentTodoStore(TodoFile(path))
	if len(third.List()) != 0 {
		t.Fatalf("a cleared list came back: %+v", third.List())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("%d files beside the list, want only the list", len(entries))
	}
}

type failingTodos struct{ loadErr, saveErr error }

func (f failingTodos) LoadTodos() ([]Todo, error) { return nil, f.loadErr }
func (f failingTodos) SaveTodos([]Todo) error     { return f.saveErr }

// A persister that fails never costs the model its call: the list stays
// current in memory and the error is the host's to read.
func TestPersistentTodoStoreErrors(t *testing.T) {
	boom := errors.New("disk full")
	s, err := NewPersistentTodoStore(failingTodos{saveErr: boom})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Tool().Call(context.Background(), []byte(`{"todos":[{"content":"a","status":"pending"}]}`), &ToolContext{})
	if err != nil || res.IsError {
		t.Fatalf("the call failed with the save: %+v, %v", res, err)
	}
	if len(s.List()) != 1 || !errors.Is(s.Err(), boom) {
		t.Fatalf("list %v, err %v", s.List(), s.Err())
	}

	s, err = NewPersistentTodoStore(failingTodos{loadErr: boom})
	if !errors.Is(err, boom) || s == nil || len(s.List()) != 0 {
		t.Fatalf("load failure: store %v, err %v", s, err)
	}

	// A corrupt file is reported, not treated as an empty list silently.
	path := filepath.Join(t.TempDir(), "todos.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPersistentTodoStore(TodoFile(path)); err == nil {
		t.Fatal("a corrupt file loaded without an error")
	}
}
