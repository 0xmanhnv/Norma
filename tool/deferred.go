package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/0xmanhnv/corvid/llm"
	"github.com/0xmanhnv/corvid/permission"
)

// Deferred tools: a mechanism to keep a tool callable while withholding its schema
// from the model's tool list (saving context tokens). Only the tool NAME surfaces
// (via RenderDeferredToolsBlock, placed by the host in the cached system prompt).
// The model discovers a deferred tool's schema with SearchExtraTools, then invokes
// it with ExecuteExtraTool. Both access tools stay in the model's list (never
// deferred).

// Fixed names of the two always-loaded access tools.
const (
	SearchExtraToolsName = "SearchExtraTools"
	ExecuteExtraToolName = "ExecuteExtraTool"
)

// UnlockSet is a session-scoped, concurrency-safe set of deferred-tool names that
// are currently callable via ExecuteExtraTool. Globally-available deferred tools
// are seeded at construction; skill-gated ones are added at runtime when their
// skill loads. Withholding the schema (deferred) is separate from gating the call
// (unlock): a name may be listed yet locked until its skill is invoked.
type UnlockSet struct {
	mu    sync.RWMutex
	names map[string]bool
}

// NewUnlockSet builds an unlock set seeded with the given tool names.
func NewUnlockSet(initial ...string) *UnlockSet {
	s := &UnlockSet{names: make(map[string]bool, len(initial))}
	for _, n := range initial {
		s.names[n] = true
	}
	return s
}

// Add unlocks the given tool names (idempotent).
func (s *UnlockSet) Add(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range names {
		s.names[n] = true
	}
}

// Has reports whether name is currently unlocked (callable).
func (s *UnlockSet) Has(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.names[name]
}

// List returns the unlocked names, sorted.
func (s *UnlockSet) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.names))
	for n := range s.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// RenderDeferredToolsBlock renders the <available-deferred-tools> block: the given
