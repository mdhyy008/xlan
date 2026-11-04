package mobile

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"xlan/core"
	"xlan/internal/config"
	"xlan/internal/spawn"
)

func TestStartStop(t *testing.T) {
	// 本机已经有 xlan 在跑时跳过，避免抢固定端口。
	l, err := net.ListenPacket("udp4", fmt.Sprintf(":%d", core.DiscoverPort))
	if err != nil {
		t.Skipf("UDP %d 已被占用，跳过", core.DiscoverPort)
	}
	l.Close()

	dir := t.TempDir()
	recv := filepath.Join(dir, "recv")
	ports := freePorts(t, 3)

	cfg := config.Default()
	cfg.APIPorts = ports
	cfg.RecvDir = recv
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	defer func() {
		_ = Stop()
		config.SetDataDir("")
	}()

	base, err := Start(dir, recv, false)
	if err != nil {
		t.Fatal(err)
	}
	if base == "" || !IsRunning() {
		t.Fatalf("服务没有起来: base=%q running=%v", base, IsRunning())
	}
	if got := GetBaseURL(); got != base {
		t.Fatalf("GetBaseURL=%q, want %q", got, base)
	}
	if !spawn.Healthy(t.Context(), base) {
		t.Fatal("本地 API 不可访问")
	}
	if _, err := os.Stat(filepath.Join(recv)); err != nil {
		t.Fatalf("收件目录没有建出来: %v", err)
	}

	// 重复 Start 应该直接返回现有地址，不会起第二份。
	again, err := Start(dir, recv, false)
	if err != nil || again != base {
		t.Fatalf("重复 Start: base=%q err=%v", again, err)
	}

	if err := Stop(); err != nil {
		t.Fatal(err)
	}
	if IsRunning() || GetBaseURL() != "" {
		t.Fatalf("停止后状态没清干净: running=%v base=%q", IsRunning(), GetBaseURL())
	}
	if spawn.Healthy(t.Context(), base) {
		t.Fatal("停止后端口仍然可访问")
	}
}

// freePorts 借几个系统分配的端口，关掉后留给服务用。
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	var listeners []net.Listener
	var ports []int
	for i := 0; i < n; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	for _, l := range listeners {
		l.Close()
	}
	return ports
}
