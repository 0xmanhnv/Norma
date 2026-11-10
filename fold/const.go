package fold

const (
	// RefWidth is the zero-padding FLOOR of a message ref, not a fixed width:
	// m00001 .. m99999 render five digits, and above that refs widen naturally
	// (m100000 .. m9999999), so every ref issued below the old cap is
	// byte-identical while the ceiling lifts. Compare refs by RefToIndex, never
	// as strings: "m100000" sorts before "m99999".
	//
	// Ported from acp-kernel (MIT plus Additional Term, Copyright (c) 2026
	// ranxianglei): src/refs.ts — 83b8609 (v0.0.100, acp-kernel#483).
	RefWidth    = 5
	MinRefIndex = 1
	MaxRefIndex = 9_999_999
	// BlockedRef marks a message that is hard-protected: it gets an entry in
	// ByRaw so it is not revisited, but consumes no ref number and can never be
	// addressed by the model.
	BlockedRef = "BLOCKED"

	// SummaryHeader opens every rendered summary message. isRenderedSummaryMessage
	// tests it together with the id prefix, role and content type — the prefix
	// alone would let a host-authored message be silently deleted by a rebuild.
	SummaryHeader = "[Compressed conversation section]"
	// SummaryIDPrefix + blockID is the synthetic id of a rendered summary.
	SummaryIDPrefix = "noa_summary_"
	// NudgeMessageID is the synthetic id of the injected nudge. It never enters
	// the ref map and never reaches the transcript.
	NudgeMessageID = "noa_nudge"

	// TruncationMarker identifies an emergency-truncated tool result. Detection
	// is a substring test, so the surrounding wording can change freely.
	TruncationMarker = "[truncated for context space"

	// ViableRangeMinTokens filters fragments out of the recommendation list. A
	// tiny range dragged into a batch makes the whole batch fail the character
	// gate, and models tend to retry the identical batch.
	ViableRangeMinTokens = 200
	// KeepLastOrphaned is how many trailing Compress calls with no block are kept
	// (a call that just failed has not formed a block yet).
	KeepLastOrphaned = 2
	// SummaryStubChars is the length a deactivated Compress call's summary
	// argument is stubbed down to.
	SummaryStubChars = 200
	// ToolPairMaxScan bounds how far boundary adjustment looks for the other half
	// of a tool exchange.
	ToolPairMaxScan = 20
	// DeadRepeatReject is how many times the same dead range may be requested
	// before it is rejected outright.
	DeadRepeatReject = 2
)

// CompressToolName is the single source of truth for the tool's name. The
// string appears in at least six places (strip exemption, hide-compress-calls
// matching, pair-adjustment skip, the hard-protected list, log replay scanning,
// schema registration); a literal in any of them would survive a rename and
// fail in a way that is hard to see.
const CompressToolName = "Compress"

// AlwaysProtectedTools can never be compressed, regardless of configuration.
var AlwaysProtectedTools = []string{CompressToolName}

// NeverPreserveRecentTools do not occupy a slot in the "most recent N messages"
// protected zone — they are bulky and inherently re-obtainable. They remain
// fully compressible; this is not the same thing as Config.ProtectedTools,
// which is a hard exclusion.
//
// These are Corvid's tool names (CamelCase), not upstream's lowercase ones. This
// is the default; Config.NeverPreserveRecentTools replaces it for a session and
// Config.PreserveRecentTools subtracts from it.
var NeverPreserveRecentTools = []string{"Read", "Grep", "Glob", "Bash", "LS"}
