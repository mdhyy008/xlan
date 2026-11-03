package serve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"xlan/core"
	"xlan/internal/config"
	"xlan/internal/discover"
	"xlan/internal/events"
	"xlan/internal/httpapi"
	"xlan/internal/pubtext"
	"xlan/internal/spawn"
	"xlan/internal/store"
)

// Run 是常驻服务的主体。所有 socket 都由它持有。
func Run(ctx context.Context, cfgDir string) error {
	cfg, err := config.Load(cfgDir)
	if err != nil {
		return err
	}
	dataDir, err := config.DataDir()
	if err != nil {
		return err
	}
	dev, err := core.LoadDevice(dataDir, cfg.Name)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(dataDir, "xlan.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	bus := events.New()

	// 端口即锁：谁先绑上谁就是服务。
	var (
		ln   net.Listener
		port int
	)
	for _, p := range cfg.APIPorts {
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err == nil {
			ln, port = l, p
			break
		}
	}
	if ln == nil {
		if spawn.Find(context.Background(), cfg) != "" {
			return nil // 已有实例，静默退出
		}
		return errors.New("所有 API 端口都被占用了")
	}
	defer ln.Close()

	disc := discover.New(dev, port, st, bus)
	if err := bindUDP(func() error { return disc.Listen(net.IPv4zero, core.DiscoverPort) }); err != nil {
		ln.Close()
		if spawn.Find(context.Background(), cfg) != "" {
			return nil // 重启交接：旧实例还占着端口，它会负责继续服务
		}
		return fmt.Errorf("绑定 UDP %d 失败: %w", core.DiscoverPort, err)
	}
	defer disc.Close()

	var pub *pubtext.Broadcaster
	if err := bindUDP(func() error {
		b, err := pubtext.Listen(net.IPv4zero, core.PublicPort, dev.ID, dev.Name)
		if err == nil {
			pub = b
		}
		return err
	}); err != nil {
		if spawn.Find(context.Background(), cfg) != "" {
			return nil
		}
		return fmt.Errorf("绑定 UDP %d 失败: %w", core.PublicPort, err)
	}
	defer pub.Close()

	pub.OnMsg = func(p core.Packet, from *net.UDPAddr) {
		it := core.Item{
			ID: p.ID, Kind: core.KindText, FromID: p.SID, FromName: p.SName,
			ToID: core.PublicTo, Channel: core.ChanPublic, Body: p.Body,
			Status: core.StatusDone, CreatedAt: p.Ts,
		}
		if it.CreatedAt == 0 {
			it.CreatedAt = time.Now().Unix()
		}
		if err := st.AddItem(it); err != nil {
			return
		}
		bus.Publish(core.Event{Type: core.EvItemNew, Ts: it.CreatedAt, Data: it})
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 收件目录在启动时定下来，传输中途不会换地方，避免续传对不上。
	recvDir := cfg.RecvDirPath()
	restartCh := make(chan struct{}, 1)

	srv := &httpapi.Server{
		Dev: dev, Port: port, Store: st, Disc: disc, Pub: pub, Bus: bus,
		RecvDir:   func() string { return recvDir },
		StartedAt: time.Now(),
		Shutdown:  cancel,
		RequestRestart: func() {
			select {
			case restartCh <- struct{}{}:
			default:
			}
		},
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

	writeRuntime(dataDir, port, dev)
	defer os.Remove(runtimePath(dataDir))

	fmt.Printf("xlan %s 已启动\n  设备  %s (%s)\n  接口  http://0.0.0.0:%d\n  发现  udp %d / 公共文本 udp %d\n  收件  %s\n",
		core.Version, dev.Name, dev.ID, port, core.DiscoverPort, core.PublicPort, cfg.RecvDirPath())

	errCh := make(chan error, 1)
	go func() { errCh <- hs.Serve(ln) }()

	disc.Announce()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-runCtx.Done():
			shutdown(hs)
			return nil
		case <-restartCh:
			// 先放掉所有监听端口，再拉新进程，避免新进程绑不上。
			shutdown(hs)
			_ = disc.Close()
			_ = pub.Close()
			_ = spawn.RestartSelf(cfgDir)
			return nil
		case err := <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ticker.C:
			disc.Announce()
		}
	}
}

func shutdown(hs *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = hs.Shutdown(ctx)
}

func runtimePath(dataDir string) string {
	return filepath.Join(dataDir, "runtime.json")
}

func writeRuntime(dataDir string, port int, dev core.Device) {
	b := fmt.Sprintf("{\"port\":%d,\"pid\":%d,\"id\":%q,\"name\":%q}\n",
		port, os.Getpid(), dev.ID, dev.Name)
	_ = os.WriteFile(runtimePath(dataDir), []byte(b), 0o644)
}

// bindUDP 在重启交接时端口可能还被旧进程占着，重试一小会儿。
func bindUDP(fn func() error) error {
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := fn()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(150 * time.Millisecond)
	}
}
