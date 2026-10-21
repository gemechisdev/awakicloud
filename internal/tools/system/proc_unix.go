//go:build unix

package system

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the child in its own process group so cancel can
// kill the whole tree (Linux/macOS).
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
