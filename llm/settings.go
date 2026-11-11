package llm

// ModelSettings are the request settings that belong to a whole session rather
// than to one call: how long the prompt cache lives, and how the model reasons.
//
// They are per session on purpose. A provider's prompt cache is keyed on the
// request up to each breakpoint, and the thinking mode and effort are part of
// that key: changing either between two requests of one conversation makes the
// second one pay for the whole history again. Pick them when the session
// starts and leave them alone.
type ModelSettings struct {
	// CacheTTL is the lifetime of the stable prefix (tools and static system
	// prompt): "" or "5m" for the provider default of five minutes, "1h" for
	// one hour. See CompletionRequest.CacheTTL.
	CacheTTL string
	// ConversationCacheTTL is the lifetime of the conversation breakpoints,
	// with the same values. "1h" also moves the stable prefix to one hour.
	// See CompletionRequest.ConversationCacheTTL.
	ConversationCacheTTL string
	// DisableCache sends every request without prompt-cache markers.
	DisableCache bool
	// Thinking is the thinking mode of every request ("" inherits the
	// provider's Config.ThinkingType). See CompletionRequest.Thinking.
	Thinking string
	// ReasoningEffort is the reasoning effort of every request ("" inherits
	// the provider's Config.ReasoningEffort).
	ReasoningEffort string
	// MaxOutputTokens is the model's declared output limit: the most tokens one
	// response may hold. The agent loop uses it as the output cap when the
	// host names none, never asks for more, and raises a truncated turn's cap
	// to it and no further. 0 means unknown: the loop then sends no cap of its
	// own and the provider adapter applies its default. It is read by the
	// loop, not written onto a request by Apply.
	MaxOutputTokens int
}

// Apply writes the settings onto req. Fields the settings leave empty keep
// whatever req already carries.
func (s ModelSettings) Apply(req *CompletionRequest) {
	if s.CacheTTL != "" {
		req.CacheTTL = s.CacheTTL
	}
	if s.ConversationCacheTTL != "" {
		req.ConversationCacheTTL = s.ConversationCacheTTL
	}
	if s.DisableCache {
		req.DisableCache = true
	}
	if s.Thinking != "" {
		req.Thinking = s.Thinking
	}
	if s.ReasoningEffort != "" {
		req.ReasoningEffort = s.ReasoningEffort
	}
}

// SettingsOf reads back the session-level settings a request carries, so a
// call made on the side (a summary, a child agent) can be sent the same way.
func SettingsOf(req CompletionRequest) ModelSettings {
	return ModelSettings{
		CacheTTL:             req.CacheTTL,
		ConversationCacheTTL: req.ConversationCacheTTL,
		DisableCache:         req.DisableCache,
		Thinking:             req.Thinking,
		ReasoningEffort:      req.ReasoningEffort,
	}
}

// thinkingField is the thinking type an adapter puts on the wire for req, ""
// for none. A request that asks for "disabled" against a provider configured
// with no thinking type gets no field at all: that endpoint was never told it
// takes one, several OpenAI-compatible gateways reject it, and newer Claude
// models reject "disabled" outright. Leaving the field out is the portable way
// to not ask for thinking.
func (c Config) thinkingField(req CompletionRequest) string {
	switch {
	case req.Thinking == "":
		return c.ThinkingType
	case req.Thinking == "disabled" && c.ThinkingType == "":
		return ""
	default:
		return req.Thinking
	}
}

// cacheTTLs resolves the lifetimes of the stable and the conversation
// breakpoints of req: "" (provider default), "5m" or "1h". Unknown values are
// dropped. One hour on the conversation forces one hour on the stable prefix,
// which renders before it: a provider rejects a longer lifetime after a
// shorter one.
func cacheTTLs(req CompletionRequest) (stable, conversation string) {
	norm := func(v string) string {
		switch v {
		case "1h", "5m":
			return v
		}
		return ""
	}
	stable, conversation = norm(req.CacheTTL), norm(req.ConversationCacheTTL)
	if conversation == "1h" {
		stable = "1h"
	}
	return stable, conversation
}

// cacheControl is the marker for a breakpoint with the given lifetime.
func cacheControl(ttl string) map[string]any {
	cc := map[string]any{"type": "ephemeral"}
	if ttl != "" {
		cc["ttl"] = ttl
	}
	return cc
}

// staticBoundary is how many leading system segments form the static, cacheable
// prefix of req: DynamicBoundary when it names a real split, otherwise all of
// them.
func staticBoundary(req CompletionRequest) int {
	if b := req.DynamicBoundary; b > 0 && b < len(req.System) {
		return b
	}
	return len(req.System)
}
