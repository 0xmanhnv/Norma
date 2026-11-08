package llm

import (
	"encoding/json"
	"testing"
)

// A request's ReasoningEffort overrides the configured one in every adapter
// that sends an effort; an empty one keeps the configured value.
func TestRequestReasoningEffortOverride(t *testing.T) {
	msgs := []Message{UserText("hi")}
	type probe struct {
		name  string
		build func(cfg Config, req CompletionRequest) []byte
		read  func(body []byte) string
	}
	field := func(path ...string) func([]byte) string {
		return func(b []byte) string {
			var v any
			_ = json.Unmarshal(b, &v)
			for _, k := range path {
				m, _ := v.(map[string]any)
				v = m[k]
			}
			s, _ := v.(string)
			return s
		}
	}
	probes := []probe{
		{"openai", func(c Config, r CompletionRequest) []byte {
			b, _ := (&openaiProvider{cfg: c}).buildBody(r, false)
			return b
		}, field("reasoning_effort")},
		{"anthropic", func(c Config, r CompletionRequest) []byte {
			b, _ := (&anthropicProvider{cfg: c}).buildBody(r, false)
			return b
		}, field("output_config", "effort")},
		{"responses", func(c Config, r CompletionRequest) []byte {
			b, _ := (&openaiResponsesProvider{cfg: c}).buildBody(r, false)
			return b
		}, field("reasoning", "effort")},
	}
	for _, p := range probes {
		cfg := Config{Model: "m", ReasoningEffort: "medium"}
		if got := p.read(p.build(cfg, CompletionRequest{Messages: msgs})); got != "medium" {
			t.Errorf("%s: configured effort %q", p.name, got)
		}
		if got := p.read(p.build(cfg, CompletionRequest{Messages: msgs, ReasoningEffort: "low"})); got != "low" {
			t.Errorf("%s: override %q", p.name, got)
		}
		if got := p.read(p.build(Config{Model: "m"}, CompletionRequest{Messages: msgs})); got != "" {
			t.Errorf("%s: no effort configured, sent %q", p.name, got)
		}
	}
}
