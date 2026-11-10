package corvid_test

// Documentation checks. They keep the Markdown in this repository honest about
// the tree it describes: every relative link and #anchor resolves, every
// backticked repository path exists, Go comments only point at docs that exist,
// and Mermaid diagrams avoid the parser traps that break GitHub's renderer
// (reserved words used as ids, unquoted labels with brackets in them).
//
// They read files only; nothing here needs the network or a Mermaid install.

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// mdDoc is one parsed Markdown file.
type mdDoc struct {
	path    string   // slash-separated, relative to the repo root
	lines   []string // every line of the file
	prose   []bool   // lines[i] is outside a fenced code block
	anchors map[string]bool
	mermaid []mermaidBlock
}

type mermaidBlock struct {
	line int // 1-based line of the first diagram line
	body []string
}

var (
	fenceRE      = regexp.MustCompile("^\\s*(```+|~~~+)\\s*([A-Za-z0-9_-]*)")
	headingRE    = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$`)
	codeSpanRE   = regexp.MustCompile("(`+)(.+?)(`+)")
	inlineLinkRE = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)
	refLinkRE    = regexp.MustCompile(`^\s{0,3}\[[^\]]+\]:\s*<?(\S+?)>?(?:\s+"[^"]*")?\s*$`)
	htmlAttrRE   = regexp.MustCompile(`\b(?:src|href|srcset)="([^"]+)"`)
	mdLinkTextRE = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	goDocRefRE   = regexp.MustCompile("docs/[^\\s)\"'`,;]+\\.md")
)

// repoRoot is the module root: this file lives there.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// walkRepo calls fn for every regular file whose name has one of exts,
// skipping VCS metadata and testdata.
func walkRepo(t *testing.T, root string, exts []string, fn func(rel string)) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if slices.Contains(exts, filepath.Ext(p)) {
			rel, _ := filepath.Rel(root, p)
			fn(filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func loadDocs(t *testing.T, root string) map[string]*mdDoc {
	t.Helper()
	docs := map[string]*mdDoc{}
	walkRepo(t, root, []string{".md"}, func(rel string) {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		docs[rel] = parseDoc(rel, string(b))
	})
	if len(docs) == 0 {
		t.Fatal("no Markdown files found")
	}
	return docs
}

func parseDoc(rel, text string) *mdDoc {
	d := &mdDoc{path: rel, lines: strings.Split(text, "\n"), anchors: map[string]bool{}}
	d.prose = make([]bool, len(d.lines))
	seen := map[string]int{}
	var fence, lang string
	var block *mermaidBlock
	for i, ln := range d.lines {
		if fence != "" {
			if strings.HasPrefix(strings.TrimSpace(ln), fence) {
				if block != nil {
					d.mermaid = append(d.mermaid, *block)
					block = nil
				}
				fence = ""
				continue
			}
			if block != nil {
				block.body = append(block.body, ln)
			}
			continue
		}
		if m := fenceRE.FindStringSubmatch(ln); m != nil {
			fence, lang = m[1], m[2]
			if lang == "mermaid" {
				block = &mermaidBlock{line: i + 2}
			}
			continue
		}
		d.prose[i] = true
		if m := headingRE.FindStringSubmatch(ln); m != nil {
			slug := githubSlug(m[2])
			if n := seen[slug]; n > 0 {
				d.anchors[slug+"-"+strconv.Itoa(n)] = true
			} else {
				d.anchors[slug] = true
			}
			seen[slug]++
		}
	}
	return d
}

// githubSlug derives the anchor GitHub gives a heading: the rendered text,
// lower-cased, with everything but letters, digits, spaces, hyphens and
// underscores removed, and spaces turned into hyphens.
func githubSlug(heading string) string {
	s := mdLinkTextRE.ReplaceAllString(heading, "$1")
	s = strings.NewReplacer("`", "", "*", "").Replace(s)
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '_', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

// TestDocsLinks checks every relative link (Markdown inline and reference
// links, and src/href/srcset attributes of inline HTML) for a target that
// exists, and every #fragment for a heading that produces it.
func TestDocsLinks(t *testing.T) {
	root := repoRoot(t)
	docs := loadDocs(t, root)
	for _, rel := range sortedKeys(docs) {
		d := docs[rel]
		for i, ln := range d.lines {
			if !d.prose[i] {
				continue
			}
			plain := codeSpanRE.ReplaceAllString(ln, "")
			var targets []string
			for _, m := range inlineLinkRE.FindAllStringSubmatch(plain, -1) {
				targets = append(targets, m[1])
			}
			if m := refLinkRE.FindStringSubmatch(plain); m != nil {
				targets = append(targets, m[1])
			}
			for _, m := range htmlAttrRE.FindAllStringSubmatch(plain, -1) {
				for _, cand := range strings.Split(m[1], ",") {
					if f := strings.Fields(cand); len(f) > 0 {
						targets = append(targets, f[0])
					}
				}
			}
			for _, target := range targets {
				if msg := checkLink(root, docs, d, target); msg != "" {
					t.Errorf("%s:%d: link %q: %s", rel, i+1, target, msg)
				}
			}
		}
	}
}

func checkLink(root string, docs map[string]*mdDoc, from *mdDoc, target string) string {
	if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
		return ""
	}
	pathPart, frag, _ := strings.Cut(target, "#")
	pathPart, _, _ = strings.Cut(pathPart, "?")
	dest := from
	if pathPart != "" {
		if strings.HasPrefix(pathPart, "/") {
			return "absolute paths do not resolve on GitHub; use a relative link"
		}
		resolved := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(from.path), pathPart)))
		if resolved == ".." || strings.HasPrefix(resolved, "../") {
			return "points outside the repository"
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(resolved))); err != nil {
			return "target does not exist"
		}
		dest = docs[resolved]
	}
	if frag == "" {
		return ""
	}
	if dest == nil {
		return "" // a fragment into a non-Markdown file is not checked
	}
	if !dest.anchors[strings.ToLower(frag)] {
		return "no heading in " + dest.path + " produces #" + frag
	}
	return ""
}

