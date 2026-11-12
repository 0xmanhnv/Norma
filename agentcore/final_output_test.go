package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/0xmanhnv/corvid/harness"
	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/tool"
)

// forecastSchema is the final answer of a weather assistant.
const forecastSchema = `{
	"type": "object",
	"required": ["city", "high_c"],
	"properties": {
		"city":   {"type": "string"},
		"high_c": {"type": "number"},
		"sky":    {"enum": ["clear", "cloudy", "rain"]}
	}
}`

func finalTerminal(t *testing.T, s *Session) *harness.Terminal {
	t.Helper()
	var term *harness.Terminal
	for ev, err := range s.Prompt(context.Background(), "forecast for Springfield") {
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if ev.Kind == harness.KindResult {
			term = ev.Terminal
		}
	}
	if term == nil {
		t.Fatal("no result event")
	}
	return term
}

// The final answer is checked in the default and in the embedded profile
// alike: valid on the first try, valid after one repair, invalid twice.
func TestFinalOutputSession(t *testing.T) {
	const good = `{"city":"Springfield","high_c":21.5,"sky":"clear"}`
	const wrongType = `{"city":"Springfield","high_c":"warm"}`
	const badEnum = `{"city":"Springfield","high_c":21.5,"sky":"meatballs"}`
	for _, embedded := range []bool{false, true} {
		run := func(turns ...[]llm.StreamEvent) *harness.Terminal {
			s := NewSession(Options{
				Embedded:               embedded,
				Tools:                  []tool.CoreTool{},
				DisableBackgroundTasks: true,
				FinalOutput:            &FinalOutput{Schema: json.RawMessage(forecastSchema)},
				Deps:                   harness.QueryDeps{CallModel: scripted(turns...)},
			})
			defer s.Close()
			if err := s.Err(); err != nil {
				t.Fatalf("Err = %v", err)
			}
			return finalTerminal(t, s)
		}
		name := fmt.Sprintf("embedded=%v", embedded)

		term := run(textTurn(good))
		if term.Reason != harness.ReasonCompleted || string(term.Output) != good {
			t.Fatalf("%s first try: %s, Output %s", name, term.Reason, term.Output)
		}

		term = run(textTurn(wrongType), textTurn(good))
		if term.Reason != harness.ReasonCompleted || string(term.Output) != good {
			t.Fatalf("%s after repair: %s, Output %s", name, term.Reason, term.Output)
		}

		term = run(textTurn(wrongType), textTurn(badEnum))
		if term.Reason != harness.ReasonInvalidOutput {
			t.Fatalf("%s invalid twice: reason %s", name, term.Reason)
		}
		var oe *harness.OutputError
		if !errors.As(term.Err, &oe) || oe.Repairs != 1 {
			t.Fatalf("%s invalid twice: Err %v", name, term.Err)
		}
		if !llm.IsCode(term.Err, tool.CodeInvalidOutput) {
			t.Fatalf("%s invalid twice: code %q", name, llm.CodeOf(term.Err))
		}
	}
}

// Run returns the typed error when the answer never becomes valid.
func TestFinalOutputRunError(t *testing.T) {
	_, err := Run(context.Background(), Options{
		Embedded:    true,
		FinalOutput: &FinalOutput{Schema: json.RawMessage(forecastSchema), MaxRepairs: -1},
		Deps:        harness.QueryDeps{CallModel: scripted(textTurn("It will be sunny."))},
	}, "forecast")
	var oe *harness.OutputError
	if !errors.As(err, &oe) || oe.Repairs != 0 {
		t.Fatalf("Run err = %v, want an *harness.OutputError", err)
	}
}

// A schema that is not a JSON object makes the session unusable: Err says so,
// OnWarn is told, and Prompt fails before any model call.
func TestFinalOutputSchemaMustBeObject(t *testing.T) {
	for _, schema := range []string{``, `null`, `[]`, `"object"`, `{"type":`} {
		var warned []string
		var calls atomic.Int32
		s := NewSession(Options{
			Embedded:    true,
			OnWarn:      func(w string) { warned = append(warned, w) },
			FinalOutput: &FinalOutput{Schema: json.RawMessage(schema)},
			Deps: harness.QueryDeps{CallModel: func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
				calls.Add(1)
				return scripted(textTurn(`{}`))(ctx, req)
			}},
		})
		if !llm.IsCode(s.Err(), llm.CodeInvalidConfig) {
			t.Fatalf("schema %q: Err = %v, want %s", schema, s.Err(), llm.CodeInvalidConfig)
		}
		if len(warned) != 1 {
			t.Fatalf("schema %q: warnings = %q", schema, warned)
		}
		var got error
		for _, err := range s.Prompt(context.Background(), "forecast") {
			got = err
		}
		if !llm.IsCode(got, llm.CodeInvalidConfig) || calls.Load() != 0 || len(s.Messages()) != 0 {
			t.Fatalf("schema %q: Prompt err = %v, model calls = %d, messages = %d", schema, got, calls.Load(), len(s.Messages()))
		}
		s.Close()
	}
}

// Sessions built from the same Options run at once without sharing anything
// mutable (go test -race).
func TestFinalOutputConcurrentSessions(t *testing.T) {
	fo := &FinalOutput{Schema: json.RawMessage(forecastSchema), MaxRepairs: 2}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			good := fmt.Sprintf(`{"city":"City %d","high_c":%d}`, i, 10+i)
			s := NewSession(Options{
				Embedded:    true,
				FinalOutput: fo,
				Deps:        harness.QueryDeps{CallModel: scripted(textTurn(`{"city":1}`), textTurn(good))},
			})
			defer s.Close()
			for ev, err := range s.Prompt(context.Background(), "forecast") {
				if err != nil {
					errs <- err
					return
				}
				if ev.Kind == harness.KindResult && (ev.Terminal.Reason != harness.ReasonCompleted || string(ev.Terminal.Output) != good) {
					errs <- fmt.Errorf("session %d: %s, Output %s", i, ev.Terminal.Reason, ev.Terminal.Output)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
