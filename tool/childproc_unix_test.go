//go:build !windows

package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Credential lands on the command with an empty group list, next to the
// process-group flag the task manager sets.
func TestChildProcessCredentialSet(t *testing.T) {
	withChild(t, &ChildProcess{Credential: &Credential{UID: 4242, GID: 4343}})
	cmd := exec.Command("true")
	setDetached(cmd)
	if err := applyChild(cmd); err != nil {
		t.Fatal(err)
	}
	c := cmd.SysProcAttr.Credential
	if c == nil || c.Uid != 4242 || c.Gid != 4343 || c.Groups == nil || len(c.Groups) != 0 || c.NoSetGroups {
		t.Fatalf("credential: %+v", c)
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Fatal("process-group flag lost")
	}
}

// As root, a Bash child really runs as the configured user (CI containers).
func TestChildProcessCredentialRuns(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to switch users")
	}
	withChild(t, &ChildProcess{Credential: &Credential{UID: 65534, GID: 65534}, CleanEnv: true, Home: "/"})
	raw, _ := json.Marshal(map[string]any{"command": "id -u; id -g; id -G"})
	res, _ := NewBash().Call(context.Background(), raw, &ToolContext{WorkingDir: "/"})
	if out := strings.Fields(res.Flatten()); len(out) != 3 || out[0] != "65534" || out[1] != "65534" || out[2] != "65534" {
		t.Fatalf("child ids: %q", res.Flatten())
	}
}
