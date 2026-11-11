// Package llm defines a vendor-neutral message model and a pluggable Provider
// interface, with adapters that speak the Anthropic Messages API, the OpenAI
// Chat Completions API and the OpenAI Responses API (FormatAnthropic,
// FormatOpenAI, FormatOpenAIResponses). The internal model mirrors Anthropic's
// content-block shape (the richer superset, including thinking and structured
// tool results); the OpenAI adapters translate at their boundary. This lets the
// harness be written once against a single representation while remaining
// compatible with any model reachable through one of these wire formats.
// Failures carry a stable Code (see Error, CodeOf, IsCode).
package llm

import "encoding/json"

// Role identifies the author of a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockType enumerates the kinds of content a message block may carry.
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockThinking   BlockType = "thinking"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
	// BlockRedactedThinking is reasoning the provider returned encrypted (Data).
	// It has no readable text; it is kept and replayed unchanged, because the
	// provider expects the turn's reasoning blocks back exactly as it sent them.
	BlockRedactedThinking BlockType = "redacted_thinking"
)

// ContentBlock is one piece of message content. Which fields are populated
// depends on Type. This is the Anthropic content-block union.
type ContentBlock struct {
	Type BlockType `json:"type"`

	// Text block.
	Text string `json:"text,omitempty"`

	// Thinking block (extended thinking). Anthropic requires the field to be
	// named "thinking" (not "text") and requires "signature" when replaying
	// the block in multi-turn history. The text may be empty (a model can be
	// configured to omit it and return the signature alone); the block is still
	// kept, and MarshalJSON writes the "thinking" key for it regardless.
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// Data is the opaque payload of a redacted_thinking block.
	Data string `json:"data,omitempty"`

	// ToolUse: a model request to invoke a tool.
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// ThoughtSignature is Gemini's opaque per-tool-call token, present on a
	// tool_use block only when the model was reached through Gemini's
	// OpenAI-compatible endpoint (generativelanguage.googleapis.com/.../openai).
	// A Gemini thinking model requires this value to be echoed back verbatim on
	// the same tool call when the turn is replayed in multi-turn history;
	// otherwise the API rejects the request with "Function call is missing a
	// thought_signature in functionCall parts."
	// See https://ai.google.dev/gemini-api/docs/thinking#signatures.
	//
	// It is provider-scoped state that outlives the provider that issued it:
	// transcripts persist llm.Message verbatim, so a session recorded against
	// Gemini can be resumed, summarized or sub-agented with a DIFFERENT provider
	// configured. Two consequences, handled in opposite ways:
	//
	//   - Anthropic: ContentBlock is marshaled straight into anthropicReq, so a
	//     populated field would become an unknown key inside a tool_use block
	//     and draw "Extra inputs are not permitted". filterThinkingBlocks
	//     clears it on the way out; this field must never reach that wire.
	//   - Other OpenAI-compatible gateways: toOpenAIMessages replays it
	//     unconditionally, because the alternative — sniffing the base URL for
	//     Google's host — silently drops the signature for anyone reaching
	//     Gemini through a proxy or Vertex-style path, re-breaking the exact
	//     rejection this field exists to prevent. Over-sending costs a 400 only
	//     on a gateway that both forbids unknown fields AND is being fed a
	//     history recorded elsewhere; that fails loudly and is recoverable,
	//     whereas a dropped signature fails on every Gemini turn.
	ThoughtSignature string `json:"thought_signature,omitempty"`

	// ToolResult: the outcome of a tool, fed back to the model.
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   []ContentBlock `json:"content,omitempty"`
	IsError   bool           `json:"is_error,omitempty"`
	// ToolError classifies a failed tool_result for hosts, hooks and the
	// transcript. It is local metadata: no adapter sends it to a provider (the
	// Anthropic adapter, which marshals blocks directly, clears it), and the
	// model sees only Content. Nil on success and on errors nothing classified.
	ToolError *ToolError `json:"tool_error,omitempty"`
}

// MarshalJSON encodes the block. It differs from the default encoding in one
// case: a thinking block always carries its "thinking" key, even when the text
// is empty. A thinking block without that key is malformed on the wire, and a
// block with a signature and no text is a legitimate one to replay.
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	if b.Type == BlockThinking && b.Thinking == "" {
		return json.Marshal(struct {
			Type      BlockType `json:"type"`
			Thinking  string    `json:"thinking"`
			Signature string    `json:"signature,omitempty"`
		}{b.Type, "", b.Signature})
	}
	type plain ContentBlock // drops the method, so this does not recurse
	return json.Marshal(plain(b))
}

// TextBlock builds a text content block.
func TextBlock(s string) ContentBlock { return ContentBlock{Type: BlockText, Text: s} }

