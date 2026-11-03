//go:build !windows

package spawn

import (
	"os/exec"
	"syscall"
)

// detach 让子进程脱离当前终端，Ctrl-C 不会带走它。
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
