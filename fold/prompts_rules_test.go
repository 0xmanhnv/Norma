package fold

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// UpstreamRulesCommit pins the acp-kernel source the rule constants correspond
// to: tag v0.0.97, commit 1d42ec2197f8f3c879c72e9029c4bcc442180308. The four
// constants in src/compression-rules.ts are byte-identical from that commit
// through v0.0.101. See docs/guides/fold-upstream-sync.md.
const UpstreamRulesCommit = "1d42ec2197f8f3c879c72e9029c4bcc442180308"

// upstreamDirEnv names an acp-kernel checkout to compare against, e.g.
//
//	FOLD_UPSTREAM_DIR=~/src/acp-kernel go test ./fold -run Rules
//
// Check out UpstreamRulesCommit (or any later commit whose rules have not
// changed) first. Unset, the comparison is skipped so the suite runs anywhere.
const upstreamDirEnv = "FOLD_UPSTREAM_DIR"

func upstreamConst(t *testing.T, name string) (string, bool) {
	t.Helper()
	dir := os.Getenv(upstreamDirEnv)
	if dir == "" {
		return "", false
	}
	path := filepath.Join(dir, "src", "compression-rules.ts")
	raw, err := os.ReadFile(path)
	if err != nil {
		// The variable was set on purpose; a missing file is a setup error,
		// not a reason to pass silently.
		t.Fatalf("%s is set but %s cannot be read: %v", upstreamDirEnv, path, err)
	}
	re := regexp.MustCompile("(?s)export const " + name + " = `(.*?)`;\n")
	m := re.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("upstream constant %s not found in %s", name, path)
	}
	return strings.ReplaceAll(string(m[1]), "\\`", "`"), true
}

// The four edits below are the ONLY divergence permitted from upstream. Each is
// a factual correction: upstream directs the model to a `decompress` tool that
// fold does not have, so leaving the text alone would send it after something
// that does not exist.
var allowedEdits = []struct{ upstream, ported string }{
	{"(or you, after decompressing)", "(or you, after reading the archive file)"},
	{"they let you or a later reader jump back via decompress to the exact original.",
		"they let you or a later reader locate the exact original inside the archive file."},
	{"This lets a later decompress target the right block by relevance, not by guessing locations.",
		"This lets a later reader grep the archive directory and find the right block by relevance, not by guessing locations."},
	{"they are ambiguous and cannot be grepped or decompressed-to later.",
		"they are ambiguous and cannot be grepped or located in the archive later."},
	{"you call `compress`", "you call `Compress`"},
}

func TestHowToCompressRulesMatchesUpstream(t *testing.T) {
	up, ok := upstreamConst(t, "HOW_TO_COMPRESS_RULES")
	if !ok {
		t.Skip("set " + upstreamDirEnv + " to an acp-kernel checkout to compare")
	}
	want := up
	for _, e := range allowedEdits {
		if !strings.Contains(want, e.upstream) {
			t.Fatalf("upstream no longer contains %q — the port's edit list is stale and must be revisited", e.upstream)
		}
		want = strings.Replace(want, e.upstream, e.ported, 1)
	}
	// The archive section is appended, so compare only the leading portion.
	got := HowToCompressRules
	idx := strings.Index(got, "\n\nTHE ARCHIVE CHANGES THE COST OF FORGETTING")
	if idx < 0 {
		t.Fatal("HowToCompressRules is missing the archive addendum")
	}
	if got[:idx] != want {
		t.Fatalf("HowToCompressRules diverges from upstream beyond the permitted edits.\nFirst difference at byte %d",
			firstDiff(got[:idx], want))
	}
}

func TestTierRulesAreVerbatim(t *testing.T) {
	cases := []struct {
		name   string
		ported string
	}{
		{"TIER2_DISTILL_RULES", Tier2Rules},
		{"TIER3_CONDENSE_RULES", Tier3Rules},
		{"COMPRESS_PHILOSOPHY", CompressPhilosophy},
	}
	for _, c := range cases {
		up, ok := upstreamConst(t, c.name)
		if !ok {
			t.Skip("set " + upstreamDirEnv + " to an acp-kernel checkout to compare")
		}
		if c.ported != up {
			t.Errorf("%s diverges from upstream at byte %d — it must be verbatim",
				c.name, firstDiff(c.ported, up))
		}
	}
}

// Whatever else changes, the rules must not send the model after a tool that
// does not exist.
func TestRulesMentionNoMissingTools(t *testing.T) {
	for name, text := range map[string]string{
		"CompressPhilosophy": CompressPhilosophy,
		"HowToCompressRules": HowToCompressRules,
		"Tier2Rules":         Tier2Rules,
		"Tier3Rules":         Tier3Rules,
	} {
		for _, missing := range []string{"decompress", "search_context", "acp_status"} {
			if strings.Contains(text, missing) {
				t.Errorf("%s mentions %q, which fold does not provide", name, missing)
			}
		}
	}
}

func TestHowToCompressRulesCarriesArchiveGuidance(t *testing.T) {
	if !strings.Contains(HowToCompressRules, "THE ARCHIVE CHANGES THE COST OF FORGETTING") {
		t.Fatal("the archive addendum is missing — without it the model treats compression as deletion and under-compresses")
	}
	// The closing constraint is what keeps the archive reachable.
	if !strings.Contains(HowToCompressRules, "leaves the archive unreachable") {
		t.Fatal("the addendum must warn that a summary lacking anchors makes the archive unfindable")
	}
}

func firstDiff(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
