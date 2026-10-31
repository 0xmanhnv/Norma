//go:build windows

package tool

import (
	"errors"
	"os/exec"
)

// setCredential: switching users is not supported on Windows; refuse rather
// than run the child unconfined.
func setCredential(_ *exec.Cmd, _ Credential) error {
	return errors.New("child process credential is not supported on Windows")
}
