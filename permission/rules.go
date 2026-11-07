package permission

import (
	"encoding/json"
	"strings"
)

// SubjectKind says how a rule spec is matched against a tool call.
type SubjectKind int

const (
	SubjectNone  SubjectKind = iota // the call has no subject a spec can match
	SubjectValue                    // a path, URL or other single value
	SubjectShell                    // a shell command line
)

// Subject is the part of a tool call that a rule spec such as "Bash(git status)"
// or "Read(/src/*)" is matched against.
type Subject struct {
	Value string
	Kind  SubjectKind
}

// subjectFields are the input fields a subject is taken from, in order, when the
// host does not supply one.
var subjectFields = []struct {
	name string
	kind SubjectKind
}{
	{"command", SubjectShell},
	{"file_path", SubjectValue},
	{"notebook_path", SubjectValue},
	{"path", SubjectValue},
	{"url", SubjectValue},
}

// SubjectOf derives the subject from a tool input: the first string field among
// command, file_path, notebook_path, path and url. Inputs without one have no
// subject.
func SubjectOf(input json.RawMessage) Subject {
	var m map[string]json.RawMessage
	if json.Unmarshal(input, &m) != nil {
		return Subject{}
	}
	for _, f := range subjectFields {
		var s string
		if raw, ok := m[f.name]; ok && json.Unmarshal(raw, &s) == nil && s != "" {
			return Subject{Value: s, Kind: f.kind}
		}
	}
	return Subject{}
}

// shellControl marks a command line that runs more than one command or splices
// in another command's output. An allow rule never matches one: "Bash(git:*)"
// must not allow "git status; rm -rf ~".
var shellControl = []string{";", "&", "|", "`", "$(", "\n", "\r", ">", "<"}

func isCompound(cmd string) bool {
	for _, c := range shellControl {
		if strings.Contains(cmd, c) {
			return true
		}
	}
	return false
}

// shellSegments splits a command line on its separators so a deny rule can match
// any command in it.
func shellSegments(cmd string) []string {
	f := func(r rune) bool { return r == ';' || r == '&' || r == '|' || r == '\n' || r == '\r' }
	var out []string
	for _, s := range strings.FieldsFunc(cmd, f) {
		s = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s), "({"))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// specMatches matches one value against a rule spec:
//   - "prefix:*" matches the prefix itself or the prefix followed by a space and anything;
//   - a spec containing "*" is a glob where "*" matches any run of characters, "/" included;
//   - anything else must be equal.
func specMatches(spec, v string) bool {
	if p, ok := strings.CutSuffix(spec, ":*"); ok {
		return v == p || strings.HasPrefix(v, p+" ")
	}
	if strings.Contains(spec, "*") {
		return globMatch(spec, v)
	}
	return spec == v
}

// globMatch is path.Match with "*" also crossing "/": rules name paths and URLs,
// where "/src/*" is meant to cover the whole tree.
func globMatch(pattern, v string) bool {
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(v, parts[0]) {
		return false
	}
	v = v[len(parts[0]):]
	for i, p := range parts[1:] {
		if i == len(parts)-2 {
			return strings.HasSuffix(v, p)
		}
		j := strings.Index(v, p)
		if j < 0 {
			return false
		}
		v = v[j+len(p):]
	}
	return true
}

// ruleMatches reports whether one rule matches a call. deny selects the
// fail-safe direction for what can't be interpreted: a spec the call has no
// subject for never allows and always denies; a compound shell command never
// matches an allow spec, and matches a deny spec when any of its commands does.
func ruleMatches(rule, toolName string, sub Subject, deny bool) bool {
	rule = strings.TrimSpace(rule)
	if rule == "*" || rule == toolName {
		return true
	}
	i := strings.IndexByte(rule, '(')
	if i <= 0 || !strings.HasSuffix(rule, ")") || rule[:i] != toolName {
		return false
	}
	spec := strings.TrimSpace(rule[i+1 : len(rule)-1])
	if spec == "" || spec == "*" {
		return true
	}
	switch sub.Kind {
	case SubjectNone:
		return deny
	case SubjectShell:
		cmd := strings.TrimSpace(sub.Value)
		if !deny {
			return !isCompound(cmd) && specMatches(spec, cmd)
		}
		if specMatches(spec, cmd) {
			return true
		}
		for _, seg := range shellSegments(cmd) {
			if specMatches(spec, seg) {
				return true
			}
		}
		// Substitutions ($(...), backticks) can hide a command from the split
		// above; a deny spec then also matches anywhere in the line.
		return (strings.Contains(cmd, "$(") || strings.Contains(cmd, "`")) &&
			strings.Contains(cmd, strings.TrimSuffix(strings.TrimSuffix(spec, ":*"), "*"))
	default:
		return specMatches(spec, sub.Value)
	}
}

// matchRule reports whether any rule matches the call.
func matchRule(rules []string, toolName string, sub Subject, deny bool) bool {
	for _, r := range rules {
		if ruleMatches(r, toolName, sub, deny) {
			return true
		}
	}
	return false
}
