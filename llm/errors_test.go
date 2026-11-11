package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The code survives wrapping by callers that never heard of it, and the cause
// stays reachable through the chain.
func TestErrorCodeSurvivesWrapping(t *testing.T) {
	cause := io.ErrUnexpectedEOF
	err := fmt.Errorf("outer: %w", WrapError(CodeUnavailable, "fetch", cause))
	if got := CodeOf(err); got != CodeUnavailable {
		t.Fatalf("CodeOf = %q, want %q", got, CodeUnavailable)
	}
	if !IsCode(err, CodeUnavailable) || IsCode(err, CodeAuth) {
		t.Fatal("IsCode does not narrow on the code")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("the cause is not reachable through errors.Is")
	}
	e, ok := AsError(err)
	if !ok || e.Message != "fetch" {
		t.Fatalf("AsError = %+v, %v", e, ok)
	}
	if err.Error() != "outer: fetch: unexpected EOF" {
		t.Fatalf("Error() = %q", err.Error())
	}
}

func TestCodeOfUncodedAndNil(t *testing.T) {
	if CodeOf(nil) != "" || IsCode(nil, CodeUnknown) {
		t.Fatal("nil must carry no code")
	}
	if CodeOf(errors.New("plain")) != CodeUnknown {
		t.Fatal("an error with no code must read as CodeUnknown")
	}
}

// Error() reads like the text the same failure had before codes, so string
// matching and logs are unaffected.
func TestErrorTextForms(t *testing.T) {
	cause := errors.New("cause")
	for _, tc := range []struct {
		err  *Error
		want string
	}{
		{NewError(CodeInvalidConfig, "msg"), "msg"},
		{WrapError(CodeUnknown, "", cause), "cause"},
		{WrapError(CodeUnknown, "msg", cause), "msg: cause"},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}

// A panic with a non-error value becomes CodeUnknown with the value kept as
// the cause; an error value is kept as is; a coded *Error keeps its code.
func TestRecovered(t *testing.T) {
	e := Recovered("boom")
	if e.Code != CodeUnknown || e.Cause == nil || e.Error() != "panic: boom" {
		t.Fatalf("string panic = %+v (%q)", e, e.Error())
	}
	sentinel := errors.New("sentinel")
	if e := Recovered(sentinel); e.Code != CodeUnknown || !errors.Is(e, sentinel) {
		t.Fatalf("error panic lost its cause: %+v", e)
	}
	coded := NewError(CodeAuth, "x")
	if Recovered(coded) != coded {
		t.Fatal("a panic with an *Error must keep it")
	}
}

func TestClassifyStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   Code
	}{
		{400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 215000 tokens > 200000 maximum"}}`, CodeContextOverflow},
		{400, `{"error":{"message":"This model's maximum context length is 128000 tokens","code":"context_length_exceeded"}}`, CodeContextOverflow},
		{413, `request entity too large`, CodeContextOverflow},
		{413, ``, CodeContextOverflow},
		{429, `rate limit`, CodeRateLimited},
		{401, `invalid x-api-key`, CodeAuth},
		{403, `forbidden`, CodeAuth},
		{529, `overloaded`, CodeUnavailable},
		{503, ``, CodeUnavailable},
		{408, ``, CodeUnavailable},
		// A 400 that is not about size must not be mistaken for an overflow:
		// the reactive compaction it would trigger cannot fix it.
		{400, `{"error":{"message":"tools.0.name: String should match pattern"}}`, CodeInvalidRequest},
		{404, `model not found`, CodeInvalidRequest},
		// The words "too long" alone say nothing about the context window.
		{400, `{"error":{"message":"tools.3.custom.name: String should have at most 64 characters, the name is too long"}}`, CodeInvalidRequest},
		{400, `{"error":{"message":"Invalid 'metadata.user_id': string too long. Expected a string with maximum length 64"}}`, CodeInvalidRequest},
		{504, `The request took too long to process`, CodeUnavailable},
		{429, `{"error":{"message":"Request too large for model on tokens per min (TPM): Limit 30000, Requested 41000","code":"rate_limit_exceeded"}}`, CodeRateLimited},
		// The context-length rejections of the providers reachable through
		// the three wire formats.
		{400, `{"type":"error","error":{"type":"invalid_request_error","message":"input length and ` + "`max_tokens`" + ` exceed context limit: 190000 + 20000 > 200000, decrease input length or ` + "`max_tokens`" + ` and try again"}}`, CodeContextOverflow},
		{400, `{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","code":"context_length_exceeded"}}`, CodeContextOverflow},
		{400, `{"error":{"message":"Please reduce the length of the messages or completion.","type":"invalid_request_error"}}`, CodeContextOverflow},
		{400, `{"error":{"code":400,"message":"The input token count (1200000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`, CodeContextOverflow},
		{400, `{"message":"Input is too long for requested model."}`, CodeContextOverflow},
		{400, `{"error":"This model's maximum prompt length is 131072 but the request contains 140000 tokens."}`, CodeContextOverflow},
		{400, `{"error":{"message":"the request exceeds the available context size, try increasing it"}}`, CodeContextOverflow},
		{422, `{"message":"Prompt contains 40000 tokens and 0 draft tokens, too large for model with 32768 maximum context length"}`, CodeContextOverflow},
		{400, `{"error":{"type":"request_too_large","message":"Request exceeds the maximum size"}}`, CodeContextOverflow},
	} {
		if got := classifyStatus(tc.status, tc.body); got != tc.want {
			t.Errorf("classifyStatus(%d, %q) = %q, want %q", tc.status, tc.body, got, tc.want)
		}
	}
}

