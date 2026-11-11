package foldadapter

import (
	"github.com/0xmanhnv/corvid/agentcore"
	"github.com/0xmanhnv/corvid/fold"
	"github.com/0xmanhnv/corvid/tool"
)

// Enable wires fold into a set of agentcore Options.
//
// This is the ONLY entry point, and it is all-or-nothing by design. It attaches
// three things at once:
//
//  1. opts.Compactor          — takes over the view (and disables the built-in
//     Compaction, which a session cannot run alongside it)
//  2. opts.Tools += Compress  — the compression tool
//  3. opts.AppendSystemPrompt — the three resident prompt sections
//
// They must live or die together. Wiring them separately makes three ways to
// get it half-right, and each failure is quiet:
//
//   - no Compactor: the model compresses, the panel says it worked, and the
//     view never changes — the compression went nowhere.
//   - no tool: the prompt teaches a tool that does not exist.
//   - no prompt: the model has the tool but not the rules, and writes summaries
//     that drop paths, decisions and error strings. Nothing reports an error;
//     it surfaces hours later as a model that has forgotten what it did.
//
// Not calling Enable is the off switch: nothing is attached, and the built-in
// compaction keeps working exactly as before.
func Enable(opts *agentcore.Options, o Options) error {
	if o.ModelContextLimit == 0 && o.Config == nil {
		o.ModelContextLimit = defaultContextLimit
	}
	if o.OnWarn == nil {
		o.OnWarn = opts.OnWarn
	}
	sess, err := newSession(o)
	if err != nil {
		return err
	}

	opts.Compactor = &Compactor{sess: sess}
	opts.Tools = append(opts.Tools, newCompressTool(sess))
	addStaticSection(opts, fold.BuildSystemPrompt(fold.Sections{}))
	return nil
}

// addStaticSection adds fold's prompt section to the system prompt, inside the
// cacheable prefix. The section is fully static: left past the boundary it
// would be re-billed on every request.
//
// The host's own boundary is respected. Enable used to overwrite it with "the
// whole prompt is static", which moved every segment the host had declared
// dynamic into the cached prefix: the first time one of them changed, the
// static part in front of it stopped being readable from the cache too.
//
//   - Host boundary inside the prompt: the section goes in at the boundary,
//     which moves one segment right. What was static stays static, what was
//     dynamic stays dynamic, and their order is kept.
//   - No boundary, or one at the end: the section is appended and the
//     boundary set after it, so everything the host passed is static (the
//     default) while segments the session adds later, such as the memory
//     index, stay outside the prefix.
func addStaticSection(opts *agentcore.Options, section string) {
	nSys, total := len(opts.SystemPrompt), len(opts.SystemPrompt)+len(opts.AppendSystemPrompt)
	b := opts.DynamicBoundary
	if b <= 0 || b >= total {
		opts.AppendSystemPrompt = append(opts.AppendSystemPrompt, section)
		opts.DynamicBoundary = total + 1
		return
	}
	if b < nSys {
		// The boundary falls inside SystemPrompt. Copy before inserting: the
		// host's slice must not be edited behind its back.
		sys := make([]string, 0, nSys+1)
		sys = append(sys, opts.SystemPrompt[:b]...)
		sys = append(sys, section)
		opts.SystemPrompt = append(sys, opts.SystemPrompt[b:]...)
	} else {
		at := b - nSys
		app := make([]string, 0, len(opts.AppendSystemPrompt)+1)
		app = append(app, opts.AppendSystemPrompt[:at]...)
		app = append(app, section)
		opts.AppendSystemPrompt = append(app, opts.AppendSystemPrompt[at:]...)
	}
	opts.DynamicBoundary = b + 1
}

// defaultContextLimit is used when the host names no window. It is deliberately
// conservative: guessing high would delay every pressure threshold past the
// point where they could still help.
const defaultContextLimit = 200000

// EnableWithSession is Enable, also returning the session so a host can inspect
// state or archives. The session is owned by the returned wiring; callers must
// not construct a second one over the same directory.
func EnableWithSession(opts *agentcore.Options, o Options) (*Session, error) {
	if err := Enable(opts, o); err != nil {
		return nil, err
	}
	c, ok := opts.Compactor.(*Compactor)
	if !ok {
		return nil, ErrNoArchiveDir
	}
	return c.sess, nil
}

var _ tool.CoreTool = newCompressTool(&Session{cfg: fold.DefaultConfig(1)})
