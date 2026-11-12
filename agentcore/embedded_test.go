package agentcore

import (
	"context"
	"encoding/json"
	"iter"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/0xmanhnv/corvid/harness"
	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// hostTool is a tool an embedding host registers: it changes something, so it
// needs approval (the tool.Build default).
func hostTool(ran *atomic.Bool) tool.CoreTool {
	return tool.Build(tool.Spec{
		Name:   "submit_result",
		Schema: map[string]any{"type": "object"},
		Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			ran.Store(true)
			return tool.Text("ok"), nil
		},
	})
}

func scripted(turns ...[]llm.StreamEvent) func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	n := 0
	return func(_ context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		evs := turns[min(n, len(turns)-1)]
		n++
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, e := range evs {
				if !yield(e, nil) {
					return
				}
			}
		}
	}
}

// An Embedded session holds only the tools the host registers: no built-in
// file, shell or search tool, no background-task tools.
func TestEmbeddedHasOnlyHostTools(t *testing.T) {
	s := NewSession(Options{Embedded: true})
	defer s.Close()
	if got := s.registry.List(); len(got) != 0 {
		names := []string{}
		for _, tl := range got {
			names = append(names, tl.Name())
		}
		t.Fatalf("Embedded session with no Tools holds %v", names)
	}
	if s.tasks != nil {
		t.Fatal("Embedded session started a background-task manager")
	}

	var ran atomic.Bool
	s2 := NewSession(Options{Embedded: true, Tools: []tool.CoreTool{hostTool(&ran)}})
	defer s2.Close()
	if got := s2.registry.List(); len(got) != 1 || got[0].Name() != "submit_result" {
		t.Fatalf("Embedded session holds %d tools, want only submit_result", len(got))
	}
	if s2.opts.MaxTurns != EmbeddedMaxTurns {
		t.Fatalf("MaxTurns = %d, want the embedded default %d", s2.opts.MaxTurns, EmbeddedMaxTurns)
	}

	// The default profile is unchanged.
	d := NewSession(Options{})
	defer d.Close()
	if _, ok := d.registry.Get(tool.BashName); !ok {
		t.Fatal("the default profile lost its default tools")
	}
}

// bypassPermissions is never in effect in an Embedded session: a call the
// host did not allow is denied, and a built-in tool the model names is unknown.
func TestEmbeddedRefusesBypass(t *testing.T) {
	var ran atomic.Bool
	var warns []string
	s := NewSession(Options{
		Embedded:       true,
		Tools:          []tool.CoreTool{hostTool(&ran)},
		PermissionMode: permission.ModeBypass,
		OnWarn:         func(m string) { warns = append(warns, m) },
		Deps: harness.QueryDeps{CallModel: scripted(
			toolTurn("c1", "submit_result", `{}`),
			toolTurn("c2", tool.BashName, `{"command":"env"}`),
			textTurn("done"),
		)},
	})
	defer s.Close()
	var results []llm.ContentBlock
	for ev, err := range s.Prompt(context.Background(), "go") {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == harness.KindToolResult {
			results = append(results, *ev.ToolResult)
		}
	}
	if ran.Load() {
		t.Fatal("a tool that needs approval ran in an Embedded session asking for bypass")
	}
	if len(results) != 2 || results[0].ToolError == nil || results[0].ToolError.Code != tool.CodePermissionDenied {
		t.Fatalf("results = %+v", results)
	}
	if results[1].ToolError == nil || results[1].ToolError.Code != tool.CodeUnknownTool {
		t.Fatalf("Bash in an Embedded session: %+v", results[1])
	}
	if len(warns) == 0 || !strings.Contains(warns[0], "bypassPermissions") {
		t.Fatalf("no warning about bypass: %v", warns)
	}

	// An allow rule still lets the host's own tool run.
	ran.Store(false)
	s2 := NewSession(Options{
		Embedded:     true,
		Tools:        []tool.CoreTool{hostTool(&ran)},
		AllowedTools: []string{"submit_result"},
		Deps:         harness.QueryDeps{CallModel: scripted(toolTurn("c1", "submit_result", `{}`), textTurn("done"))},
	})
	defer s2.Close()
	for range s2.Prompt(context.Background(), "go") {
	}
	if !ran.Load() {
		t.Fatal("an allowed host tool did not run")
	}

	// A mode switched to bypass at run time (plan mode's target) reads as dontAsk.
	f := embeddedMode(permission.ModeDefault, func() permission.Mode { return permission.ModeBypass })
	if f() != permission.ModeDontAsk {
		t.Fatalf("embedded mode func returned %s", f())
	}
}

// A host that imports agentcore does not link fold or foldadapter (their
// attribution term applies only where they are linked), nor the multi-agent
// add-ons it did not import.
func TestAgentcoreImportGraph(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not on PATH")
	}
	out, err := exec.Command(gobin, "list", "-deps", "github.com/0xmanhnv/corvid/agentcore").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	for _, pkg := range strings.Fields(string(out)) {
		for _, banned := range []string{"/fold", "/foldadapter", "/coordinator", "/subagent", "/mcp"} {
			if strings.HasSuffix(pkg, "github.com/0xmanhnv/corvid"+banned) {
				t.Errorf("agentcore links %s", pkg)
			}
		}
	}
}

// An Embedded session always runs the strict policy: a looser one given is
// replaced (and reported), so a deferred tool is checked under its own name
// and a shell or memory write asks, whatever the host set.
func TestEmbeddedPolicyIsAlwaysStrict(t *testing.T) {
	var warns []string
	s := NewSession(Options{Embedded: true, Policy: permission.CompatiblePolicy(),
		OnWarn: func(m string) { warns = append(warns, m) }})
	defer s.Close()
	if s.opts.Policy != permission.StrictPolicy() {
		t.Fatalf("Embedded policy = %+v, want StrictPolicy", s.opts.Policy)
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w, "always permission.StrictPolicy()") {
			found = true
		}
		if strings.HasPrefix(w, "agentcore: permission.Policy.") {
			t.Fatalf("an Embedded session reported a loosened setting: %q", w)
		}
	}
	if !found {
		t.Fatalf("no warning that the looser policy was ignored: %v", warns)
	}
	// Set nothing: strict, silently.
	warns = nil
	s2 := NewSession(Options{Embedded: true, OnWarn: func(m string) { warns = append(warns, m) }})
	defer s2.Close()
	if s2.opts.Policy != permission.StrictPolicy() || len(warns) != 0 {
		t.Fatalf("policy %+v, warnings %v", s2.opts.Policy, warns)
	}
}
