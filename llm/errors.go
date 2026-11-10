package llm

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Code is a stable, machine-routable error class. Branch on a Code, never on
// an error's text: messages change and vary by provider and language, codes do
// not. Codes are lowercase snake_case. The ones corvid assigns are declared
// here and next to the packages that raise them (tool.CodeInvalidInput, …);
// a host defining its own should prefix them so they cannot collide.
type Code string

const (
	// CodeUnknown classifies a failure nothing recognised: a plain error from a
	// tool or a host callback, or a recovered panic. CodeOf returns it for any
	// non-nil error that carries no code.
	CodeUnknown Code = "unknown"

	// CodeContextOverflow is a request the provider rejected because the
	// prompt does not fit the model's context window.
	CodeContextOverflow Code = "context_overflow"
	// CodeRateLimited is a provider rejection for request or token rate.
	CodeRateLimited Code = "rate_limited"
	// CodeAuth is a provider rejection of the credentials (401/403).
	CodeAuth Code = "auth"
	// CodeUnavailable is a provider that could not serve the request: a
	// network failure, a timeout, an overload or a 5xx.
	CodeUnavailable Code = "unavailable"
	// CodeInvalidRequest is any other provider rejection of the request.
	CodeInvalidRequest Code = "invalid_request"

	// CodeInvalidConfig is a configuration the SDK cannot run with.
	CodeInvalidConfig Code = "invalid_config"
	// CodeInvalidArgument is an API call with an argument that cannot be used.
	CodeInvalidArgument Code = "invalid_argument"
)

// Error is corvid's base error: a stable Code for routing, a Message for
// people, and an optional Cause kept for errors.Is / errors.As.
//
// Error() reads exactly like the message the same failure produced before
// codes existed, so logs and string checks keep working; only the code is
// new. Narrow with CodeOf / IsCode, or errors.As(err, &e) with e *Error.
type Error struct {
	Code    Code
	Message string
	Cause   error
}

// Error renders "Message: Cause", or whichever of the two is set.
func (e *Error) Error() string {
	switch {
	case e.Cause == nil:
		return e.Message
	case e.Message == "":
		return e.Cause.Error()
	default:
		return e.Message + ": " + e.Cause.Error()
	}
}

// Unwrap returns the cause, so the chain stays visible to errors.Is/As.
func (e *Error) Unwrap() error { return e.Cause }

// NewError builds an Error with no cause.
func NewError(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// WrapError builds an Error around cause. message may be empty, in which case
// the error reads as the cause does.
func WrapError(code Code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}

// AsError returns the first *Error in err's chain.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// CodeOf returns the code of the first *Error in err's chain: "" for nil,
// CodeUnknown for an error that carries none.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	if e, ok := AsError(err); ok && e.Code != "" {
		return e.Code
	}
	return CodeUnknown
}

// IsCode reports whether err's chain carries code.
func IsCode(err error, code Code) bool {
	return err != nil && CodeOf(err) == code
}

// Recovered turns a recovered panic value into an Error. A panic with an
// *Error keeps it; anything else becomes CodeUnknown with the value as the
// cause (an error value as is, other values formatted), so nothing is lost.
func Recovered(v any) *Error {
	switch x := v.(type) {
	case *Error:
		return x
	case error:
		return WrapError(CodeUnknown, "panic", x)
	default:
		return WrapError(CodeUnknown, "panic", fmt.Errorf("%v", v))
	}
}

// ToolError is the machine-readable side of a failed tool call, carried on its
// tool_result block (ContentBlock.ToolError). It is persisted with the
// conversation and delivered on the tool_result event, so retry and policy
// code can branch on it, but it is never sent to a model: the model reads the
// result's text, which is unchanged.
type ToolError struct {
	// Name is the tool that failed. A tool_result block carries only the id of
	// its call, so this saves a reader the lookup.
	Name string `json:"name"`
	Code Code   `json:"code"`
}

// overflowMarkers are phrasings providers use when a prompt does not fit the
// context window. Matched case-insensitively. This list is the single owner of
// text-based overflow recognition: the adapters classify provider errors with
// it, and compaction.IsOverflow falls back to it for errors no adapter
// classified.
var overflowMarkers = []string{
	"too long",
	"context length",
	"context_length",
	"maximum context",
	"status 413",
}

// LooksLikeOverflow reports whether text reads as a context-window rejection.
// It is the fallback for errors that carry no code; prefer
// IsCode(err, CodeContextOverflow).
func LooksLikeOverflow(text string) bool {
	s := strings.ToLower(text)
	for _, m := range overflowMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// classifyStatus classifies a non-200 provider response by its status and
// body. Overflow wins over the status class: providers reject an oversized
// prompt with 400, 413 or 422 depending on who you ask.
func classifyStatus(status int, body string) Code {
	if status == http.StatusRequestEntityTooLarge || LooksLikeOverflow(body) {
		return CodeContextOverflow
	}
	switch {
	case status == http.StatusTooManyRequests:
		return CodeRateLimited
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return CodeAuth
	case status == http.StatusRequestTimeout || status >= 500:
		return CodeUnavailable
	case status >= 400:
		return CodeInvalidRequest
	}
	return CodeUnknown
}

// classifyMessage classifies an error a provider reported inside a response
// (an error body on a 200, or an error frame mid-stream), where only text is
// available.
func classifyMessage(msg string) Code {
	s := strings.ToLower(msg)
	switch {
	case LooksLikeOverflow(s):
		return CodeContextOverflow
	case strings.Contains(s, "rate limit") || strings.Contains(s, "rate_limit"):
		return CodeRateLimited
	case strings.Contains(s, "overloaded"):
		return CodeUnavailable
	}
	return CodeUnknown
}

// providerError builds the error an adapter returns for a failure the provider
// reported in-band.
func providerError(prefix, msg string) *Error {
	return NewError(classifyMessage(msg), prefix+": "+msg)
}
