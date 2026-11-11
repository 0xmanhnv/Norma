package subagent

import (
	"context"
	"iter"
	"sync"
	"testing"

	"github.com/0xmanhnv/corvid/harness"
	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/tool"
)

// recording is scripted plus a log of the requests it was sent.
type recording struct {
	scripted
	rmu  sync.Mutex
	reqs []llm.CompletionRequest
}

func (r *recording) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	r.rmu.Lock()
	r.reqs = append(r.reqs, req)
	r.rmu.Unlock()
	return r.scripted.Stream(ctx, req)
}

// A subagent started from a tool call is sent with the calling run's cache
// lifetime, thinking mode and effort. Before, it was sent with none of them
// (and, having no boundary, without prompt caching at all).
func TestSubagentInheritsModelSettings(t *testing.T) {
	want := llm.ModelSettings{CacheTTL: "1h", ConversationCacheTTL: "1h", Thinking: "adaptive", ReasoningEffort: "low"}
	child := &recording{scripted: scripted{turns: [][]llm.StreamEvent{textTurn("child done")}}}
	agentTool := NewTool(Config{Provider: child, ParentTools: tool.NewRegistry(tool.DefaultTools()...),
		Agents: map[string]Definition{"w": {Prompt: []string{"worker prompt"}}}})
	parent := harness.QueryInput{
		Provider:      &scripted{turns: [][]llm.StreamEvent{toolTurn("p1", "Agent", `{"agent_type":"w","prompt":"go"}`), textTurn("parent done")}},
		Messages:      []llm.Message{llm.UserText("start")},
		Tools:         tool.NewRegistry(append(tool.DefaultTools(), agentTool)...),
		WorkingDir:    t.TempDir(),
		ModelSettings: want,
	}
	for _, err := range harness.Query(context.Background(), parent, harness.QueryDeps{}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(child.reqs) != 1 {
		t.Fatalf("child made %d requests, want 1", len(child.reqs))
	}
	if got := llm.SettingsOf(child.reqs[0]); got != want {
		t.Fatalf("child request settings %+v, want the parent's %+v", got, want)
	}
	if child.reqs[0].DisableCache {
		t.Fatal("child request has prompt caching off")
	}
}

// Settings on the subagent config win over the calling run's, and are what a
// subagent started with Run (no calling run) is sent with.
func TestSubagentOwnModelSettings(t *testing.T) {
	own := llm.ModelSettings{ReasoningEffort: "high"}
	child := &recording{scripted: scripted{turns: [][]llm.StreamEvent{textTurn("done")}}}
	cfg := Config{Provider: child, ParentTools: tool.NewRegistry(), ModelSettings: own,
		Agents: map[string]Definition{"w": {Prompt: []string{"p"}}}}
	if _, err := Run(context.Background(), cfg, "w", "go", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got := llm.SettingsOf(child.reqs[0]); got != own {
		t.Fatalf("settings %+v, want %+v", got, own)
	}
}
