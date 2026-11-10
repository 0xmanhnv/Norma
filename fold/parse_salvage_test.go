package fold

// Tests for single-quote salvage and parser-position repair, adapted from
// acp-kernel's tests/basic.test.ts (#603 matrix) and
// tests/parse-compress-input.test.ts (MIT, Copyright © 2026 ranxianglei).

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseSalvagesSingleQuotedArgs(t *testing.T) {
	r := parse(`{'content':[{'startId':'m00010','endId':'m00020','summary':'first'},{'startId':'m00030','endId':'m00040','summary':'second','topic':'mid'}],'topic':'intro'}`)
	d := r.Diagnostics
	if d.Kind != ParseOK || !d.Ok || !d.QuoteSalvage {
		t.Fatalf("diagnostics = %+v", d)
	}
	if len(r.Ranges) != 2 || r.Ranges[0].StartRef != "m00010" || r.Ranges[0].Summary != "first" ||
		r.Ranges[0].Topic != "intro" || r.Ranges[1].Topic != "mid" {
		t.Fatalf("ranges = %+v", r.Ranges)
	}
}

func TestParseSalvagesMixedQuotesKeepingApostrophes(t *testing.T) {
	r := parse(`{'content': [{'startId': "m00001", 'endId': "m00009", 'summary': "it's done, really"}]}`)
	if !r.Diagnostics.QuoteSalvage || len(r.Ranges) != 1 || r.Ranges[0].Summary != "it's done, really" {
		t.Fatalf("result = %+v", r)
	}
}

func TestParseSalvagesTruncatedSingleQuotedArray(t *testing.T) {
	r := parse(`{'content':[{'startId':'m00010','endId':'m00020','summary':'first'},{'startId':'m00030','endId':'m00040','summary':'secon`)
	d := r.Diagnostics
	if d.Kind != ParseTruncated || !d.Ok || !d.QuoteSalvage {
		t.Fatalf("diagnostics = %+v", d)
	}
	if len(r.Ranges) != 1 || r.Ranges[0].Summary != "first" {
		t.Fatalf("ranges = %+v", r.Ranges)
	}
}

func TestParseSalvagesSingleQuotedContentString(t *testing.T) {
	r := parse(`{"content":"[{'startId':'m00005','endId':'m00006','summary':'ok'}]"}`)
	if r.Diagnostics.Kind != ParseOK || !r.Diagnostics.QuoteSalvage || len(r.Ranges) != 1 || r.Ranges[0].EndRef != "m00006" {
		t.Fatalf("result = %+v", r)
	}
}

func TestParseRejectsProseWithApostrophes(t *testing.T) {
	r := parse(`I couldn't believe {that} would 'work'`)
	if r.Diagnostics.Ok || len(r.Ranges) != 0 || r.Diagnostics.Kind != ParseMalformedJSON || r.Diagnostics.QuoteSalvage {
		t.Fatalf("result = %+v", r)
	}
}

func TestParseLeavesValidArgsUntouched(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"content": []map[string]any{{"startId": "m00001", "endId": "m00002", "summary": "don't panic"}}})
	r := ParseCompressArgs(raw)
	if r.Diagnostics.QuoteSalvage || len(r.Ranges) != 1 || r.Ranges[0].Summary != "don't panic" {
		t.Fatalf("result = %+v", r)
	}
}

func TestParseSingleQuotesWithTrailingCommaAndNewline(t *testing.T) {
	r := parse("{'content':[{'startId':'m00001','endId':'m00002','summary':'line1\nline2',}]}")
	if !r.Diagnostics.QuoteSalvage || len(r.Ranges) != 1 || r.Ranges[0].Summary != "line1\nline2" {
		t.Fatalf("result = %+v", r)
	}
}

// Quote repair makes the entry parseable, but validation still applies, and a
// retry that recovers no more ranges does not replace the first diagnostics.
func TestParseSingleQuoteSalvageInventsNothing(t *testing.T) {
	r := parse(`{'content':[{'startId':'m00010'}]}`)
	if len(r.Ranges) != 0 || r.Diagnostics.Ok || r.Diagnostics.QuoteSalvage {
		t.Fatalf("result = %+v", r)
	}
}

// Raw newlines AND markdown \` escapes in one stringified entry: the
// string-state pass fixes neither reliably; the parser-position pass fixes
// both.
func TestParseRepairsNewlinesPlusInvalidEscapes(t *testing.T) {
	content := "[{\"startId\":\"m00066\",\"endId\":\"m00081\",\"summary\":\"exact pre-edit code\n\n## Files (\\`src/config.ts\\` quoted \\`path\\`) tail\"}]"
	raw, _ := json.Marshal(map[string]any{"content": content})
	r := ParseCompressArgs(raw)
	if !r.Diagnostics.Ok || len(r.Ranges) != 1 {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(r.Ranges[0].Summary, "(\\`src/config.ts\\` quoted \\`path\\`)") {
		t.Fatalf("summary = %q, want the backslashes kept literally", r.Ranges[0].Summary)
	}
	if !strings.Contains(r.Ranges[0].Summary, "\n\n## Files") {
		t.Fatalf("summary = %q, want the newlines restored", r.Ranges[0].Summary)
	}
}

func TestParseRepairsScatteredControlCharacters(t *testing.T) {
	content := "[{\"startId\":\"m00010\",\"endId\":\"m00012\",\"summary\":\"a\nb\tc\rd\x01e\"}]"
	raw, _ := json.Marshal(map[string]any{"content": content})
	r := ParseCompressArgs(raw)
	if !r.Diagnostics.Ok || len(r.Ranges) != 1 || r.Ranges[0].Summary != "a\nb\tc\rd\x01e" {
		t.Fatalf("result = %+v", r)
	}
}

// Unescaped inner quotes are the unexpected-token class: structure would
// have to be invented, so the parse stays a clean failure.
func TestParseLeavesUnexpectedTokensUnrecovered(t *testing.T) {
	content := "[{\"startId\":\"m00002\",\"endId\":\"m00007\",\"summary\":\"user said \"fix it\" now\nsecond line\"}]"
	raw, _ := json.Marshal(map[string]any{"content": content})
	if r := ParseCompressArgs(raw); r.Diagnostics.Ok && len(r.Ranges) > 0 {
		t.Fatalf("recovered %+v from input that needs invented structure", r.Ranges)
	}
	if _, changed := repairAtParserPositions(`[{"startId": "m00010", "endId"]`); changed {
		t.Fatal("a structural error must not be touched")
	}
	if _, changed := repairAtParserPositions(`{"a":"ok"}`); changed {
		t.Fatal("valid JSON must not be touched")
	}
}
