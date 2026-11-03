//go:build !windows

package cli

import "syscall"

func pidAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}
