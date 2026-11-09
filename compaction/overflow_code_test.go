package compaction

import (
	"fmt"
	"testing"

	"github.com/0xmanhnv/norma/llm"
)

// The code decides first: an overflow an adapter classified is recognised
// whatever its text says (a localized message, an unfamiliar phrasing), and an
// error classified as something else is not taken for an overflow even if its
// text happens to match. Uncoded errors keep the text fallback, which
// TestIsOverflowStrings covers.
func TestIsOverflowByCode(t *testing.T) {
	if !IsOverflow(llm.NewError(llm.CodeContextOverflow, "la solicitud excede la ventana")) {
		t.Fatal("a classified overflow must be recognised by its code alone")
	}
	wrapped := fmt.Errorf("model call: %w", llm.NewError(llm.CodeContextOverflow, "x"))
	if !IsOverflow(wrapped) {
		t.Fatal("the code must survive wrapping")
	}
	// Trap: a rate-limit body that mentions the context length is still a rate
	// limit; a reactive compaction cannot fix it.
	if IsOverflow(llm.NewError(llm.CodeRateLimited, "openai: status 429: too many tokens for this context length per minute")) {
		t.Fatal("a classified non-overflow must not fall through to the text match")
	}
	if !IsOverflow(llm.NewError(llm.CodeUnknown, "custom: prompt is too long")) {
		t.Fatal("CodeUnknown must keep the text fallback")
	}
	if IsOverflow(llm.NewError(llm.CodeUnknown, "custom: connection reset")) {
		t.Fatal("text fallback false positive")
	}
}
