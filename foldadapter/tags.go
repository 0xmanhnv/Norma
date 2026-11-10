package foldadapter

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/0xmanhnv/corvid/fold"
	"github.com/0xmanhnv/corvid/llm"
)

// The ref tag tells the model how to address a message when compressing.
//
// Shape decisions, each load-bearing:
//   - Self-closing, so a truncated model response cannot leave an orphaned
//     </fold-ref> polluting the context.
//   - A hyphenated custom element, which models read as host-injected structure
//     rather than content, and which cannot collide with a <ref> or <msg> the
//     agent read out of a file.
//   - src only on tool output. Its presence IS the signal; writing src="text"
//     on every user message would spend bytes to say nothing.
//
// The regexes anchor on BOTH the element name and the id format. Matching the
// name alone would let a <fold-ref> appearing inside a file the agent read be
// stripped out of that file's content.
// canonicalRef matches exactly what fold.IndexToRef renders.
//
// Ported from acp-kernel (MIT plus Additional Term, Copyright (c) 2026
// ranxianglei): src/ccr.ts, CANONICAL_REF — 83b8609 (v0.0.100, acp-kernel#483).
const canonicalRef = `m(?:\d{5}|[1-9]\d{5,6})`

var (
	// Only the TRAILING form is stripped, because only the trailing form is ever
	// produced (see AppendRefTag). Stripping a leading one as well would defend
	// against a shape fold never emits, at the cost of silently eating a user
	// message that legitimately opens with tag-shaped text — a transcript
	// pasted back in, say. A silent corruption of real content is worse than the
	// theoretical id drift it would prevent, and that drift is at least visible
	// as a dangling ref.
	// Exactly ONE separator is consumed, matching the single newline
	// AppendRefTag inserts. A greedy `\n*` would also swallow a newline the body
	// itself ended with, so tagging and stripping would not round-trip and the
	// content would quietly change.
	//
	// The id must be a CANONICAL ref rendering — five zero-padded digits, or
	// six to seven with no leading zero (m100000 .. m9999999, see fold.RefWidth)
	// — so tag-shaped text with any other id is content and stays.
	trailingTagRE = regexp.MustCompile(`(?:^|\n)<fold-ref\s+id="` + canonicalRef + `"[^>]*/>\s*$`)
	anyRefTagRE   = regexp.MustCompile(`<fold-ref\s+id="(` + canonicalRef + `)"[^>]*/>`)
)

// StripRefTag removes the ref tag from the end of a body.
//
// Projection calls this before hashing: a tag inside the identity would make
// the id drift every turn the tag is re-rendered.
func StripRefTag(s string) string {
	return trailingTagRE.ReplaceAllString(s, "")
}

// MessageRef reads a ref back out of a message's text, if it carries one.
func MessageRef(m llm.Message) string {
	for _, b := range m.Content {
		if match := anyRefTagRE.FindStringSubmatch(b.Text); match != nil {
			return match[1]
		}
	}
	return ""
}

// srcOf returns the tag's src attribute; "" means omit it.
//
// Note the value space: because assistant messages carry no tag at all (see
// InjectRefTag), only user text and tool results reach this function. So src is
// either "" or a tool name. The reasoning and tool-call branches are defensive
// — if assistant tagging is ever enabled, this function needs no change.
func srcOf(m fold.CoreMessage) string {
	switch m.ContentType {
	case fold.CTToolCall, fold.CTToolResult:
		if m.ToolName != "" {
			return m.ToolName
		}
		return "tool"
	case fold.CTText:
		return ""
	default:
		return string(m.ContentType)
	}
}

// RefTag renders the tag for one message.
//
// tokens is a pure function of the body AT FIRST RENDER, frozen thereafter in
// the token snapshot. A tag whose number moves is a byte that moves, and a byte
// that moves invalidates the whole prefix cache from that point on — so the
// number describes what the model first saw, not what is there now.
func RefTag(ref string, m fold.CoreMessage, tokens int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<fold-ref id=%q tokens=%q`, ref, fold.FormatTokens(tokens))
	if s := srcOf(m); s != "" {
		fmt.Fprintf(&b, ` src=%q`, s)
	}
	b.WriteString("/>")
	return b.String()
}

// tokenForRef resolves a ref's frozen token count, computing and freezing it on
// first sight.
func tokenForRef(snapshot map[string]int, ref, body string) int {
	if n, ok := snapshot[ref]; ok {
		return n
	}
	n := fold.DefaultCountTokens(body)
	if snapshot != nil {
		snapshot[ref] = n
	}
	return n
}

// AppendRefTag puts the tag at the END of a body.
//
// Trailing rather than leading: a leading tag would corrupt a tool_use's JSON
// arguments, and models are markedly more likely to echo something that opens a
// message than something that closes it.
func AppendRefTag(body, tag string) string {
	if body == "" {
		return tag
	}
	return body + "\n" + tag
}
