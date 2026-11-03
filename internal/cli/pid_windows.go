//go:build windows

package cli

// Windows 上没法可靠探测进程存活，交给退出时清理。
func pidAlive(pid int) bool { return pid > 0 }