// TestDocsBacktickedPaths checks that a code span naming a repository path
// (one whose first segment is a top-level file or directory of this repo, like
// `harness/query.go` or `docs/guides/`) names something that exists. A trailing
// `: symbol` or ` (note)` is ignored. Bare file names with an asset or Go
// extension must exist next to the document or at the repo root.
func TestDocsBacktickedPaths(t *testing.T) {
	root := repoRoot(t)
	docs := loadDocs(t, root)
	top := map[string]bool{}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			top[e.Name()] = true
		}
	}
	pathy := regexp.MustCompile(`^[A-Za-z0-9_.\-/]+$`)
	for _, rel := range sortedKeys(docs) {
		d := docs[rel]
		for i, ln := range d.lines {
			if !d.prose[i] {
				continue
			}
			for _, m := range codeSpanRE.FindAllStringSubmatch(ln, -1) {
				span := strings.TrimSpace(m[2])
				if j := strings.IndexAny(span, ": ("); j > 0 {
					span = span[:j]
				}
				if !pathy.MatchString(span) || strings.Contains(span, "..") {
					continue
				}
				first, _, hasSlash := strings.Cut(span, "/")
				switch {
				case hasSlash && top[first]:
					if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(span))); err != nil {
						t.Errorf("%s:%d: `%s` does not exist in the repository", rel, i+1, span)
					}
				case !hasSlash && slices.Contains([]string{".go", ".svg", ".png"}, filepath.Ext(span)):
					near := filepath.Join(root, filepath.FromSlash(filepath.Dir(rel)), span)
					_, errNear := os.Stat(near)
					_, errRoot := os.Stat(filepath.Join(root, span))
					if errNear != nil && errRoot != nil {
						t.Errorf("%s:%d: `%s` exists neither next to the document nor at the repo root", rel, i+1, span)
					}
				}
			}
		}
	}
}

// TestGoCommentDocRefs checks that Go source only refers to Markdown files
// under docs/ that exist.
func TestGoCommentDocRefs(t *testing.T) {
	root := repoRoot(t)
	walkRepo(t, root, []string{".go"}, func(rel string) {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		for i, ln := range strings.Split(string(b), "\n") {
			for _, ref := range goDocRefRE.FindAllString(ln, -1) {
				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(ref))); err != nil {
					t.Errorf("%s:%d: refers to %s, which does not exist", rel, i+1, ref)
				}
			}
		}
	})
}

