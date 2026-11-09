package tool

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

const defaultMaxOutput = 30000

func maxOut(tc *ToolContext) int {
	if tc != nil && tc.MaxOutputChars > 0 {
		return tc.MaxOutputChars
	}
	return defaultMaxOutput
}

var spillSeq atomic.Int64

// Capture bounds a tool's textual output for the model. When the output exceeds
// the limit (ToolContext.MaxOutputChars, default 30000) and the call has a
// spill store (ToolContext.Spill, or a LocalSpill over ToolContext.OutputDir),
// the FULL output is stored there and a tight head + the store's retrieval
// guidance is returned — so the model can read slices on demand (grep/sed)
// instead of losing the overflow. Without a store, or when storing fails, it
// falls back to a head+tail truncation (the overflow is discarded). Tools that
// produce potentially large output should return Capture(tc, output) rather
// than the raw string.
//
// With the local store the pointer is always an ABSOLUTE path (see LocalSpill).
// Heads cut by Capture and tail-fallbacks in truncate() are aligned to UTF-8
// rune boundaries, so a bounded result never carries a half-encoded character.
func Capture(tc *ToolContext, s string) string {
	max := maxOut(tc)
	if len(s) <= max {
		return s
	}
	if store := spillStore(tc); store != nil {
		// Capture has no context of its own; a store that needs one for a
		// deadline should bound itself.
		if sp, err := store.Store(context.Background(), SpillItem{Data: []byte(s), MediaType: MediaText}); err == nil {
			lines := strings.Count(s, "\n") + 1
			return cutHead(s, max) + fmt.Sprintf(
				"\n\n... <persisted-output>[Output too large: full %d bytes / %d lines.%s </persisted-output>",
				len(s), lines, sp.Guidance)
		}
	}
	return truncate(s, max)
}

// alreadyCaptured reports whether s already carries a marker Capture leaves
// behind — a spill pointer or a head+tail truncation notice. It is how a global
// post-tool net recognises output a tool already ran through Capture itself.
//
// A false negative (legitimate text that happens to contain a marker) only means
// the guard fails to re-cap already-large output; it never corrupts. The markers
// are distinctive enough that this is vanishingly rare.
func alreadyCaptured(s string) bool {
	return strings.Contains(s, "<persisted-output>") || strings.Contains(s, "characters truncated]")
}

// CaptureOnce is Capture with an idempotency guard: output a tool already ran
// through Capture (so it carries a spill pointer or truncation notice) is
// returned unchanged. This lets a single post-tool net cap every tool's output
// without re-truncating or re-spilling what built-in / MCP / custom tools
// already handled themselves. Output within the limit is a no-op either way,
// since Capture returns it unchanged.
func CaptureOnce(tc *ToolContext, s string) string {
	if alreadyCaptured(s) {
		return s
	}
	return Capture(tc, s)
}

// resolvePath joins a relative path against the working directory.
func resolvePath(tc *ToolContext, p string) string {
	if filepath.IsAbs(p) || tc == nil || tc.WorkingDir == "" {
		return p
	}
	return filepath.Join(tc.WorkingDir, p)
}

// truncate trims s to max bytes, keeping head and tail. Both cuts are aligned to
// rune boundaries (see cutHead/cutTail): slicing bytes blindly can split a
// multi-byte character, and the mojibake then travels into the model's context
// (and into transcripts) as invalid UTF-8.
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	half := max / 2
	return cutHead(s, half) + fmt.Sprintf("\n\n... [%d characters truncated] ...\n\n", len(s)-max) + cutTail(s, half)
}

// byteSeq is string or []byte: both index to a byte, so the rune-boundary
// primitives below serve either without converting (and allocating) between them.
type byteSeq interface{ string | []byte }

// runeSafeLen returns the largest n <= max with b[:n] ending on a rune boundary.
func runeSafeLen[T byteSeq](b T, max int) int {
	if max <= 0 {
		return 0
	}
	if len(b) <= max {
		return len(b)
	}
	for max > 0 && !utf8.RuneStart(b[max]) {
		max--
	}
	return max
}

// runeSafeStart returns the smallest n with b[n:] starting on a rune boundary,
// i.e. the number of leading UTF-8 continuation bytes to drop. Use it when the
// length is already right and only the boundary is in question — after slicing a
// ring buffer, or seeking a file to an arbitrary byte offset.
func runeSafeStart[T byteSeq](b T) int {
	i := 0
	for i < len(b) && !utf8.RuneStart(b[i]) {
		i++
	}
	return i
}

// cutHead returns the longest prefix of s that is at most max bytes AND ends on a
// rune boundary. max<len(s) is assumed (callers check the length first).
func cutHead(s string, max int) string { return s[:runeSafeLen(s, max)] }

// cutTail returns the longest suffix of s that is at most max bytes AND starts on
// a rune boundary.
func cutTail(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	s = s[len(s)-max:]
	return s[runeSafeStart(s):]
}
