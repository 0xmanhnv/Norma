package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/0xmanhnv/corvid/permission"
)

// TodoStatus is the state of a todo item.
type TodoStatus string

const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoCompleted  TodoStatus = "completed"
)

// Todo is one task in the agent's working plan.
type Todo struct {
	Content string     `json:"content"`
	Status  TodoStatus `json:"status"`
	// Meta is optional free-form metadata: string keys and values the harness
	// stores and returns but never interprets. A host uses it to tie a todo to
	// a record of its own (a ticket id, a row key) so it can check the todo
	// against its own state instead of trusting the mark. Nil for an ordinary
	// todo, which then encodes exactly as {"content","status"}.
	//
	// A todo written by an older version carries a "ref" object instead; its
	// string fields are read into Meta under the same keys (see UnmarshalJSON).
	//
	// Stored, and read by the host, Meta is a map. The TodoWrite tool offers the
	// model a list of {"key","value"} pairs instead, because a schema with an
	// open object cannot be sent to a provider in strict tool-call mode; both
	// shapes are read.
	Meta map[string]string `json:"meta,omitempty"`
}

// UnmarshalJSON reads a todo. "meta" is an object of string values (how a todo
// is stored) or a list of {"key","value"} string pairs (how the TodoWrite tool
// offers it to the model); an empty key is dropped and a repeated key keeps its
// last value. Besides "meta" it accepts the deprecated "ref" object earlier
// versions wrote: each of its string fields becomes a Meta entry under the same
// key, unless "meta" already has that key. Stored lists and transcripts from
// before the change therefore load unchanged. "ref" is never written back.
func (t *Todo) UnmarshalJSON(data []byte) error {
	var in struct {
		Content string          `json:"content"`
		Status  TodoStatus      `json:"status"`
		Meta    json.RawMessage `json:"meta"`
		Ref     map[string]any  `json:"ref"` // deprecated
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	meta, err := decodeTodoMeta(in.Meta)
	if err != nil {
		return err
	}
	t.Content, t.Status, t.Meta = in.Content, in.Status, meta
	for k, v := range in.Ref {
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		if _, set := t.Meta[k]; set {
			continue
		}
		if t.Meta == nil {
			t.Meta = map[string]string{}
		}
		t.Meta[k] = s
	}
	if len(t.Meta) == 0 {
		t.Meta = nil
	}
	return nil
}

// decodeTodoMeta reads the "meta" member of a todo in either shape.
func decodeTodoMeta(raw json.RawMessage) (map[string]string, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] != '[' {
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("todo meta: %w", err)
		}
		return m, nil
	}
	var pairs []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &pairs); err != nil {
		return nil, fmt.Errorf("todo meta: %w", err)
	}
	m := make(map[string]string, len(pairs))
	for _, p := range pairs {
		if p.Key != "" {
			m[p.Key] = p.Value
		}
	}
	return m, nil
}

// TodoPersister keeps a todo list outside the process, so that it survives a
// restart. Implement it over whatever the host stores state in (a file, a
// database row keyed by the session) and hand it to NewPersistentTodoStore;
// TodoFile is a ready-made one over a JSON file.
//
// SaveTodos receives the whole list each time the model rewrites it and must
// replace what was stored. LoadTodos returns the stored list, and (nil, nil)
// when nothing has been stored yet. Both are called with the store's lock
// held, one at a time.
type TodoPersister interface {
	LoadTodos() ([]Todo, error)
	SaveTodos([]Todo) error
}

// TodoStore holds the current todo list across tool calls. It is safe for
// concurrent reads; the host can inspect progress via List.
type TodoStore struct {
	mu      sync.Mutex
	items   []Todo
	persist TodoPersister
	saveErr error
}

// NewTodoStore returns an empty store that lives in memory only.
func NewTodoStore() *TodoStore { return &TodoStore{} }

