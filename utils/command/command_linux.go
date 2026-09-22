package command

import (
	"os/exec"
	"syscall"
)

// killGroupOnCancel runs a script in its own process group, so that its
// timeout kills whatever the script started along with it.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
