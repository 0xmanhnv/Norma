package foldadapter

// Ref tags past the old m99999 cap, following acp-kernel 83b8609
// (acp-kernel#483; MIT plus Additional Term, Copyright (c) 2026 ranxianglei —
// see THIRD-PARTY-LICENSES.md).

import (
	"testing"

	"github.com/0xmanhnv/corvid/fold"
	"github.com/0xmanhnv/corvid/llm"
)

// A tag for a widened ref must round-trip exactly like a five-digit one;
// otherwise it would stay in the body, enter the message identity, and the id
// would drift every turn the tag is re-rendered.
func TestWidenedRefTagRoundTrip(t *testing.T) {
	for _, ref := range []string{"m99999", "m100000", "m9999999"} {
		body := "body"
		tagged := AppendRefTag(body, RefTag(ref, fold.CoreMessage{ContentType: fold.CTText}, 120))
		if got := StripRefTag(tagged); got != body {
			t.Errorf("%s: StripRefTag(%q) = %q, want %q", ref, tagged, got, body)
		}
		m := llm.Message{Content: []llm.ContentBlock{{Type: llm.BlockText, Text: tagged}}}
		if got := MessageRef(m); got != ref {
			t.Errorf("MessageRef = %q, want %q", got, ref)
		}
	}
}

// Trap: only canonical renderings are tags. A widened ref never carries a
// leading zero, so tag-shaped text such as m004234 is content and stays.
func TestNonCanonicalRefTagIsContent(t *testing.T) {
	for _, in := range []string{
		"body\n<fold-ref id=\"m004234\" tokens=\"5\"/>",
		"body\n<fold-ref id=\"m10000000\" tokens=\"5\"/>",
		"body\n<fold-ref id=\"m1234\" tokens=\"5\"/>",
	} {
		if got := StripRefTag(in); got != in {
			t.Errorf("StripRefTag(%q) = %q, want it untouched", in, got)
		}
		m := llm.Message{Content: []llm.ContentBlock{{Type: llm.BlockText, Text: in}}}
		if got := MessageRef(m); got != "" {
			t.Errorf("MessageRef(%q) = %q, want none", in, got)
		}
	}
}