// ToolResultText builds a tool_result block carrying a single text block.
func ToolResultText(toolUseID, text string, isError bool) ContentBlock {
	return ContentBlock{
		Type:      BlockToolResult,
		ToolUseID: toolUseID,
		Content:   []ContentBlock{TextBlock(text)},
		IsError:   isError,
	}
}

// Message is a single conversation turn.
type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
}

// UserText builds a user message with one text block.
func UserText(s string) Message {
	return Message{Role: RoleUser, Content: []ContentBlock{TextBlock(s)}}
}

// ToolUses returns the tool_use blocks of a message.
func (m Message) ToolUses() []ContentBlock {
	var out []ContentBlock
	for _, b := range m.Content {
		if b.Type == BlockToolUse {
			out = append(out, b)
		}
	}
	return out
}

// Text concatenates all text blocks of a message.
func (m Message) Text() string {
	var s string
	for _, b := range m.Content {
		if b.Type == BlockText {
			s += b.Text
		}
	}
	return s
}

// Usage reports token accounting, including prompt-cache tokens (FR-03.8).
//
// InputTokens is the total input of the request, cached or not. CacheReadTokens
// (served from the cache) and CacheWriteTokens (written to it) are parts of
// that total, not additions to it: the full-price input is
// InputTokens - CacheReadTokens - CacheWriteTokens. Every adapter normalizes to
// this, so do not add the cache counts to InputTokens when pricing a request.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// Add accumulates token counts from another Usage.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
}

// MergeEcho folds a usage report that may re-echo fields already counted by an
// earlier event (Anthropic repeats the input side in message_start and
// message_delta): echoed input/cache fields take the max instead of summing,
// while output keeps accumulating. Emulations that report zeros in
// message_start and real counts only in message_delta are thereby recovered
// instead of lost.
func (u *Usage) MergeEcho(o Usage) {
	u.OutputTokens += o.OutputTokens
	if o.InputTokens > u.InputTokens {
		u.InputTokens = o.InputTokens
	}
	if o.CacheReadTokens > u.CacheReadTokens {
		u.CacheReadTokens = o.CacheReadTokens
	}
	if o.CacheWriteTokens > u.CacheWriteTokens {
		u.CacheWriteTokens = o.CacheWriteTokens
	}
}

// ToolSchema is a tool definition exposed to the model.
type ToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// CompletionRequest is a single, provider-neutral model invocation.
type CompletionRequest struct {
	// System is the host-controlled system prompt, in segments. The SDK injects
	// no presets (design decision 1). Segments are joined with blank lines.
	System []string
	// DynamicBoundary is the index in System separating cacheable static prefix
	// from per-request dynamic content; providers set a cache breakpoint there
	// (FR-02.2). 0 (or a value past the end) treats the whole system prompt as
	// static. Caching itself is on unless DisableCache is set.
	DynamicBoundary int
	// DisableCache sends the request without any prompt-cache marker. Use it
	// for a one-off call whose prefix no later request repeats (a summary made
	// by a separate model): a marker there buys a cache write nobody reads.
	DisableCache bool
	// VolatileTail is how many content blocks at the very end of Messages change
	// on every request (a working frame, a per-request reminder). Providers that
	// cache the conversation place the cache breakpoint before them, since a
	// prefix ending in content that is never sent again can never be read back.
	VolatileTail int
	// ReasoningEffort overrides Config.ReasoningEffort for THIS request when
	// non-empty, so callers that share one provider can ask for more or less
	// reasoning per call (a planner at "high", a summarizer at "low"). It is
	// sent the same way as the configured value.
	ReasoningEffort string
	// CacheTTL is how long a provider that takes a cache lifetime keeps the
	// stable part of the prompt cached: the tool list and the static system
	// prefix. "" keeps the provider's default (five minutes on Anthropic);
	// "1h" asks for one hour, worth it when requests sharing that prefix
	// start more than five minutes apart (a cache write costs more at one
	// hour). The conversation breakpoints follow ConversationCacheTTL.
	CacheTTL string
	// ConversationCacheTTL is the lifetime of the conversation breakpoints. ""
	// keeps the provider's default (five minutes); "1h" keeps the history
	// cached across gaps of up to an hour between requests (a slow tool, a
	// person answering). A longer lifetime may come before a shorter one but
	// not after it, so "1h" here puts the stable prefix on one hour as well.
	ConversationCacheTTL string

	Messages    []Message
	Tools       []ToolSchema
	MaxTokens   int
	Temperature *float64
	Stop        []string
	// Thinking overrides the provider's thinking mode for THIS request only:
	// "" inherits Config.ThinkingType; any other value is that mode. An adapter
	// only puts a thinking field on the wire when the endpoint is known to take
	// one: "disabled" against a provider configured with no ThinkingType sends
	// nothing, which is how such an endpoint is asked not to think.
	Thinking string
}
