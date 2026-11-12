package permission

import "context"

// Policy is the one place a host chooses, item by item, between the strict
// behaviour of a safety check and the behaviour corvid had before that check
// was tightened ("compatible"). Every field's zero value is its documented
// default, chosen so that a host which upgrades corvid without changing its
// code keeps working; StrictPolicy and CompatiblePolicy set every field. The
// session reports each field left at its compatible value once at start
// (Loosened), naming the strict value. docs/guides/permissions.md has the
// table of fields, values and what each protects against.
type Policy struct {
	// DeferredPermissions: a deferred tool called through ExecuteExtraTool is
	// permission-checked under its own name (deny rules, its own check, the
	// mode, the approval callback): "own_name". "wrapper" runs it after the
	// schema check only, as before, so only ExecuteExtraTool's own (allowing)
	// check applies.
	DeferredPermissions DeferredPermissions
	// DeferredHooks: PreToolUse and PostToolUse hooks also see a deferred tool
	// under its own name ("own_name"), or only as ExecuteExtraTool
	// ("wrapper", as before).
	DeferredHooks DeferredHooks
	// ShellSessions: shell_open and shell_send ask for approval, with the Bash
	// safety floor and Bash deny rules applied to what they run or type
	// ("ask"), or allow themselves ("allow", as before).
	ShellSessions ShellSessions
	// MemoryWrites: RecordMemory asks for approval ("ask") or allows itself
	// ("allow", as before).
	MemoryWrites MemoryWrites
	// HookDecisions: a hook.Registry decision other than "block", "approve" or
	// empty (a misspelt "Block", or "deny") blocks ("block_unknown"), or is
	// ignored and the action continues ("ignore_unknown", as before).
	HookDecisions HookDecisions
	// RewrittenInput: input a PreToolUse hook or a permission decision
	// rewrites is validated against the schema and evaluated again
	// ("recheck"), or used as it is ("trust", as before).
	RewrittenInput RewrittenInput
	// CallPanics: a panic anywhere in a tool call (permission check, hook,
	// result rewriter, the tool) fails that call ("contain"), or only a panic
	// inside the tool itself does and any other ends the process ("tool_only",
	// as before).
	CallPanics CallPanics
	// CallsAfterCancel: a call still queued when the run is cancelled or runs
	// out of time does not start ("refuse"), or starts with the cancelled
	// context ("start", as before).
	CallsAfterCancel CallsAfterCancel
}

// The types of the Policy fields.
type (
	DeferredPermissions string
	DeferredHooks       string
	ShellSessions       string
	MemoryWrites        string
	HookDecisions       string
	RewrittenInput      string
	CallPanics          string
	CallsAfterCancel    string
)

// The values of the Policy fields. The first of each pair is the strict one.
const (
	DeferredPermissionsOwnName DeferredPermissions = "own_name"
	DeferredPermissionsWrapper DeferredPermissions = "wrapper"

	DeferredHooksOwnName DeferredHooks = "own_name"
	DeferredHooksWrapper DeferredHooks = "wrapper"

	ShellSessionsAsk   ShellSessions = "ask"
	ShellSessionsAllow ShellSessions = "allow"

	MemoryWritesAsk   MemoryWrites = "ask"
	MemoryWritesAllow MemoryWrites = "allow"

	HookDecisionsBlockUnknown  HookDecisions = "block_unknown"
	HookDecisionsIgnoreUnknown HookDecisions = "ignore_unknown"

	RewrittenInputRecheck RewrittenInput = "recheck"
	RewrittenInputTrust   RewrittenInput = "trust"

	CallPanicsContain  CallPanics = "contain"
	CallPanicsToolOnly CallPanics = "tool_only"

	CallsAfterCancelRefuse CallsAfterCancel = "refuse"
	CallsAfterCancelStart  CallsAfterCancel = "start"
)

// policyItem describes one Policy field for Resolved, the presets and
// Loosened.
type policyItem struct {
	field          string
	strict, compat string
	def            string // the value an empty field means
	get            func(*Policy) *string
	loose          string // what the compatible value leaves open
}

func items() []policyItem {
	return []policyItem{
		{"DeferredPermissions", "own_name", "wrapper", "own_name",
			func(p *Policy) *string { return (*string)(&p.DeferredPermissions) },
			"a deferred tool called through ExecuteExtraTool skips deny rules and the approval callback"},
		{"DeferredHooks", "own_name", "wrapper", "wrapper",
			func(p *Policy) *string { return (*string)(&p.DeferredHooks) },
			"hooks see deferred tools only as ExecuteExtraTool"},
		{"ShellSessions", "ask", "allow", "ask",
			func(p *Policy) *string { return (*string)(&p.ShellSessions) },
			"shell_open and shell_send allow themselves and Bash deny rules do not reach them"},
		{"MemoryWrites", "ask", "allow", "ask",
			func(p *Policy) *string { return (*string)(&p.MemoryWrites) },
			"RecordMemory saves without approval"},
		{"HookDecisions", "block_unknown", "ignore_unknown", "block_unknown",
			func(p *Policy) *string { return (*string)(&p.HookDecisions) },
			"a misspelt or unknown hook decision lets the action through"},
		{"RewrittenInput", "recheck", "trust", "recheck",
			func(p *Policy) *string { return (*string)(&p.RewrittenInput) },
			"input rewritten by a hook runs without a schema check or a new permission decision"},
		{"CallPanics", "contain", "tool_only", "contain",
			func(p *Policy) *string { return (*string)(&p.CallPanics) },
			"a panic in a permission check, hook or result rewriter ends the process"},
		{"CallsAfterCancel", "refuse", "start", "refuse",
			func(p *Policy) *string { return (*string)(&p.CallsAfterCancel) },
			"a call queued behind others can start after the run was cancelled"},
	}
}

// StrictPolicy sets every field to its strict value: what the embedded
// profile always uses.
func StrictPolicy() Policy {
	var p Policy
	for _, it := range items() {
		*it.get(&p) = it.strict
	}
	return p
}

// CompatiblePolicy sets every field to its value from before the check was
// tightened.
func CompatiblePolicy() Policy {
	var p Policy
	for _, it := range items() {
		*it.get(&p) = it.compat
	}
	return p
}

// Resolved returns p with every empty or unknown field set to its default.
func (p Policy) Resolved() Policy {
	for _, it := range items() {
		if v := it.get(&p); *v != it.strict && *v != it.compat {
			*v = it.def
		}
	}
	return p
}

// Loosened returns one warning for each field of p (resolved) that is not at
// its strict value, naming the field and the value to set.
func (p Policy) Loosened() []string {
	r := p.Resolved()
	var out []string
	for _, it := range items() {
		if v := *it.get(&r); v != it.strict {
			out = append(out, "permission.Policy."+it.field+" is \""+v+"\": "+it.loose+
				"; set it to \""+it.strict+"\" (or use permission.StrictPolicy())")
		}
	}
	return out
}

type policyKey struct{}

// WithPolicy returns ctx carrying p, for code the loop calls that has no
// permission.Context (hooks).
func WithPolicy(ctx context.Context, p Policy) context.Context {
	return context.WithValue(ctx, policyKey{}, p.Resolved())
}

// PolicyFrom returns the policy ctx carries, or the defaults.
func PolicyFrom(ctx context.Context) Policy {
	if p, ok := ctx.Value(policyKey{}).(Policy); ok {
		return p
	}
	return Policy{}.Resolved()
}
