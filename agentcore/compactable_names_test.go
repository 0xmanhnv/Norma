package agentcore

import (
	"testing"

	"github.com/0xmanhnv/corvid/compaction"
	"github.com/0xmanhnv/corvid/tool"
)

// Every tool MicroCompact may clear must exist under that exact name in the
// default toolset or among the opt-in web tools; a name that matches nothing silently stops its results
// from ever being cleared (web search was listed as "WebSearch" while the tool
// is "web_search").
func TestCompactableToolsAreRealToolNames(t *testing.T) {
	have := map[string]bool{}
	for _, ct := range tool.DefaultTools() {
		have[ct.Name()] = true
	}
	// The web tools are opt-in, so they are not in the default set.
	have[tool.NewWebFetch(tool.WebFetchConfig{}).Name()] = true
	ws, err := tool.NewWebSearch(tool.WebSearchConfig{}) // ddgs: no key, no network at construction
	if err != nil {
		t.Fatalf("NewWebSearch: %v", err)
	}
	have[ws.Name()] = true
	for _, n := range compaction.CompactableTools() {
		if !have[n] {
			t.Errorf("compaction lists %q, which is not a default tool name", n)
		}
	}
}
