package memory

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/0xmanhnv/corvid/permission"
)

// RecordMemory asks by default and in the strict policy; MemoryWrites "allow"
// is the behaviour before.
func TestRecordMemoryPolicy(t *testing.T) {
	rec := RecordTool(NewStore(t.TempDir()))
	in := json.RawMessage(`{"name":"n","content":"c"}`)
	for pol, want := range map[string]permission.Behavior{"": permission.Ask, "strict": permission.Ask, "compatible": permission.Allow} {
		pc := permission.Context{Mode: permission.ModeDefault}
		switch pol {
		case "strict":
			pc.Policy = permission.StrictPolicy()
		case "compatible":
			pc.Policy = permission.CompatiblePolicy()
		}
		if d := rec.CheckPermissions(context.Background(), in, pc); d.Behavior != want {
			t.Errorf("policy %q: %s, want %s", pol, d.Behavior, want)
		}
	}
}
