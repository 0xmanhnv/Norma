package tool

// Names of the built-in tools, as the model sees them. Hosts that match on a
// tool name (permission rules, hooks, allow and deny lists) should use these
// constants instead of string literals, so a reference that drifts from the
// tool fails to compile instead of silently matching nothing.
//
// SearchExtraToolsName and ExecuteExtraToolName are declared in deferred.go.
const (
	ReadName            = "Read"
	WriteName           = "Write"
	EditName            = "Edit"
	MultiEditName       = "MultiEdit"
	LSName              = "LS"
	GlobName            = "Glob"
	GrepName            = "Grep"
	BashName            = "Bash"
	SleepName           = "sleep"
	MonitorName         = "Monitor"
	TaskOutputName      = "TaskOutput"
	TaskStopName        = "TaskStop"
	TaskListName        = "TaskList"
	TodoWriteName       = "TodoWrite"
	AskUserQuestionName = "AskUserQuestion"
	WebFetchName        = "WebFetch"
	WebSearchName       = "web_search"

	// The interactive PTY session tools (ShellSessionTools).
	ShellOpenName  = "shell_open"
	ShellSendName  = "shell_send"
	ShellReadName  = "shell_read"
	ShellCloseName = "shell_close"
	ShellListName  = "shell_list"
)
