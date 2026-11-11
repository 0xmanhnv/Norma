package coordinator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
	"github.com/0xmanhnv/corvid/transcript"
)

// With a transcript store, every worker's conversation is recorded to a
// sidechain file and its Result says where, so what the worker did can be read
// after the fact. The workers of one call share a session. ParseOutput turns
// the final text into a typed value; a parse error is carried, not fatal.
func TestRunParallelRecordsWorkerTranscripts(t *testing.T) {
	store := transcript.NewStore(t.TempDir())
	c := &Config{
		Provider:       &echoProvider{},
		WorkerTools:    tool.NewRegistry(tool.DefaultTools()...),
		PermissionMode: permission.ModeBypass,
		Transcript:     store,
		ParseOutput: func(text string) (any, error) {
			if strings.Contains(text, "bad") {
				return nil, errors.New("unparseable")
			}
			return len(text), nil
		},
	}
	tasks := []string{"task A", "task bad"}
	results := c.RunParallel(context.Background(), tasks)
	if len(results) != 2 {
		t.Fatalf("results = %d", len(results))
	}
	for i, r := range results {
		s := r.Summary
		if r.Err != nil || s.SessionID == "" || s.AgentID == "" || s.TranscriptPath == "" {
			t.Fatalf("result %d: err=%v summary=%+v", i, r.Err, s)
		}
		if s.SessionID != results[0].Summary.SessionID {
			t.Fatalf("workers of one call are filed under different sessions: %q, %q", s.SessionID, results[0].Summary.SessionID)
		}
		if s.TranscriptPath != store.AgentPath(s.SessionID, s.AgentID) {
			t.Fatalf("result %d: path %q is not the store's path for its ids", i, s.TranscriptPath)
		}
		recs, err := store.Load(s.TranscriptPath)
		if err != nil {
			t.Fatal(err)
		}
		msgs, _ := transcript.Messages(recs)
		if len(msgs) < 2 || !strings.Contains(msgs[0].Text(), tasks[i]) || msgs[len(msgs)-1].Text() != r.Output {
			t.Fatalf("result %d: the transcript does not hold the worker's conversation: %+v", i, msgs)
		}
	}
	if results[0].Summary.Output != len(results[0].Output) || results[0].Summary.OutputErr != nil {
		t.Fatalf("typed output = %v (%v), want %d", results[0].Summary.Output, results[0].Summary.OutputErr, len(results[0].Output))
	}
	if results[1].Summary.Output != nil || results[1].Summary.OutputErr == nil || results[1].Err != nil {
		t.Fatalf("a parse error must be carried on the summary, not fail the task: %+v", results[1])
	}
}

// Without a store nothing is recorded and the locator stays empty.
func TestRunParallelNoTranscriptNoLocator(t *testing.T) {
	c := &Config{
		Provider:       &echoProvider{},
		WorkerTools:    tool.NewRegistry(tool.DefaultTools()...),
		PermissionMode: permission.ModeBypass,
	}
	r := c.RunParallel(context.Background(), []string{"x"})[0]
	if r.Err != nil || r.Summary.TranscriptPath != "" || r.Summary.SessionID != "" || r.Summary.AgentType != "worker" {
		t.Fatalf("result = %+v", r)
	}
}
