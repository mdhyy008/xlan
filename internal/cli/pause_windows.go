//go:build windows

package cli

import "errors"

// Windows 上没有 SIGTSTP/SIGUSR，暂停功能不可用，保持空实现。
type pauser struct{}

func newPauser() *pauser { return &pauser{} }

func (p *pauser) Watch(onToggle func(bool)) {}

func (p *pauser) Wait() {}

func (p *pauser) Paused() bool { return false }

// signalSend 在 Windows 上不支持。
func signalSend(pid int, pause bool) error {
	return errors.New("当前系统不支持暂停传输")
}
