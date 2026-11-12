package harness

import (
	"runtime"
	"strings"
	"testing"

	"github.com/0xmanhnv/corvid/permission"
	"github.com/0xmanhnv/corvid/tool"
)

// Policy.ChildEnv "allowlist" (the default) reaches the tools: a Bash call of
// the session does not see a secret of the host process; with "process" and
// no tool.ChildProcess settings installed it does, as before.
func TestPolicyChildEnvReachesBash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell")
	}
	t.Setenv("CHILDENV_TEST_TOKEN", "tok-secret")
	tool.SetChildProcess(nil)
	for _, c := range []struct {
		pol  permission.Policy
		want string
	}{
		{permission.Policy{ChildEnv: permission.ChildEnvAllowlist}, "token=[]"},
		{permission.StrictPolicy(), "token=[]"},
		{permission.Policy{}, "token=[]"},
		{permission.Policy{ChildEnv: permission.ChildEnvProcess}, "token=[tok-secret]"},
	} {
		in := QueryInput{Tools: tool.NewRegistry(tool.NewBash()), PermissionMode: permission.ModeBypass, Policy: c.pol}
		b, _, _ := runOneCall(t, in, tool.BashName, `{"command":"echo token=[$CHILDENV_TEST_TOKEN]"}`)
		if got := b.Content[0].Text; !strings.Contains(got, c.want) {
			t.Fatalf("policy %+v: %q, want %q", c.pol, got, c.want)
		}
	}
}
