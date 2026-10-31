//go:build !windows

package tool

import (
	"os/exec"
	"syscall"
)

// setCredential runs cmd as c with an empty supplementary group list (the
// child must not keep the parent's groups).
func setCredential(cmd *exec.Cmd, c Credential) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: c.UID, Gid: c.GID, Groups: []uint32{}}
	return nil
}