// Words Mermaid's sequence-diagram lexer treats as keywords, matched without
// regard to case: a participant named "Loop" or "end" breaks the diagram.
var sequenceReserved = map[string]bool{
	"loop": true, "alt": true, "else": true, "opt": true, "par": true, "and": true,
	"critical": true, "option": true, "break": true, "rect": true, "end": true,
	"note": true, "over": true, "participant": true, "actor": true, "box": true,
	"activate": true, "deactivate": true, "autonumber": true, "title": true,
	"create": true, "destroy": true, "links": true, "link": true,
}

// Words that cannot be flowchart node or subgraph ids (the flowchart lexer is
// case-sensitive, so "End" is fine and "end" is not).
var flowchartReserved = map[string]bool{
	"end": true, "subgraph": true, "graph": true, "flowchart": true, "direction": true,
	"style": true, "classDef": true, "class": true, "click": true, "linkStyle": true,
	"call": true, "href": true, "interpolate": true, "default": true,
}

var (
	seqDeclRE    = regexp.MustCompile(`^\s*(?:participant|actor)\s+(\S+)`)
	seqMessageRE = regexp.MustCompile(`^\s*([^\s:]+?)\s*(?:-->>|->>|-->|->|--x|-x|--\)|-\))\s*[+-]?\s*([^\s:]+?)\s*:`)
)

// TestDocsMermaid checks the ids used in flowchart and sequence diagrams
// against the keywords of Mermaid's grammar, and flowchart labels for
// unquoted brackets, which end a label early.
func TestDocsMermaid(t *testing.T) {
	root := repoRoot(t)
	docs := loadDocs(t, root)
	total := 0
	for _, rel := range sortedKeys(docs) {
		for _, b := range docs[rel].mermaid {
			total++
			kind := ""
			for _, ln := range b.body {
				if f := strings.Fields(ln); len(f) > 0 && !strings.HasPrefix(f[0], "%%") {
					kind = f[0]
					break
				}
			}
			for k, ln := range b.body {
				at := func(format string, args ...any) {
					t.Errorf("%s:%d: "+format, append([]any{rel, b.line + k}, args...)...)
				}
				switch kind {
				case "sequenceDiagram":
					for _, id := range sequenceIDs(ln) {
						if sequenceReserved[strings.ToLower(id)] {
							at("sequence participant %q is a Mermaid keyword; give it an alias (participant L as %s)", id, id)
						}
					}
				case "flowchart", "graph":
					ids, bad := flowchartLine(ln)
					for _, id := range ids {
						if flowchartReserved[id] {
							at("flowchart id %q is a Mermaid keyword; rename it", id)
						}
					}
					for _, label := range bad {
						at("flowchart label %q has brackets; wrap it in double quotes", label)
					}
				}
			}
		}
	}
	if total == 0 {
		t.Fatal("no Mermaid diagrams found; the extractor is broken")
	}
}

func sequenceIDs(ln string) []string {
	if m := seqDeclRE.FindStringSubmatch(ln); m != nil {
		return []string{m[1]}
	}
	if m := seqMessageRE.FindStringSubmatch(ln); m != nil {
		return []string{m[1], m[2]}
	}
	return nil
}

var shapeClose = map[byte]byte{'[': ']', '(': ')', '{': '}'}

// flowchartLine returns the ids a flowchart statement names and any unquoted
// shape label that contains a bracket. It is a scanner, not a parser: it skips
// quoted strings and |edge labels|, reads each word as an id, and reads the
// shape that follows a word as its label.
func flowchartLine(ln string) (ids, badLabels []string) {
	s := strings.TrimSpace(ln)
	if s == "" || strings.HasPrefix(s, "%%") || s == "end" {
		return nil, nil
	}
	first := strings.Fields(s)[0]
	switch first {
	case "flowchart", "graph", "direction", "classDef", "class", "style", "linkStyle", "click":
		return nil, nil
	case "subgraph":
		s = strings.TrimSpace(strings.TrimPrefix(s, "subgraph"))
	}
	isWord := func(c byte) bool {
		return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '"' || c == '|':
			j := strings.IndexByte(s[i+1:], c)
			if j < 0 {
				return ids, badLabels
			}
			i += j + 2
		case isWord(c):
			j := i
			for j < len(s) && isWord(s[j]) {
				j++
			}
			ids = append(ids, s[i:j])
			i = j
			if i < len(s) && (s[i] == '[' || s[i] == '(' || s[i] == '{' || s[i] == '>') {
				label, next, bad := readShape(s, i)
				if bad {
					badLabels = append(badLabels, label)
				}
				i = next
			}
		default:
			i++
		}
	}
	return ids, badLabels
}

