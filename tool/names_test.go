package tool

import "testing"

// TestToolNamesAreStable pins the name each built-in tool goes by. Hosts store
// these names (allow lists, permission rules, transcripts), so a change here is
// a breaking change and must be deliberate.
func TestToolNamesAreStable(t *testing.T) {
	ws, err := NewWebSearch(WebSearchConfig{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		tool CoreTool
		want string
	}{
		{NewRead(), "Read"},
		{NewWrite(), "Write"},
		{NewEdit(), "Edit"},
		{NewMultiEdit(), "MultiEdit"},
		{NewLS(), "LS"},
		{NewGlob(), "Glob"},
		{NewGrep(), "Grep"},
		{NewBash(), "Bash"},
		{NewSleep(), "sleep"},
		{NewMonitor(), "Monitor"},
		{NewTaskOutput(), "TaskOutput"},
		{NewTaskStop(), "TaskStop"},
		{NewTaskList(), "TaskList"},
		{NewTodoStore().Tool(), "TodoWrite"},
		{NewAskUserQuestion(nil), "AskUserQuestion"},
		{NewWebFetch(WebFetchConfig{}), "WebFetch"},
		{ws, "web_search"},
		{NewShellOpen(), "shell_open"},
		{NewShellSend(), "shell_send"},
		{NewShellRead(), "shell_read"},
		{NewShellClose(), "shell_close"},
		{NewShellList(), "shell_list"},
		{NewSearchExtraTools(NewRegistry(), nil), "SearchExtraTools"},
		{NewExecuteExtraTool(NewRegistry(), NewUnlockSet()), "ExecuteExtraTool"},
	}
	for _, c := range cases {
		if got := c.tool.Name(); got != c.want {
			t.Errorf("tool name %q, want %q", got, c.want)
		}
	}
}
