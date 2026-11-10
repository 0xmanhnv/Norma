package coordinator

import (
	"context"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// echoProvider answers each worker with text derived from the task's last user
// message, so we can verify distinct workers ran.
type echoProvider struct{ count int32 }

func (p *echoProvider) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	atomic.AddInt32(&p.count, 1)
	last := ""
	if n := len(req.Messages); n > 0 {
		last = req.Messages[n-1].Text()
	}
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "did: " + last}, nil)
		yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func (p *echoProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return accumulateStream(ctx, p, req)
}

// accumulateStream drains a provider's own Stream to satisfy the non-streaming
// Complete method for test mocks.
func accumulateStream(ctx context.Context, p llm.Provider, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	acc := llm.NewAccumulator()
	for ev, err := range p.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		acc.Add(ev)
	}
	return acc.Message(), acc.StopReason, acc.Usage, nil
}

func TestRunParallelFansOut(t *testing.T) {
	prov := &echoProvider{}
	c := &Config{
		Provider:       prov,
		WorkerTools:    tool.NewRegistry(tool.DefaultTools()...),
		PermissionMode: permission.ModeBypass,
	}
	tasks := []string{"task A", "task B", "task C"}
	results := c.RunParallel(context.Background(), tasks)
	if len(results) != 3 {
		t.Fatalf("results=%d", len(results))
	}
	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("task %d err: %v", i, r.Err)
		}
		if !strings.Contains(r.Output, tasks[i]) {
			t.Fatalf("result %d mismatched: task=%q output=%q", i, tasks[i], r.Output)
		}
	}
	if atomic.LoadInt32(&prov.count) != 3 {
		t.Fatalf("expected 3 worker runs, got %d", prov.count)
	}
}

func TestAgentToolAndSystemSegment(t *testing.T) {
	c := &Config{
		Provider:    &echoProvider{},
		WorkerTools: tool.NewRegistry(tool.NewRead(), tool.NewBash()),
	}
	at := c.AgentTool()
	if at.Name() != "Agent" {
		t.Fatalf("agent tool name=%q", at.Name())
	}
	seg := c.SystemSegment()
	if !strings.Contains(seg, "Coordinator mode") || !strings.Contains(seg, "Read") || !strings.Contains(seg, "Bash") {
		t.Fatalf("system segment missing pieces:\n%s", seg)
	}
	// The Agent tool actually dispatches a worker.
	res, _ := at.Call(context.Background(), []byte(`{"agent_type":"worker","prompt":"find bugs"}`), &tool.ToolContext{})
	if !strings.Contains(res.Flatten(), "find bugs") {
		t.Fatalf("worker dispatch: %q", res.Flatten())
	}
}

// failProvider yields a fixed provider error on every call and counts calls,
// so a test can see how many workers actually reached the model.
type failProvider struct {
	count int32
	code  llm.Code
}

func (p *failProvider) Stream(_ context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	atomic.AddInt32(&p.count, 1)
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{}, llm.WrapError(p.code, "boom", nil))
	}
}

func (p *failProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return accumulateStream(ctx, p, req)
}

func TestRunParallelOnResult(t *testing.T) {
	prov := &echoProvider{}
	var mu sync.Mutex
	seen := map[int]string{}
	c := &Config{
		Provider:       prov,
		WorkerTools:    tool.NewRegistry(tool.DefaultTools()...),
		PermissionMode: permission.ModeBypass,
		OnResult: func(i int, r Result) {
			mu.Lock()
			seen[i] = r.Output
			mu.Unlock()
		},
	}
	tasks := []string{"alpha", "beta", "gamma", "delta"}
	results := c.RunParallel(context.Background(), tasks)
	if len(seen) != len(tasks) {
		t.Fatalf("OnResult fired for %d tasks, want %d", len(seen), len(tasks))
	}
	for i, task := range tasks {
		if !strings.Contains(seen[i], task) {
			t.Fatalf("OnResult[%d]=%q missing task %q", i, seen[i], task)
		}
		if seen[i] != results[i].Output {
			t.Fatalf("OnResult[%d]=%q disagrees with results[%d]=%q", i, seen[i], i, results[i].Output)
		}
	}
}

func TestRunParallelStopOnFatal(t *testing.T) {
	tasks := []string{"1", "2", "3", "4", "5", "6"}

	// Without the breaker, every task reaches the (failing) model.
	base := &failProvider{code: llm.CodeAuth}
	c := &Config{Provider: base, WorkerTools: tool.NewRegistry(), PermissionMode: permission.ModeBypass, MaxParallel: 1}
	res := c.RunParallel(context.Background(), tasks)
	if got := atomic.LoadInt32(&base.count); got != int32(len(tasks)) {
		t.Fatalf("without StopOnFatal: %d workers ran, want %d", got, len(tasks))
	}
	for i, r := range res {
		if r.Err == nil {
			t.Fatalf("result %d: want error", i)
		}
	}

	// With the breaker, a fatal auth failure stops the remaining launches.
	brk := &failProvider{code: llm.CodeAuth}
	c = &Config{Provider: brk, WorkerTools: tool.NewRegistry(), PermissionMode: permission.ModeBypass, MaxParallel: 1, StopOnFatal: true}
	res = c.RunParallel(context.Background(), tasks)
	if got := atomic.LoadInt32(&brk.count); got >= int32(len(tasks)) {
		t.Fatalf("with StopOnFatal: %d workers ran, want fewer than %d", got, len(tasks))
	}
	for i, r := range res {
		if r.Err == nil {
			t.Fatalf("result %d: want error (auth or canceled)", i)
		}
	}
}

func TestRunParallelTransientNotFatal(t *testing.T) {
	// A transient code must NOT trip the breaker: all workers still run.
	prov := &failProvider{code: llm.CodeUnavailable}
	tasks := []string{"1", "2", "3", "4"}
	c := &Config{Provider: prov, WorkerTools: tool.NewRegistry(), PermissionMode: permission.ModeBypass, MaxParallel: 1, StopOnFatal: true}
	c.RunParallel(context.Background(), tasks)
	if got := atomic.LoadInt32(&prov.count); got != int32(len(tasks)) {
		t.Fatalf("transient failure tripped the breaker: %d workers ran, want %d", got, len(tasks))
	}
}