func TestClassifyMessage(t *testing.T) {
	for msg, want := range map[string]Code{
		"prompt is too long: 215000 tokens > 200000 maximum": CodeContextOverflow,
		"Overloaded": CodeUnavailable,
		"Number of requests exceeds the rate limit": CodeRateLimited,
		"Internal server error":                     CodeUnknown,
	} {
		if got := classifyMessage(msg); got != want {
			t.Errorf("classifyMessage(%q) = %q, want %q", msg, got, want)
		}
	}
}

// Each adapter classifies a context-window rejection at its boundary, over
// HTTP, for both the status path and the in-band error paths.
func TestAdaptersClassifyOverflow(t *testing.T) {
	overflowBody := `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 215000 tokens > 200000 maximum"}}`
	for _, format := range []Format{FormatAnthropic, FormatOpenAI, FormatOpenAIResponses} {
		t.Run(string(format)+"/stream-status", func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(overflowBody))
			}))
			defer srv.Close()
			p, _ := NewProvider(Config{Format: format, BaseURL: srv.URL, APIKey: "k", Model: "m"})
			err := streamErr(p)
			if !IsCode(err, CodeContextOverflow) {
				t.Fatalf("err = %v (code %q), want %q", err, CodeOf(err), CodeContextOverflow)
			}
			if !strings.Contains(err.Error(), "status 400") {
				t.Fatalf("error text changed: %q", err.Error())
			}
		})
		t.Run(string(format)+"/complete-status", func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
			}))
			defer srv.Close()
			p, _ := NewProvider(Config{Format: format, BaseURL: srv.URL, APIKey: "k", Model: "m"})
			_, _, _, err := p.Complete(context.Background(), CompletionRequest{Messages: []Message{UserText("go")}})
			if !IsCode(err, CodeContextOverflow) {
				t.Fatalf("err = %v (code %q)", err, CodeOf(err))
			}
		})
	}
}