// readShape reads the shape that starts at s[i] and returns its label, the
// index just past it, and whether the label is unquoted and contains a bracket.
func readShape(s string, i int) (label string, next int, bad bool) {
	var closer []byte
	if s[i] == '>' { // asymmetric shape: id>label]
		closer = []byte{']'}
		i++
	} else {
		for i < len(s) {
			c, ok := shapeClose[s[i]]
			if !ok {
				break
			}
			closer = append([]byte{c}, closer...)
			i++
		}
	}
	end := string(closer)
	if i < len(s) && s[i] == '"' {
		j := strings.IndexByte(s[i+1:], '"')
		if j < 0 {
			return s[i:], len(s), false
		}
		label = s[i+1 : i+1+j]
		i += j + 2
		if k := strings.Index(s[i:], end); k >= 0 {
			i += k + len(end)
		}
		return label, i, false
	}
	k := strings.Index(s[i:], end)
	if k < 0 {
		return s[i:], len(s), true
	}
	label = s[i : i+k]
	return label, i + k + len(end), strings.ContainsAny(label, "[](){}")
}

// TestDocsCheckersSelfTest pins the scanners above to the cases they exist for,
// so a regression in a checker cannot quietly turn the doc tests into no-ops.
func TestDocsCheckersSelfTest(t *testing.T) {
	slugs := map[string]string{
		"7. MCP, skills, memory, plan mode":                            "7-mcp-skills-memory-plan-mode",
		"Model-driven compression (`fold`)":                            "model-driven-compression-fold",
		"Code ported from v0.0.98 or later (MIT plus Additional Term)": "code-ported-from-v0098-or-later-mit-plus-additional-term",
		"See [jobs](guides/jobs.md) → **Safety**":                      "see-jobs--safety",
	}
	for in, want := range slugs {
		if got := githubSlug(in); got != want {
			t.Errorf("githubSlug(%q) = %q, want %q", in, got, want)
		}
	}
	seq := map[string][]string{
		"    participant Loop":            {"Loop"},
		"    participant L as Loop":       {"L"},
		"    Host->>H: range Prompt(ctx)": {"Host", "H"},
		"    end->>H: x":                  {"end", "H"},
		"    Note over H: x":              nil,
	}
	for in, want := range seq {
		if got := sequenceIDs(in); !slices.Equal(got, want) {
			t.Errorf("sequenceIDs(%q) = %q, want %q", in, got, want)
		}
	}
	type flow struct{ ids, bad []string }
	flows := map[string]flow{
		`    A --> end`:                              {ids: []string{"A", "end"}},
		`    End1([max_turns / timeout])`:            {ids: []string{"End1"}},
		`    X[foo (bar)] --> Y`:                     {ids: []string{"X", "Y"}, bad: []string{"foo (bar)"}},
		`    X["foo (bar)"] -->|a (b)| Y{ok?}`:       {ids: []string{"X", "Y"}},
		`    subgraph Loop["harness.Query"]`:         {ids: []string{"Loop"}},
		`    memory -.tools.-> tool`:                 {ids: []string{"memory", "tools", "tool"}},
		`    end`:                                    {},
		`    P1 & P2 --> Agg[collected<br/>results]`: {ids: []string{"P1", "P2", "Agg"}},
	}
	for in, want := range flows {
		ids, bad := flowchartLine(in)
		if !slices.Equal(ids, want.ids) || !slices.Equal(bad, want.bad) {
			t.Errorf("flowchartLine(%q) = %q, %q; want %q, %q", in, ids, bad, want.ids, want.bad)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