// tool names (one per line, sorted) plus the fixed usage instruction. Returns "" if
// names is empty. Hosts place this in the CACHED system-prompt prefix (a SystemPrompt
// segment before DynamicBoundary) — see design doc §2.1 / C1.
func RenderDeferredToolsBlock(names []string) string {
	if len(names) == 0 {
		return ""
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	var b strings.Builder
	b.WriteString("<available-deferred-tools>\n")
	for _, n := range sorted {
		b.WriteString(n)
		b.WriteByte('\n')
	}
	b.WriteString("</available-deferred-tools>\n")
	b.WriteString("IMPORTANT: The tools listed above are deferred — they are NOT in your tool list. ")
	b.WriteString("To use one you MUST first discover it via " + SearchExtraToolsName +
		" (returns its schema), then invoke it via " + ExecuteExtraToolName + ".\n")
	b.WriteString("Tool priority: use core tools for core tasks (e.g. bash/curl for HTTP); " +
		"only use " + ExecuteExtraToolName + " for a deferred tool when the task truly needs it.")
	return b.String()
}

// NewSearchExtraTools builds the always-loaded discovery tool. deferred is the set
// of tool names whose schemas are withheld; the tool searches those names in reg
// and returns each match's name + description + params schema.
func NewSearchExtraTools(reg *Registry, deferred []string) CoreTool {
	deferredSet := make(map[string]bool, len(deferred))
	for _, n := range deferred {
		deferredSet[n] = true
	}
	ordered := append([]string(nil), deferred...)
	sort.Strings(ordered)
	return Build(Spec{
		Name: SearchExtraToolsName,
		Description: "Discover deferred tools (not in your tool list) and get their schemas, then invoke via " +
			ExecuteExtraToolName + ". Query forms: \"select:Name\" exact, \"select:A,B\" multiple, or free keywords to match name/description.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "\"select:<name>\" exact (comma-separated for multiple), or keywords.",
				},
			},
			"required": []any{"query"},
		},
		ReadOnly: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(_ context.Context, in json.RawMessage, _ *ToolContext) (Result, error) {
			var args struct {
				Query string `json:"query"`
			}
			_ = json.Unmarshal(in, &args)
			q := strings.TrimSpace(args.Query)
			if q == "" {
				return Errorf("Error: query is required"), nil
			}
			var matches []CoreTool
			seen := map[string]bool{}
			addByFullName := func(name string) {
				if seen[name] || !deferredSet[name] {
					return
				}
				if t, ok := reg.Get(name); ok {
					seen[name] = true
					matches = append(matches, t)
				}
			}
			// addByQuery resolves one query token: exact full-name first, then
			// case-insensitive suffix match against each deferred tool name so that
			// short MCP tool names like "list_projects" also find
			// "mcp__scopesentry__list_projects".
			addByQuery := func(token string) {
				token = strings.TrimSpace(token)
				if token == "" {
					return
				}
				// 1. exact match
				addByFullName(token)
				if seen[token] {
					return
				}
				// 2. suffix / substring match (case-insensitive) for short names
				lower := strings.ToLower(token)
				for _, name := range ordered {
					lname := strings.ToLower(name)
					// match if full name equals, ends with __token, or contains token
					if lname == lower ||
						strings.HasSuffix(lname, "__"+lower) ||
						strings.Contains(lname, lower) {
						addByFullName(name)
					}
				}
			}
			if strings.HasPrefix(q, "select:") {
				for _, part := range strings.Split(strings.TrimPrefix(q, "select:"), ",") {
					addByQuery(part)
				}
			} else {
				// Multi-token keyword search: every space-separated token must appear
				// in the normalised haystack (__ replaced by space for MCP tool names).
				tokens := strings.Fields(strings.ToLower(q))
				for _, name := range ordered {
					t, ok := reg.Get(name)
					if !ok {
						continue
					}
					// normalise __ → space so "scopesentry list_projects" finds
					// "mcp__scopesentry__list_projects".
					haystack := strings.ReplaceAll(
						strings.ToLower(t.Name()+" "+t.Description()), "__", " ")
					allMatch := true
					for _, tok := range tokens {
						if !strings.Contains(haystack, tok) {
							allMatch = false
							break
						}
					}
					if allMatch {
						addByFullName(name)
					}
				}
			}
			if len(matches) == 0 {
				return Text("No matching deferred tools for query: " + q), nil
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Found %d deferred tool(s). Invoke with %s({\"tool_name\":\"<name>\",\"params\":{...}}):\n",
				len(matches), ExecuteExtraToolName)
			for _, t := range matches {
				schemaJSON, _ := json.Marshal(t.InputSchema())
				fmt.Fprintf(&b, "\n## %s\n%s\nparams schema: %s\n", t.Name(), t.Description(), string(schemaJSON))
			}
			return Text(b.String()), nil
		},
	})
}

// NewExecuteExtraTool builds the always-loaded invoker for deferred tools. It
// resolves the named tool from reg, but only if it is currently in the unlock
// set. unlock may be nil (then every registry tool is callable — no gating).
//
// It runs nothing itself: it hands the deferred tool to the agent loop
// (ToolContext.RunDeferred), which checks it under its own name like a direct
// call. A host that wraps tools keeps that path, since a wrapper passes the
// ToolContext on; a call with no loop behind it is refused.
func NewExecuteExtraTool(reg *Registry, unlock *UnlockSet) CoreTool {
	return NewExecuteExtraToolFor(reg, unlock, nil)
}

// NewExecuteExtraToolFor is NewExecuteExtraTool knowing which tools are
// deferred. A call for a tool that is in the session but not deferred (its
// schema is already in the model's list) is answered with "call it directly"
// rather than "load the skill first": nothing gates it, the model only took
// the wrong path.
func NewExecuteExtraToolFor(reg *Registry, unlock *UnlockSet, deferred []string) CoreTool {
	isDeferred := map[string]bool{}
	for _, n := range deferred {
		isDeferred[n] = true
	}
	x := &executeExtra{reg: reg, unlock: unlock, deferred: deferred, isDeferred: isDeferred}
	return Build(Spec{
		Name: ExecuteExtraToolName,
		Description: "Invoke a deferred tool discovered via " + SearchExtraToolsName +
			". tool_name is the deferred tool's exact name; params must match its schema.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool_name": map[string]any{
					"type":        "string",
					"description": "Exact name of the deferred tool (from " + SearchExtraToolsName + " / <available-deferred-tools>).",
				},
				"params": map[string]any{
					"type":        "object",
					"description": "Parameters for the tool, matching its schema.",
				},
			},
			"required": []any{"tool_name"},
		},
		// The wrapper itself needs no approval: the loop checks the deferred
		// tool it hands over (ToolContext.RunDeferred).
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, tc *ToolContext) (Result, error) {
			t, params, refusal := x.resolve(in)
			if refusal != nil {
				return *refusal, nil
			}
			if tc == nil || tc.RunDeferred == nil {
				r := Errorf("Error: " + ExecuteExtraToolName +
					" runs only inside the agent loop, which checks the deferred tool's permissions")
				r.ErrorCode = CodePermissionDenied
				return r, nil
			}
			return tc.RunDeferred(ctx, t, params)
		},
	})
}

