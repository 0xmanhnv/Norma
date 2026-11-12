package hook

import (
	"context"
	"testing"

	"github.com/0xmanhnv/corvid/permission"
)

func TestPreToolUseBlockAndRewrite(t *testing.T) {
	r := NewRegistry()
	r.On(PreToolUse, func(_ context.Context, ev Event) Result {
		if ev.ToolName == "Bash" {
			return Result{Decision: "block", Message: "no bash"}
		}
		return Result{UpdatedInput: []byte(`{"x":1}`)}
	})
	if block, msg, _ := r.PreToolUse(context.Background(), "Bash", nil); !block || msg != "no bash" {
		t.Fatalf("expected block, got %v %q", block, msg)
	}
	if block, _, upd := r.PreToolUse(context.Background(), "Read", nil); block || string(upd) != `{"x":1}` {
		t.Fatalf("expected rewrite, got block=%v upd=%s", block, upd)
	}
}

func TestStopHookBlockingContinues(t *testing.T) {
	r := NewRegistry()
	calls := 0
	r.On(Stop, func(_ context.Context, _ Event) Result {
		calls++
		if calls == 1 {
			return Result{Decision: "block", AdditionalContext: "keep going"}
		}
		return Result{}
	})
	// First call: a "block" decision → continue with an injected message.
	prevent, blocking, _ := r.Stop(context.Background(), nil)
	if prevent || len(blocking) != 1 || blocking[0] != "keep going" {
		t.Fatalf("blocking path wrong: prevent=%v blocking=%v", prevent, blocking)
	}
	// Second call: no decision → allow stop.
	if prevent, blocking, _ := r.Stop(context.Background(), nil); prevent || len(blocking) != 0 {
		t.Fatalf("second Stop should allow: prevent=%v blocking=%v", prevent, blocking)
	}
}

func TestStopHookPreventTerminates(t *testing.T) {
	r := NewRegistry()
	r.On(Stop, func(_ context.Context, _ Event) Result {
		return Result{PreventContinuation: true, Message: "halt"}
	})
	prevent, _, msg := r.Stop(context.Background(), nil)
	if !prevent || msg != "halt" {
		t.Fatalf("prevent path wrong: prevent=%v msg=%q", prevent, msg)
	}
}

// Only "approve" or an empty decision lets an action through: a misspelt or
// unknown decision blocks instead of approving, unless the run's policy says
// to ignore unknown decisions (the behaviour before).
func TestUnknownDecisionBlocks(t *testing.T) {
	for _, d := range []string{"deny", "Block", "BLOCK", "reject", "blokc"} {
		r := NewRegistry().On(PreToolUse, func(context.Context, Event) Result {
			return Result{Decision: d, Message: "no"}
		})
		if block, _, _ := r.PreToolUse(context.Background(), "Bash", nil); !block {
			t.Errorf("decision %q let the tool call through", d)
		}
	}
	ignore := permission.WithPolicy(context.Background(), permission.Policy{HookDecisions: permission.HookDecisionsIgnoreUnknown})
	for _, d := range []string{"deny", "Block", "reject"} {
		r := NewRegistry().On(PreToolUse, func(context.Context, Event) Result { return Result{Decision: d} })
		if block, _, _ := r.PreToolUse(ignore, "Bash", nil); block {
			t.Errorf("ignore_unknown: decision %q blocked", d)
		}
	}
	r := NewRegistry().On(PreToolUse, func(context.Context, Event) Result { return Result{Decision: "block"} })
	if block, _, _ := r.PreToolUse(ignore, "Bash", nil); !block {
		t.Error(`ignore_unknown: "block" no longer blocks`)
	}
	for _, d := range []string{"", "approve"} {
		r := NewRegistry().On(PreToolUse, func(context.Context, Event) Result { return Result{Decision: d} })
		if block, _, _ := r.PreToolUse(context.Background(), "Bash", nil); block {
			t.Errorf("decision %q blocked", d)
		}
	}
}
