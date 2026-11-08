package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
)

type anthropicProvider struct{ cfg Config }

type anthropicSystemBlock struct {
	Type         string         `json:"type"`
	Text         string         `json:"text"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

type anthropicReq struct {
	Model         string                 `json:"model"`
	System        []anthropicSystemBlock `json:"system,omitempty"`
	Messages      []json.RawMessage      `json:"messages"`
	Tools         []anthropicTool        `json:"tools,omitempty"`
	MaxTokens     int                    `json:"max_tokens"`
	Temperature   *float64               `json:"temperature,omitempty"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
	Thinking      *anthropicThinking     `json:"thinking,omitempty"`
	OutputConfig  *anthropicOutputConfig `json:"output_config,omitempty"`
	Stream        bool                   `json:"stream"`
}

// anthropicThinking is the "thinking":{"type":"enabled"|"disabled"} switch.
type anthropicThinking struct {
	Type string `json:"type"`
}

// anthropicOutputConfig carries the reasoning strength via
// "output_config":{"effort":"low"|"medium"|"high"|"max"}.
type anthropicOutputConfig struct {
	Effort string `json:"effort"`
}

type anthropicTool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

func (p *anthropicProvider) buildBody(req CompletionRequest, stream bool) ([]byte, error) {
	maxTok := req.MaxTokens
	if maxTok == 0 {
		maxTok = 8192
	}
	// Effective thinking mode: a non-empty per-request override (req.Thinking)
	// wins over Config.ThinkingType. Thinking-type content blocks may be replayed
	// only when thinking is actively "enabled"; any other mode (incl. "disabled")
	// strips them so Anthropic does not reject the request.
	thinkingType := p.cfg.ThinkingType
	thinkingEnabled := thinkingType != ""
	if req.Thinking != "" {
		thinkingType = req.Thinking
		thinkingEnabled = req.Thinking == "enabled"
	}
	msgs, err := anthropicMessages(mergeAdjacentSameRole(filterThinkingBlocks(req.Messages, thinkingEnabled)),
		req.DynamicBoundary > 0, req.VolatileTail)
	if err != nil {
		return nil, err
	}
	body := anthropicReq{
		Model:         p.cfg.Model,
		Messages:      msgs,
		MaxTokens:     maxTok,
		Temperature:   req.Temperature,
		StopSequences: req.Stop,
		Stream:        stream,
		System:        buildAnthropicSystem(req),
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	// Tools render before the system prompt, so a breakpoint on the last tool
	// caches the tool list on its own: a request whose system prompt differs
	// (another role, an edited prompt) still reads the tools from the cache.
	// With the system and the two message breakpoints that makes four, the most
	// Anthropic accepts.
	if req.DynamicBoundary > 0 && len(body.Tools) > 0 {
		body.Tools[len(body.Tools)-1].CacheControl = stableCacheControl(req)
	}
	if thinkingType != "" {
		body.Thinking = &anthropicThinking{Type: thinkingType}
	}
	if effort := p.cfg.effort(req); effort != "" {
		body.OutputConfig = &anthropicOutputConfig{Effort: effort}
	}
	return json.Marshal(body)
}

// anthropicMessages encodes the conversation. With caching on (the host set a
// DynamicBoundary) it also marks two breakpoints, so a long tool loop reads its
// history from the cache instead of paying full price for it every turn:
//
//   - the tail: the last block before the volatile tail (working frame), i.e.
//     everything this request adds that the next one will send again;
//   - the previous tail: the end of the user message before the latest
//     assistant turn, which is where the previous request put its tail. Marking
//     it again guarantees the read, whatever the provider's lookback window.
//
// Thinking blocks can't carry a breakpoint; the mark moves to the block before.
// Together with the tools and system breakpoints this stays within Anthropic's
// limit of four.
func anthropicMessages(msgs []Message, cache bool, volatile int) ([]json.RawMessage, error) {
	type pos struct{ m, b int }
	var marks []pos
	if cache {
		cacheable := func(p pos) bool {
			t := msgs[p.m].Content[p.b].Type
			return t != BlockThinking
		}
		// stableBefore walks back from the block before (m, b), skipping skip
		// blocks and anything that can't carry a mark.
		stableBefore := func(m, b, skip int) (pos, bool) {
			for m >= 0 {
				for b--; b >= 0; b-- {
					p := pos{m, b}
					if skip > 0 {
						skip--
						continue
					}
					if cacheable(p) {
						return p, true
					}
				}
				m--
				if m >= 0 {
					b = len(msgs[m].Content)
				}
			}
			return pos{}, false
		}
		if n := len(msgs); n > 0 {
			if p, ok := stableBefore(n-1, len(msgs[n-1].Content), volatile); ok {
				marks = append(marks, p)
			}
			for a := n - 1; a > 0; a-- {
				if msgs[a].Role == RoleAssistant {
					if msgs[a-1].Role == RoleUser {
						if p, ok := stableBefore(a-1, len(msgs[a-1].Content), 0); ok && (len(marks) == 0 || p != marks[0]) {
							marks = append(marks, p)
						}
					}
					break
				}
			}
		}
	}
	out := make([]json.RawMessage, len(msgs))
	for i, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		var marked bool
		for _, p := range marks {
			marked = marked || p.m == i
		}
		if marked {
			var raw struct {
				Role    json.RawMessage   `json:"role"`
				Content []json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(b, &raw); err != nil {
				return nil, err
			}
			for _, p := range marks {
				if p.m == i {
					raw.Content[p.b] = withCacheControl(raw.Content[p.b])
				}
			}
			if b, err = json.Marshal(raw); err != nil {
				return nil, err
			}
		}
		out[i] = b
	}
	return out, nil
}

// withCacheControl adds "cache_control":{"type":"ephemeral"} to an encoded block,
// leaving its other bytes as they were.
func withCacheControl(block json.RawMessage) json.RawMessage {
	s := bytes.TrimSpace(block)
	if len(s) < 2 || s[len(s)-1] != '}' {
		return block
	}
	out := append([]byte{}, s[:len(s)-1]...)
	if len(bytes.TrimSpace(s[1:len(s)-1])) > 0 {
		out = append(out, ',')
	}
	return append(out, []byte(`"cache_control":{"type":"ephemeral"}}`)...)
}

// stableCacheControl is the marker of the tools and system breakpoints: the
// request's CacheTTL when it names one Anthropic takes, else the default. They
// come before the message breakpoints, so a longer lifetime here is allowed.
func stableCacheControl(req CompletionRequest) map[string]any {
	cc := map[string]any{"type": "ephemeral"}
	switch req.CacheTTL {
	case "1h", "5m":
		cc["ttl"] = req.CacheTTL
	}
	return cc
}

// buildAnthropicSystem splits the system prompt at DynamicBoundary, placing a
// cache breakpoint on the static prefix (FR-02.2).
func buildAnthropicSystem(req CompletionRequest) []anthropicSystemBlock {
	if len(req.System) == 0 {
		return nil
	}
	b := req.DynamicBoundary
	if b <= 0 {
		// No boundary set → no cache breakpoint.
		return []anthropicSystemBlock{{Type: "text", Text: joinSystem(req.System)}}
	}
	if b >= len(req.System) {
		// Boundary at/after the end → the whole system prompt is static; cache it
		// all in one block (an all-static prompt should still be cacheable).
		return []anthropicSystemBlock{{Type: "text", Text: joinSystem(req.System), CacheControl: stableCacheControl(req)}}
	}
	return []anthropicSystemBlock{
		{Type: "text", Text: joinSystem(req.System[:b]), CacheControl: stableCacheControl(req)},
		{Type: "text", Text: joinSystem(req.System[b:])},
	}
}

func (p *anthropicProvider) Stream(ctx context.Context, req CompletionRequest) iter.Seq2[StreamEvent, error] {
	return func(yield func(StreamEvent, error) bool) {
		body, err := p.buildBody(req, true)
		if err != nil {
			yield(StreamEvent{}, err)
			return
		}
		resp, err := doStream(ctx, p.cfg, p.cfg.BaseURL+"/v1/messages", body, func(r *http.Request) {
			r.Header.Set("content-type", "application/json")
			r.Header.Set("x-api-key", p.cfg.APIKey)
			r.Header.Set("anthropic-version", p.cfg.APIVersion)
			r.Header.Set("accept", "text/event-stream")
		}, "anthropic")
		if err != nil {
			yield(StreamEvent{}, err)
			return
		}
		defer resp.Body.Close()
		scan := newSSEScanner(resp.Body)
		for {
			_, data, serr := scan.next()
			if serr == io.EOF {
				return
			}
			if serr != nil {
				yield(StreamEvent{}, serr)
				return
			}
			if data == "" {
				continue
			}
			ev, ok, perr := parseAnthropicFrame(data)
			if perr != nil {
				yield(StreamEvent{}, perr)
				return
			}
			if !ok {
				continue
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// Complete performs a real non-streaming completion: it sends stream:false and
// parses the single JSON response from /v1/messages into a full assistant
// Message, returning it with the stop reason and normalized usage.
func (p *anthropicProvider) Complete(ctx context.Context, req CompletionRequest) (Message, string, Usage, error) {
	body, err := p.buildBody(req, false)
	if err != nil {
		return Message{}, "", Usage{}, err
	}
	resp, err := doStream(ctx, p.cfg, p.cfg.BaseURL+"/v1/messages", body, func(r *http.Request) {
		r.Header.Set("content-type", "application/json")
		r.Header.Set("x-api-key", p.cfg.APIKey)
		r.Header.Set("anthropic-version", p.cfg.APIVersion)
		r.Header.Set("accept", "application/json")
	}, "anthropic")
	if err != nil {
		return Message{}, "", Usage{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Message{}, "", Usage{}, err
	}
	return parseAnthropicResponse(raw)
}

// parseAnthropicResponse decodes a non-streaming Messages API response into the
// neutral Message model. Anthropic already speaks the content-block shape, so
// text/thinking/tool_use blocks map across directly.
func parseAnthropicResponse(raw []byte) (Message, string, Usage, error) {
	var r struct {
		Type    string `json:"type"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      *anthropicUsage `json:"usage"`
		Error      *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return Message{}, "", Usage{}, fmt.Errorf("anthropic: decode response: %w (body: %s)", err, truncate(string(raw), 500))
	}
	if r.Error != nil || r.Type == "error" {
		msg := "error response"
		if r.Error != nil {
			msg = r.Error.Message
		}
		return Message{}, "", Usage{}, fmt.Errorf("anthropic: %s", msg)
	}
	var usage Usage
	if r.Usage != nil {
		usage = r.Usage.norm()
	}
	var blocks []ContentBlock
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				blocks = append(blocks, TextBlock(b.Text))
			}
		case "thinking":
			blocks = append(blocks, ContentBlock{Type: BlockThinking, Thinking: b.Thinking, Signature: b.Signature})
		case "tool_use":
			in := b.Input
			if len(in) == 0 {
				in = json.RawMessage("{}")
			}
			blocks = append(blocks, ContentBlock{Type: BlockToolUse, ID: b.ID, Name: b.Name, Input: in})
		}
	}
	return Message{Role: RoleAssistant, Content: blocks}, r.StopReason, usage, nil
}

func parseAnthropicFrame(data string) (StreamEvent, bool, error) {
	var f struct {
		Type         string `json:"type"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage   *anthropicUsage `json:"usage"`
		Message struct {
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return StreamEvent{}, false, nil // tolerate non-JSON keepalives
	}
	switch f.Type {
	case "message_start":
		if f.Message.Usage != nil {
			// Anthropic reports the full input side (input + cache read/creation) in
			// BOTH message_start and message_delta — the two ECHO it rather than
			// partition it. Both events therefore carry the input side, and
			// Usage.MergeEcho (not Add) is what keeps it from doubling: it takes the
			// max per echoed field and accumulates only output. Do NOT "fix" this by
			// making message_delta output-only again — Anthropic-compatible
			// emulations report zeros here and the real input counts only in
			// message_delta, and dropping the delta's input side meters them at 0.
			//
			// The initial output_tokens here is a placeholder (≈1), so drop it: it is
			// not an echo of anything and MergeEcho would add it to the real count.
			u := f.Message.Usage.norm()
			u.OutputTokens = 0
			return StreamEvent{Type: SEMessageStart, Usage: u}, true, nil
		}
	case "content_block_start":
		if f.ContentBlock.Type == "tool_use" {
			return StreamEvent{Type: SEToolUseStart, ToolID: f.ContentBlock.ID, ToolName: f.ContentBlock.Name}, true, nil
		}
	case "content_block_delta":
		switch f.Delta.Type {
		case "text_delta":
			return StreamEvent{Type: SETextDelta, Text: f.Delta.Text}, true, nil
		case "thinking_delta":
			return StreamEvent{Type: SEThinkingDelta, Text: f.Delta.Thinking}, true, nil
		case "signature_delta":
			return StreamEvent{Type: SEThinkingSignature, Text: f.Delta.Signature}, true, nil
		case "input_json_delta":
			return StreamEvent{Type: SEToolInputJSON, Text: f.Delta.PartialJSON}, true, nil
		}
	case "message_delta":
		ev := StreamEvent{Type: SEMessageDelta, StopReason: f.Delta.StopReason}
		if f.Usage != nil {
			// Output plus the input side. Real Anthropic re-echoes the input counts
			// from message_start (the accumulator deduplicates echoed fields with
			// max, so nothing doubles); Anthropic-compatible emulations that report
			// zeros in message_start get their real input counts recovered here.
			ev.Usage = f.Usage.norm()
		}
		return ev, true, nil
	case "message_stop":
		return StreamEvent{Type: SEMessageStop}, true, nil
	case "error":
		msg := "stream error"
		if f.Error != nil {
			msg = f.Error.Message
		}
		return StreamEvent{}, false, fmt.Errorf("anthropic: %s", msg)
	}
	return StreamEvent{}, false, nil
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// norm maps Anthropic usage to the canonical Usage. InputTokens is the TOTAL input
// INCLUDING cached tokens (OpenAI-style: prompt_tokens semantics) — Anthropic's raw
// input_tokens counts only the non-cached portion, so cache read+creation are folded
// in. CacheReadTokens/CacheWriteTokens are the cached SUBSET (⊆ InputTokens).
func (u anthropicUsage) norm() Usage {
	return Usage{
		InputTokens:      u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
}

// filterThinkingBlocks normalizes message history before it is sent to the
// Anthropic Messages API. It strips thinking-type content blocks when thinking
// is disabled (Anthropic rejects replayed thinking blocks without the thinking
// parameter enabled), and drops any message left with no content blocks —
// Anthropic rejects an empty content array. An empty message arrives here two
// ways: a thinking-only turn that collapses to empty once thinking is stripped,
// or an empty/truncated assistant turn (only reasoning, or nothing) that was
// recorded with no text or tool_use. (A thinking-only turn WITH thinking
// enabled keeps its thinking block — that is valid content and must be
// replayed.)
//
// It also clears ContentBlock.ThoughtSignature, which belongs to Gemini's
// OpenAI-compatible endpoint. anthropicReq.Messages is []Message, so
// ContentBlock is marshaled straight onto the Anthropic wire and any populated
// field becomes a key inside a tool_use block — Anthropic answers an unknown key
// with "Extra inputs are not permitted". That history is reachable: transcripts
// persist llm.Message verbatim, so a session recorded against Gemini and then
// resumed (or summarized, or handed to a subagent) with an Anthropic provider
// would fail on every turn.
func filterThinkingBlocks(msgs []Message, thinkingEnabled bool) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		content := m.Content
		if !thinkingEnabled {
			filtered := make([]ContentBlock, 0, len(content))
			for _, b := range content {
				if b.Type != BlockThinking {
					filtered = append(filtered, b)
				}
			}
			content = filtered
		}
		if len(content) == 0 {
			continue // empty content array is rejected by Anthropic — drop it
		}
		content = stripThoughtSignatures(content)
		if len(content) == len(m.Content) && sameBacking(content, m.Content) {
			out = append(out, m)
		} else {
			out = append(out, Message{Role: m.Role, Content: content})
		}
	}
	return out
}

// stripThoughtSignatures clears ThoughtSignature on every block, copying only
// when at least one is set so untouched history keeps sharing its backing array.
// Callers own the returned slice; the input is never mutated — it belongs to the
// caller's conversation state and must survive the request unchanged.
func stripThoughtSignatures(content []ContentBlock) []ContentBlock {
	idx := -1
	for i, b := range content {
		if b.ThoughtSignature != "" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return content
	}
	out := make([]ContentBlock, len(content))
	copy(out, content)
	for i := idx; i < len(out); i++ {
		out[i].ThoughtSignature = ""
	}
	return out
}

// sameBacking reports whether two slices share their first element's address,
// i.e. whether a normalization step above returned the input untouched.
func sameBacking(a, b []ContentBlock) bool {
	return len(a) == 0 || len(b) == 0 || &a[0] == &b[0]
}

// mergeAdjacentSameRole coalesces consecutive same-role messages into one by
// concatenating their content blocks. Anthropic requires messages to alternate
// user/assistant; dropping an empty turn in filterThinkingBlocks can leave two
// adjacent user messages (e.g. a tool result followed by a resume nudge, once
// the empty assistant between them is gone), which the API would reject with
// "roles must alternate". Merging preserves alternation without losing content.
// A fresh content slice is built on merge so the caller's message backing
// arrays are never mutated.
func mergeAdjacentSameRole(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if n := len(out); n > 0 && out[n-1].Role == m.Role {
			merged := make([]ContentBlock, 0, len(out[n-1].Content)+len(m.Content))
			merged = append(merged, out[n-1].Content...)
			merged = append(merged, m.Content...)
			out[n-1].Content = merged
			continue
		}
		out = append(out, m)
	}
	return out
}