type executeExtra struct {
	reg        *Registry
	unlock     *UnlockSet
	deferred   []string
	isDeferred map[string]bool
}

// resolve returns the deferred tool a call names and its input, or the error
// result the call gets instead.
func (x *executeExtra) resolve(in json.RawMessage) (CoreTool, json.RawMessage, *Result) {
	refuse := func(text string, code llm.Code) (CoreTool, json.RawMessage, *Result) {
		r := Errorf(text)
		r.ErrorCode = code
		return nil, nil, &r
	}
	var args struct {
		ToolName string          `json:"tool_name"`
		Params   json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(in, &args); err != nil {
		return refuse("Error: "+err.Error(), CodeInvalidInput)
	}
	if args.ToolName == "" {
		return refuse("Error: tool_name is required", CodeInvalidInput)
	}
	t, ok := x.reg.Get(args.ToolName)
	if !ok {
		return refuse(UnknownToolText(x.reg, args.ToolName), CodeUnknownTool)
	}
	if args.ToolName == ExecuteExtraToolName || args.ToolName == SearchExtraToolsName ||
		(x.unlock != nil && !x.unlock.Has(args.ToolName) && x.deferred != nil && !x.isDeferred[args.ToolName]) {
		return refuse(fmt.Sprintf("Error: %s is not a deferred tool: call %s directly with these params as its input, not through %s.",
			args.ToolName, args.ToolName, ExecuteExtraToolName), CodeToolUnavailable)
	}
	if x.unlock != nil && !x.unlock.Has(args.ToolName) {
		return refuse("Error: tool "+args.ToolName+
			" is not unlocked in this session — load the skill that provides it first.", CodeToolUnavailable)
	}
	params := args.Params
	if len(params) == 0 || string(params) == "null" {
		params = json.RawMessage("{}")
	}
	return t, params, nil
}

// UnknownToolText is the error for a call to a tool the session lacks. When
// one tool's name differs only by case or separators, or by a typo of at
// most two letters, it names that tool; it never runs it, since hooks and
// permission rules match on the name the model sent.
func UnknownToolText(reg *Registry, name string) string {
	msg := fmt.Sprintf("Error: unknown tool %q", name)
	if s := closestToolName(reg, name); s != "" {
		msg += fmt.Sprintf(". Did you mean %q?", s)
	} else {
		msg += ": it is not available to this agent; use one of the tools in your tool list"
	}
	return msg
}

func closestToolName(reg *Registry, name string) string {
	if reg == nil {
		return ""
	}
	norm := func(s string) string {
		return strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(s))
	}
	want := norm(name)
	best, bestD, ties := "", 3, 0
	for _, t := range reg.List() {
		n := t.Name()
		if norm(n) == want {
			return n
		}
		if d := editDistance(norm(n), want, 3); d < bestD {
			best, bestD, ties = n, d, 1
		} else if d == bestD {
			ties++
		}
	}
	if ties != 1 || len(want) < 4 {
		return ""
	}
	return best
}

// editDistance is the Levenshtein distance of a and b, or limit when it is
// at least limit.
func editDistance(a, b string, limit int) int {
	if d := len(a) - len(b); d >= limit || -d >= limit {
		return limit
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= len(b); j++ {
			c := 1
			if a[i-1] == b[j-1] {
				c = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+c)
			rowMin = min(rowMin, cur[j])
		}
		if rowMin >= limit {
			return limit
		}
		prev, cur = cur, prev
	}
	return min(prev[len(b)], limit)
}
