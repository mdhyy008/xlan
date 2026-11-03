package serve

import (
	"context"
	"crypto/tls"
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
	"xlan/internal/secure"
	"xlan/internal/spawn"
	"xlan/internal/store"
)

// Run 是常驻服务的主体。所有 socket 都由它持有。
func Run(ctx context.Context, cfgDir string, autoAccept bool) error {
	if err := config.EnsureMigrated(); err != nil {
		return err
	}
	cfg, err := config.Load(cfgDir)
	if err != nil {
		return err
	}
	dataDir, err := config.DataDir()
	if err != nil {
		return err
	}
	// 加密开关要在任何探测之前定下来。
	if cfg.UseTLS() {
		secure.Enable(secure.LoadPins(filepath.Join(dataDir, "pins.json")))
	} else {
		secure.Disable()
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

	// 清掉历史版本可能把本机当设备写进去的记录。
	_ = st.DeletePeer(dev.ID)

	bus := events.New()

	// 端口即锁：谁先绑上谁就是服务。已经有一个在跑就别再起。
	if base := spawn.Find(context.Background(), cfg); base != "" {
		fmt.Printf("xlan 服务已经在运行了：%s\n不用再起一个，直接用 xlan scan / send / text。\n", base)
		return nil
	}
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
		base, probeErr := spawn.FindErr(context.Background(), cfg)
		if base != "" {
			fmt.Printf("xlan 服务已经在运行了：%s\n不用再起一个。\n", base)
			return nil
		}
		if probeErr != nil {
			return probeErr
		}
		return errors.New("所有 API 端口都被占用了")
	}
	defer ln.Close()

	fingerprint := ""
	if cfg.UseTLS() {
		cert, fp, err := secure.LoadOrCreateCert(dataDir)
		if err != nil {
			return fmt.Errorf("准备加密证书失败: %w", err)
		}
		fingerprint = fp
		ln = tls.NewListener(ln, secure.ServerTLS(cert))
	}

	disc := discover.New(dev, port, st, bus)
	if err := bindUDP(func() error { return disc.Listen(net.IPv4zero, core.DiscoverPort) }); err != nil {
		ln.Close()
		if base := spawn.Find(context.Background(), cfg); base != "" {
			fmt.Printf("xlan 服务已经在运行了：%s\n", base)
			return nil
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
		if base := spawn.Find(context.Background(), cfg); base != "" {
			fmt.Printf("xlan 服务已经在运行了：%s\n", base)
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
		RecvDir:     func() string { return recvDir },
		StartedAt:   time.Now(),
		AutoAccept:  autoAccept,
		Fingerprint: fingerprint,
		Shutdown:    cancel,
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

	fmt.Printf("--------------- xlan 服务已启动 ---------------\n")
	fmt.Printf("  版本   %s\n", core.VersionString())
	fmt.Printf("  设备   %s (%s)\n", dev.Name, dev.ID)
	fmt.Printf("  接口   http://0.0.0.0:%d\n", port)
	fmt.Printf("  发现   udp %d\n", core.DiscoverPort)
	fmt.Printf("  公共   udp %d\n", core.PublicPort)
	fmt.Printf("  收件   %s\n", recvDir)
	if autoAccept {
		fmt.Printf("  模式   自动接收（任何人的文件都直接收下）\n")
	}
	if fingerprint != "" {
		fmt.Printf("  加密   TLS（指纹 %s）\n", fingerprint[:16])
	} else {
		fmt.Printf("  加密   未启用（明文）\n")
	}
	fmt.Printf("  数据   %s\n", dataDir)
	fmt.Printf("--------------------------------------------\n")
	fmt.Printf("按 Ctrl-C 停止。另开一个终端用 xlan scan / send / text。\n\n")

	errCh := make(chan error, 1)
	go func() { errCh <- hs.Serve(ln) }()

	// 定期清理对方断线后挂着的传输。
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				if n := srv.ReapStale(); n > 0 {
					fmt.Printf("清理了 %d 个长时间没有数据的传输\n", n)
				}
			}
		}
	}()

	disc.Announce()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-runCtx.Done():
			shutdown(hs)
			return nil
		case <-restartCh:
			fmt.Println("配置有变更，正在重启服务…")
			// 先放掉所有监听端口，再拉新进程，避免新进程绑不上。
			shutdown(hs)
			_ = disc.Close()
			_ = pub.Close()
			_ = spawn.Start(cfgDir, autoAccept)
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
