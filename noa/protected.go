package noa

import (
	"slices"
	"strings"
)

// IsMessageProtected reports whether a message is HARD-protected: excluded from
// every compression range, and never assigned a ref.
//
// Distinct from the soft protected zone (the most recent N messages / last user
// message), which is a recency heuristic that shifts as the conversation grows.
func IsMessageProtected(m CoreMessage, cfg Config) bool {
	if m.ToolName == "" {
		return false
	}
	if slices.Contains(AlwaysProtectedTools, m.ToolName) {
		return true
	}
	for _, pat := range cfg.ProtectedTools {
		if MatchToolPattern(pat, m.ToolName) {
			return true
		}
	}
	if cfg.IsToolProtected != nil && cfg.IsToolProtected(m.ToolName) {
		return true
	}
	return false
}

// MatchToolPattern matches a tool name against a configured pattern: exact
// (case-insensitive), or a single trailing "*" prefix wildcard.
func MatchToolPattern(pattern, toolName string) bool {
	if pattern == "" || toolName == "" {
		return false
	}
	p, n := strings.ToLower(pattern), strings.ToLower(toolName)
	if prefix, ok := strings.CutSuffix(p, "*"); ok {
		return strings.HasPrefix(n, prefix)
	}
	return p == n
}

// IsNeverPreserveRecentTool reports whether a message's tool is on the BUILT-IN
// list excluded from the "most recent N" protected-zone accounting. Such
// messages remain fully compressible — they simply do not occupy a slot.
//
// It ignores configuration; IsNeverPreserveRecent applies
// Config.NeverPreserveRecentTools and Config.PreserveRecentTools.
func IsNeverPreserveRecentTool(m CoreMessage) bool {
	return IsNeverPreserveRecent(m, Config{})
}

// IsNeverPreserveRecent is IsNeverPreserveRecentTool under a configuration:
// the effective list is Config.NeverPreserveRecentTools (nil = the built-in
// list) minus every entry matched by a Config.PreserveRecentTools pattern.
//
// Ported from acp-kernel (MIT, Copyright © 2026 ranxianglei): src/protected.ts,
// isNeverPreserveRecent.
func IsNeverPreserveRecent(m CoreMessage, cfg Config) bool {
	if m.ToolName == "" {
		return false
	}
	list := NeverPreserveRecentTools
	if cfg.NeverPreserveRecentTools != nil {
		list = cfg.NeverPreserveRecentTools
	}
	for _, entry := range list {
		if preservedByConfig(entry, cfg.PreserveRecentTools) {
			continue
		}
		if MatchToolPattern(entry, m.ToolName) {
			return true
		}
	}
	return false
}

func preservedByConfig(entry string, preserve []string) bool {
	for _, p := range preserve {
		if MatchToolPattern(p, entry) {
			return true
		}
	}
	return false
}

// latestProtectedCallIDs returns, for each Config.ProtectedLatestTools pattern,
// the tool_call id of the LAST call matching it. Pairing by id then protects
// that call's result as well; older instances are left compressible.
//
// A call without an id cannot be paired with its result and is not protected
// here — Norma's tool calls always carry one.
//
// Ported from acp-kernel (MIT, Copyright © 2026 ranxianglei): src/protected.ts,
// collectLatestProtected.
func latestProtectedCallIDs(messages []CoreMessage, cfg Config) map[string]bool {
	ids := map[string]bool{}
	for _, pat := range cfg.ProtectedLatestTools {
		last := ""
		for _, m := range messages {
			if m.ContentType == CTToolCall && m.ToolCallID != "" && MatchToolPattern(pat, m.ToolName) {
				last = m.ToolCallID
			}
		}
		if last != "" {
			ids[last] = true
		}
	}
	return ids
}

// collectProtectedToolCallIDs gathers the tool_call ids of every protected
// exchange — hard-protected tools, plus the newest instance of each
// ProtectedLatestTools pattern — so both halves can be excluded.
func collectProtectedToolCallIDs(messages []CoreMessage, cfg Config) map[string]bool {
	ids := latestProtectedCallIDs(messages, cfg)
	for _, m := range messages {
		if m.ToolCallID != "" && IsMessageProtected(m, cfg) {
			ids[m.ToolCallID] = true
		}
	}
	return ids
}

// isMessageProtectedWithPairing extends hard protection to the other half of a
// protected tool exchange: a tool_result whose tool_call is protected must be
// excluded too, or the request would be rebuilt with an unpaired block. It also
// covers both halves of a latest-protected exchange, via the id set.
func isMessageProtectedWithPairing(m CoreMessage, cfg Config, protectedCallIDs map[string]bool) bool {
	if IsMessageProtected(m, cfg) {
		return true
	}
	return m.ToolCallID != "" && protectedCallIDs[m.ToolCallID]
}
