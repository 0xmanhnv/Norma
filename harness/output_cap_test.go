package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/0xmanhnv/corvid/llm"
)

// capEndpoint is a model endpoint of either wire format that records the
// output cap of every request. Its first response is cut by the output limit;
// the ones after it finish normally.
type capEndpoint struct {
	format llm.Format
	mu     sync.Mutex
	caps   []any // the cap field of each request body; nil when the field is absent
	srv    *httptest.Server
}

func newCapEndpoint(t *testing.T, format llm.Format) *capEndpoint {
	e := &capEndpoint{format: format}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		e.mu.Lock()
		e.caps = append(e.caps, req["max_tokens"])
		first := len(e.caps) == 1
		e.mu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		if format == llm.FormatAnthropic {
			stop := "end_turn"
			if first {
				stop = "max_tokens"
			}
			for _, fr := range []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"part"}}`,
				fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":3}}`, stop),
				`{"type":"message_stop"}`,
			} {
				fmt.Fprintf(w, "data: %s\n\n", fr)
			}
			return
		}
		finish := "stop"
		if first {
			finish = "length"
		}
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"part"}}]}`)
		fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(`{"choices":[{"delta":{},"finish_reason":%q}]}`, finish))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *capEndpoint) provider(t *testing.T) llm.Provider {
	p, err := llm.NewProvider(llm.Config{Format: e.format, BaseURL: e.srv.URL, APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The output cap comes from configuration, not from constants in the loop:
//
//   - with nothing known the loop sends no cap and the adapter applies its
//     default (8192 on the Anthropic format, which requires the field; no
//     field at all on the OpenAI format, so the provider's own default holds);
//   - the model's declared limit (ModelSettings.MaxOutputTokens) is the cap
//     when the host names none, and the ceiling when it names more;
//   - a truncated turn is retried with the cap raised to that limit and no
//     further, where it used to be raised to 64000 whatever the model.
func TestOutputCapFollowsTheModelsDeclaredLimit(t *testing.T) {
	cases := []struct {
		name      string
		maxTokens int
		limit     int
		escalate  bool
		want      map[llm.Format][]any // per request
	}{
		{"nothing known: the adapter default", 0, 0, false, map[llm.Format][]any{
			llm.FormatAnthropic: {8192.0, 8192.0}, llm.FormatOpenAI: {nil, nil}}},
		{"declared limit, no explicit cap", 0, 16000, false, map[llm.Format][]any{
			llm.FormatAnthropic: {16000.0, 16000.0}, llm.FormatOpenAI: {16000.0, 16000.0}}},
		{"explicit cap under the limit", 4000, 16000, false, map[llm.Format][]any{
			llm.FormatAnthropic: {4000.0, 4000.0}, llm.FormatOpenAI: {4000.0, 4000.0}}},
		{"explicit cap over the limit is lowered", 50000, 16000, false, map[llm.Format][]any{
			llm.FormatAnthropic: {16000.0, 16000.0}, llm.FormatOpenAI: {16000.0, 16000.0}}},
		{"escalation stops at the limit", 4000, 16000, true, map[llm.Format][]any{
			llm.FormatAnthropic: {4000.0, 16000.0}, llm.FormatOpenAI: {4000.0, 16000.0}}},
		{"escalation with the limit unknown", 4000, 0, true, map[llm.Format][]any{
			llm.FormatAnthropic: {4000.0, 64000.0}, llm.FormatOpenAI: {4000.0, 64000.0}}},
		{"already at the limit: nothing to escalate to", 0, 16000, true, map[llm.Format][]any{
			llm.FormatAnthropic: {16000.0, 16000.0}, llm.FormatOpenAI: {16000.0, 16000.0}}},
	}
	for _, c := range cases {
		for _, format := range []llm.Format{llm.FormatAnthropic, llm.FormatOpenAI} {
			t.Run(c.name+"/"+string(format), func(t *testing.T) {
				e := newCapEndpoint(t, format)
				in := QueryInput{
					Provider:          e.provider(t),
					Messages:          []llm.Message{llm.UserText("go")},
					MaxTokens:         c.maxTokens,
					ModelSettings:     llm.ModelSettings{MaxOutputTokens: c.limit},
					EscalateMaxTokens: c.escalate,
				}
				var term *Terminal
				for ev, err := range Query(context.Background(), in, QueryDeps{}) {
					if err != nil {
						t.Fatal(err)
					}
					if ev.Kind == KindResult {
						term = ev.Terminal
					}
				}
				if term == nil || term.Reason != ReasonCompleted {
					t.Fatalf("run did not complete: %+v", term)
				}
				want := c.want[format]
				e.mu.Lock()
				defer e.mu.Unlock()
				if len(e.caps) < len(want) {
					t.Fatalf("caps sent = %v, want %v", e.caps, want)
				}
				if fmt.Sprint(e.caps[:len(want)]) != fmt.Sprint(want) {
					t.Fatalf("caps sent = %v, want %v", e.caps[:len(want)], want)
				}
			})
		}
	}
}
