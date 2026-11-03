//go:build !windows

package cli

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// pauser 用 Ctrl-Z 暂停/继续传输，而不是把进程挂起。
type pauser struct {
	mu     sync.Mutex
	cond   *sync.Cond
	paused bool
}

func newPauser() *pauser {
	p := &pauser{}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// Watch 接管三个信号：
//
//	SIGTSTP（Ctrl-Z）在暂停/继续之间切换
//	SIGUSR1 暂停，SIGUSR2 继续（供 xlan pause / resume 用）
func (p *pauser) Watch(onToggle func(bool)) {
	tstp := make(chan os.Signal, 1)
	signal.Notify(tstp, syscall.SIGTSTP)
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	usr2 := make(chan os.Signal, 1)
	signal.Notify(usr2, syscall.SIGUSR2)

	go func() {
		for {
			select {
			case <-tstp:
				p.set(!p.Paused(), onToggle)
			case <-usr1:
				p.set(true, onToggle)
			case <-usr2:
				p.set(false, onToggle)
			}
		}
	}()
}

func (p *pauser) set(v bool, onToggle func(bool)) {
	p.mu.Lock()
	changed := p.paused != v
	p.paused = v
	if !v {
		p.cond.Broadcast()
	}
	p.mu.Unlock()
	if changed && onToggle != nil {
		onToggle(v)
	}
}

// signalSend 给某个进程发暂停/继续信号。
func signalSend(pid int, pause bool) error {
	sig := syscall.SIGUSR1
	if !pause {
		sig = syscall.SIGUSR2
	}
	return syscall.Kill(pid, sig)
}

// Wait 在暂停时阻塞。
func (p *pauser) Wait() {
	p.mu.Lock()
	for p.paused {
		p.cond.Wait()
	}
	p.mu.Unlock()
}

func (p *pauser) Paused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}
