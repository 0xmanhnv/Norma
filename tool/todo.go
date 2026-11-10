package tool

import (
	"context"
	"encoding/json"
	"fmt"
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

// TodoRef optionally links a todo to a record the host keeps, so the host can
// annotate the todo's real status from its own truth instead of trusting the
// model's manual mark. Both fields are opaque strings to the harness — the host
// populates and interprets them. The zero value (both empty) means "no link":
// the todo is an ordinary free-text item and behaves exactly as before.
type TodoRef struct {
	// IntentID names a unit of work the host tracks (e.g. an exploration intent
	// id). Empty when this todo does not track one.
	IntentID string `json:"intent_id,omitempty"`
	// CoverageCell names a coverage-matrix cell the host tracks. Empty when this
	// todo does not track one.
	CoverageCell string `json:"coverage_cell,omitempty"`
}

// Empty reports whether the ref links to nothing — a plain free-text todo.
func (r TodoRef) Empty() bool { return r.IntentID == "" && r.CoverageCell == "" }

// Todo is one task in the agent's working plan.
type Todo struct {
	Content string     `json:"content"`
	Status  TodoStatus `json:"status"`
	// Ref optionally links this todo to a host record (see TodoRef). Empty =
	// a free-text todo, the default and the backward-compatible behavior: a
	// todo written without a ref decodes to the zero TodoRef.
	//
	// `omitempty` does not drop a zero struct, so MarshalJSON below omits an
	// empty ref by hand — a ref-less todo must serialize exactly as it did
	// before this field existed ({"content","status"}), or consumers that
	// re-emit the list (e.g. todo self-heal) would start carrying a stray
	// "ref":{} that strict decoders reject.
	Ref TodoRef `json:"ref,omitempty"`
}

// MarshalJSON writes a todo, omitting an empty ref so a ref-less todo keeps the
// exact {"content","status"} wire form it had before Ref was added.
func (t Todo) MarshalJSON() ([]byte, error) {
	type todoJSON struct {
		Content string     `json:"content"`
		Status  TodoStatus `json:"status"`
		Ref     *TodoRef   `json:"ref,omitempty"`
	}
	tj := todoJSON{Content: t.Content, Status: t.Status}
	if !t.Ref.Empty() {
		r := t.Ref
		tj.Ref = &r
	}
	return json.Marshal(tj)
}

// TodoStore holds the current todo list across tool calls. It is safe for
// concurrent reads; the host can inspect progress via List.
type TodoStore struct {
	mu    sync.Mutex
	items []Todo
}

// NewTodoStore returns an empty store.
func NewTodoStore() *TodoStore { return &TodoStore{} }

// List returns a snapshot of the current todos.
func (s *TodoStore) List() []Todo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Todo(nil), s.items...)
}

func (s *TodoStore) set(items []Todo) {
	s.mu.Lock()
	s.items = items
	s.mu.Unlock()
}

// Tool returns the TodoWrite tool bound to this store. The agent uses it to
// plan and track multi-step work; each call replaces the whole list.
func (s *TodoStore) Tool() CoreTool {
	return Build(Spec{
		Name:        TodoWriteName,
		Description: "Records and updates a structured todo list for the current task. Use it to plan multi-step work and to mark progress — replace the whole list each call, marking exactly one item in_progress while working.",
		Prompt:      "status is one of: pending, in_progress, completed. Keep the list current: mark a task completed as soon as it's done; don't batch completions. Optional ref links a todo to a host record.",
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
							"ref": map[string]any{
								"type":        "object",
								"description": "Optional. Links this todo to a host record so its completion is verified from the host's truth, not the manual mark. Omit for a plain todo.",
								"properties": map[string]any{
									"intent_id":     map[string]any{"type": "string", "description": "id of the unit of work this todo tracks"},
									"coverage_cell": map[string]any{"type": "string", "description": "key of the coverage-matrix cell this todo tracks"},
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
