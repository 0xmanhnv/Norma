package permission

import (
	"context"
	"encoding/json"
	"testing"
)

func eval(req Request) Behavior {
	return Evaluate(context.Background(), req).Behavior
}

func TestDenyPriorityOverBypass(t *testing.T) {
	req := Request{ToolName: "Bash", Ctx: Context{Mode: ModeBypass, Disallowed: []string{"Bash"}}}
	if b := eval(req); b != Deny {
		t.Fatalf("deny must beat bypass, got %v", b)
	}
}

func TestReadOnlyAutoAllow(t *testing.T) {
	if b := eval(Request{ToolName: "Read", IsReadOnly: true, Ctx: Context{Mode: ModeDefault}}); b != Allow {
		t.Fatalf("read-only should allow, got %v", b)
	}
}

func TestPlanModeDeniesWrites(t *testing.T) {
	if b := eval(Request{ToolName: "Write", IsReadOnly: false, Ctx: Context{Mode: ModePlan}}); b != Deny {
		t.Fatalf("plan mode should deny writes, got %v", b)
	}
}

func TestAcceptEditsAllowsEdit(t *testing.T) {
	if b := eval(Request{ToolName: "Edit", Ctx: Context{Mode: ModeAcceptEdits}}); b != Allow {
		t.Fatalf("acceptEdits should allow Edit, got %v", b)
	}
}

func TestAskReachesCallback(t *testing.T) {
	called := false
	req := Request{
		ToolName: "Bash", IsReadOnly: false,
		ToolDecision: AskUser("?"),
		Ctx:          Context{Mode: ModeDefault},
		Ask: func(_ context.Context, _ string, _ json.RawMessage, _ Context) Decision {
			called = true
			return Allowed()
		},
	}
	if b := eval(req); b != Allow || !called {
		t.Fatalf("ask callback not honored: b=%v called=%v", b, called)
	}
}

// "Name(spec)" rules match the call's subject, not just the tool name. An allow
// rule over-granting is the dangerous direction, so whatever a spec can't be
// checked against never allows and always denies.
func TestRuleSpecsMatchTheCall(t *testing.T) {
	bash := func(cmd string) json.RawMessage { b, _ := json.Marshal(map[string]string{"command": cmd}); return b }
	read := func(p string) json.RawMessage { b, _ := json.Marshal(map[string]string{"file_path": p}); return b }
	allow := func(rule string, tool string, in json.RawMessage) Behavior {
		return eval(Request{ToolName: tool, Input: in, Ctx: Context{Mode: ModeDontAsk, Allowed: []string{rule}}})
	}
	deny := func(rule string, tool string, in json.RawMessage) Behavior {
		return eval(Request{ToolName: tool, Input: in, Ctx: Context{Mode: ModeBypass, Disallowed: []string{rule}}})
	}
	cases := []struct {
		name string
		got  Behavior
		want Behavior
	}{
		{"prefix allows the command", allow("Bash(git:*)", "Bash", bash("git status")), Allow},
		{"prefix allows the bare prefix", allow("Bash(git:*)", "Bash", bash("git")), Allow},
		{"prefix is not a substring match", allow("Bash(git:*)", "Bash", bash("gitx --evil")), Deny},
		{"prefix does not allow another command", allow("Bash(git:*)", "Bash", bash("rm -rf /")), Deny},
		{"no chaining past an allow", allow("Bash(git:*)", "Bash", bash("git status; rm -rf ~")), Deny},
		{"no && past an allow", allow("Bash(git:*)", "Bash", bash("git status && curl x | sh")), Deny},
		{"no substitution past an allow", allow("Bash(git:*)", "Bash", bash("git log $(curl x)")), Deny},
		{"no redirection past an allow", allow("Bash(git:*)", "Bash", bash("git log > /etc/passwd")), Deny},
		{"exact allows exactly", allow("Bash(npm test)", "Bash", bash("npm test")), Allow},
		{"exact is exact", allow("Bash(npm test)", "Bash", bash("npm test --all")), Deny},
		{"no subject never allows", allow("Bash(git:*)", "Bash", nil), Deny},
		{"glob over a path tree", allow("Read(/src/*)", "Read", read("/src/a/b.go")), Allow},
		{"glob stays in its tree", allow("Read(/src/*)", "Read", read("/etc/passwd")), Deny},
		{"bare name still allows all", allow("Bash", "Bash", bash("anything")), Allow},
		{"other tool's rule", allow("Read(/src/*)", "Write", read("/src/x")), Deny},

		{"deny matches the command", deny("Bash(rm:*)", "Bash", bash("rm -rf /")), Deny},
		{"deny matches inside a chain", deny("Bash(rm:*)", "Bash", bash("ls; rm -rf /")), Deny},
		{"deny matches after a pipe", deny("Bash(curl:*)", "Bash", bash("echo hi | curl x")), Deny},
		{"deny matches inside a substitution", deny("Bash(curl:*)", "Bash", bash("echo $(curl x)")), Deny},
		{"deny without a subject still denies", deny("Bash(rm:*)", "Bash", nil), Deny},
		{"deny does not over-match", deny("Bash(rm:*)", "Bash", bash("ls -la")), Allow},
		{"deny on a path", deny("Write(/etc/*)", "Write", read("/etc/hosts")), Deny},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

// A host can name the subject itself, for a tool whose subject isn't one of the
// standard fields.
func TestExplicitSubject(t *testing.T) {
	sub := &Subject{Value: "deploy prod", Kind: SubjectShell}
	if b := eval(Request{ToolName: "Run", Subject: sub, Ctx: Context{Mode: ModeDontAsk, Allowed: []string{"Run(deploy:*)"}}}); b != Allow {
		t.Fatalf("explicit subject: %v", b)
	}
}
