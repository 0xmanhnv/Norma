package foldadapter

import (
	"reflect"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/agentcore"
	"github.com/0xmanhnv/corvid/tool"
)

func systemOf(o agentcore.Options) []string {
	return append(append([]string{}, o.SystemPrompt...), o.AppendSystemPrompt...)
}

// Enable used to set DynamicBoundary to the end of the prompt whatever the host
// had chosen, which put the host's dynamic segments inside the cached prefix.
// It must keep the host's split: static stays static, dynamic stays dynamic,
// and fold's own (static) section lands in the prefix.
func TestEnableKeepsHostBoundary(t *testing.T) {
	cases := []struct {
		name            string
		system, appendS []string
		boundary        int
		wantStatic      int // segments in the prefix besides fold's
	}{
		{"boundary between SystemPrompt and AppendSystemPrompt", []string{"rules", "style"}, []string{"today's notes"}, 2, 2},
		{"boundary inside SystemPrompt", []string{"rules", "per-user"}, []string{"notes"}, 1, 1},
		{"boundary inside AppendSystemPrompt", []string{"rules"}, []string{"static extra", "notes"}, 2, 2},
		{"no boundary: everything the host passed is static", []string{"rules"}, []string{"extra"}, 0, 2},
		{"boundary at the end", []string{"rules"}, []string{"extra"}, 2, 2},
		{"boundary past the end", []string{"rules"}, nil, 9, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sysBefore := append([]string{}, c.system...)
			opts := agentcore.Options{SystemPrompt: c.system, AppendSystemPrompt: c.appendS, DynamicBoundary: c.boundary, Tools: []tool.CoreTool{}}
			before := systemOf(opts)
			if err := Enable(&opts, Options{ArchiveBaseDir: t.TempDir(), SessionID: "s"}); err != nil {
				t.Fatal(err)
			}
			after := systemOf(opts)
			if len(after) != len(before)+1 {
				t.Fatalf("%d segments after Enable, want %d", len(after), len(before)+1)
			}
			b := opts.DynamicBoundary
			if b != c.wantStatic+1 {
				t.Fatalf("boundary %d, want %d (the host's static segments plus fold's)", b, c.wantStatic+1)
			}
			// The prefix is the host's static segments followed by fold's
			// section; everything after it is the host's dynamic segments, in
			// their order.
			if !reflect.DeepEqual(after[:c.wantStatic], before[:c.wantStatic]) {
				t.Fatalf("static prefix changed: %q", after[:b])
			}
			if !strings.Contains(after[b-1], "Compress") {
				t.Fatalf("fold's section is not the last static segment: %q", after[b-1])
			}
			if !reflect.DeepEqual(after[b:], before[c.wantStatic:]) {
				t.Fatalf("dynamic segments changed or moved into the prefix: %q, want %q", after[b:], before[c.wantStatic:])
			}
			if !reflect.DeepEqual(c.system, sysBefore) {
				t.Fatal("Enable edited the host's SystemPrompt slice in place")
			}
		})
	}
}