// An error the provider reports in-band — an error frame mid-stream or an error
// object on a 200 — is classified from its text.
func TestInBandProviderErrorsAreClassified(t *testing.T) {
	t.Run("anthropic stream frame", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(sse(`event: error`, `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, ``)))
		}))
		defer srv.Close()
		p, _ := NewProvider(Config{Format: FormatAnthropic, BaseURL: srv.URL, APIKey: "k", Model: "m"})
		err := streamErr(p)
		if !IsCode(err, CodeUnavailable) || err.Error() != "anthropic: Overloaded" {
			t.Fatalf("err = %v (code %q)", err, CodeOf(err))
		}
	})
	t.Run("openai error body on 200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens","type":"invalid_request_error"}}`))
		}))
		defer srv.Close()
		p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, APIKey: "k", Model: "m"})
		_, _, _, err := p.Complete(context.Background(), CompletionRequest{Messages: []Message{UserText("go")}})
		if !IsCode(err, CodeContextOverflow) {
			t.Fatalf("err = %v (code %q)", err, CodeOf(err))
		}
	})
}

func TestConfigErrorsCarryCode(t *testing.T) {
	if _, err := NewProvider(Config{Format: "nope"}); !IsCode(err, CodeInvalidConfig) {
		t.Fatalf("unknown format: %v (code %q)", err, CodeOf(err))
	}
	if _, err := NewProvider(Config{Format: FormatOpenAI, Proxy: "ftp://x"}); !IsCode(err, CodeInvalidConfig) {
		t.Fatalf("bad proxy: %v (code %q)", err, CodeOf(err))
	}
}

// ToolError is local metadata: it is persisted with the block, but the
// Anthropic adapter (which marshals blocks directly) must never send it.
func TestToolErrorNeverReachesTheAnthropicWire(t *testing.T) {
	block := ToolResultText("c1", "Tool call was not permitted: denied", true)
	block.ToolError = &ToolError{Name: "Write", Code: "permission_denied"}

	raw, err := json.Marshal(block)
	if err != nil || !strings.Contains(string(raw), `"tool_error":{"name":"Write","code":"permission_denied"}`) {
		t.Fatalf("block JSON = %s (%v)", raw, err)
	}
	var back ContentBlock
	if err := json.Unmarshal(raw, &back); err != nil || back.ToolError == nil || back.ToolError.Code != "permission_denied" {
		t.Fatalf("round trip lost the tool error: %+v (%v)", back, err)
	}

	msgs := []Message{
		{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockToolUse, ID: "c1", Name: "Write", Input: json.RawMessage(`{}`)}}},
		{Role: RoleUser, Content: []ContentBlock{block}},
	}
	p := &anthropicProvider{cfg: Config{Model: "m"}}
	body, err := p.buildBody(CompletionRequest{Messages: msgs}, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "tool_error") {
		t.Fatalf("tool_error leaked onto the wire: %s", body)
	}
	if msgs[1].Content[0].ToolError == nil {
		t.Fatal("encoding mutated the caller's history")
	}
}

func streamErr(p Provider) error {
	for _, err := range p.Stream(context.Background(), CompletionRequest{Messages: []Message{UserText("go")}}) {
		if err != nil {
			return err
		}
	}
	return nil
}

// LooksLikeOverflow is the fallback for errors that carry no code. It must not
// fire on an error that merely contains "too long": that sends the loop into a
// compaction that costs a summary and fixes nothing.
func TestLooksLikeOverflowIsSpecific(t *testing.T) {
	for _, s := range []string{
		"tool name is too long",
		"string too long. Expected a string with maximum length 64",
		"the request took too long to process",
		"stop sequence too long",
		"rate limit reached for requests",
		"overloaded",
	} {
		if LooksLikeOverflow(s) {
			t.Errorf("LooksLikeOverflow(%q) = true", s)
		}
	}
	for _, s := range []string{
		"anthropic: status 400: prompt is too long",
		"openai: status 400: maximum context length is 128000 tokens",
		"custom provider: CONTEXT_LENGTH_EXCEEDED",
		"openai: status 413: payload too large",
		"exceeds the context window",
	} {
		if !LooksLikeOverflow(s) {
			t.Errorf("LooksLikeOverflow(%q) = false", s)
		}
	}
}