// NewPersistentTodoStore returns a store backed by p: it starts with the list
// p has stored, and every list the model writes is saved through p. With it a
// session that is resumed after a restart still has its plan, where a plain
// store starts empty and the model has to rebuild the list from a summary.
//
// A load error is returned together with a usable, empty store. A save error
// does not fail the model's call (the list is still current in memory); it is
// kept for the host to read with Err.
func NewPersistentTodoStore(p TodoPersister) (*TodoStore, error) {
	s := &TodoStore{persist: p}
	if p == nil {
		return s, nil
	}
	items, err := p.LoadTodos()
	if err != nil {
		return s, fmt.Errorf("tool: load todos: %w", err)
	}
	s.items = items
	return s, nil
}

// List returns a snapshot of the current todos.
func (s *TodoStore) List() []Todo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Todo(nil), s.items...)
}

// Err returns the error of the most recent save to the store's TodoPersister,
// nil when it succeeded or the store is not persistent.
func (s *TodoStore) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveErr
}

func (s *TodoStore) set(items []Todo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = items
	if s.persist != nil {
		s.saveErr = s.persist.SaveTodos(append([]Todo(nil), items...))
	}
}

// TodoFile returns a TodoPersister that keeps the list in the JSON file at
// path. The file is replaced atomically on every save (written beside itself,
// then renamed), so a crash leaves either the old list or the new one. A
// missing file is an empty list.
func TodoFile(path string) TodoPersister { return todoFile(path) }

type todoFile string

func (f todoFile) LoadTodos() ([]Todo, error) {
	b, err := os.ReadFile(string(f))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var items []Todo
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, fmt.Errorf("%s: %w", string(f), err)
	}
	return items, nil
}

func (f todoFile) SaveTodos(items []Todo) error {
	if items == nil {
		items = []Todo{}
	}
	b, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(string(f)), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(string(f)), filepath.Base(string(f))+".*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), string(f))
	}
	if werr != nil {
		os.Remove(tmp.Name())
	}
	return werr
}

// Tool returns the TodoWrite tool bound to this store. The agent uses it to
// plan and track multi-step work; each call replaces the whole list.
func (s *TodoStore) Tool() CoreTool {
	return Build(Spec{
		Name:        TodoWriteName,
		Description: "Records and updates a structured todo list for the current task. Use it to plan multi-step work and to mark progress — replace the whole list each call, marking exactly one item in_progress while working.",
		Prompt:      "status is one of: pending, in_progress, completed. Keep the list current: mark a task completed as soon as it's done; don't batch completions. Optional meta is a list of {key, value} string pairs the host gave you for a todo; pass them back unchanged.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"todos": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"content": map[string]any{"type": "string"},
							"status":  map[string]any{"type": "string", "enum": []any{"pending", "in_progress", "completed"}},
							"meta": map[string]any{
								"type":        "array",
								"description": "Optional. A list of {key, value} string pairs kept with the todo and returned unchanged, such as the id of a record this todo tracks. Omit for a plain todo.",
								"items": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"key":   map[string]any{"type": "string", "description": "The name of the entry."},
										"value": map[string]any{"type": "string", "description": "The value of the entry."},
									},
									"required":             []any{"key", "value"},
									"additionalProperties": false,
								},
							},
						},
						"required": []any{"content", "status"},
					},
				},
			},
			"required": []any{"todos"},
		},
		ReadOnly:   func(json.RawMessage) bool { return false },
		Concurrent: func(json.RawMessage) bool { return false },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(_ context.Context, input json.RawMessage, _ *ToolContext) (Result, error) {
			var in struct {
				Todos []Todo `json:"todos"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return Result{}, err
			}
			s.set(in.Todos)
			// Ack only — the list itself is visible in the model's own tool_use
			// input and is re-surfaced on demand via the todo system-reminder
			// (harness). Mirrors claude-code's TodoWrite result.
			return Text(todoAck), nil
		},
	})
}

const todoAck = "Todos have been modified successfully. Keep the todo list current and continue with the tasks."

// RenderTodoReminder renders the todo list for the system-reminder body, one
// numbered item per line ("N. [status] content"). Returns "" for an empty list.
func RenderTodoReminder(todos []Todo) string {
	if len(todos) == 0 {
		return ""
	}
	var b strings.Builder
	for i, t := range todos {
		fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, t.Status, t.Content)
	}
	return strings.TrimRight(b.String(), "\n")
}
