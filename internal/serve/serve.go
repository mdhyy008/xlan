package serve

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// Options 描述一次服务运行。零值等价于 Run(ctx, "", false)。
type Options struct {
	ConfigDir  string
	AutoAccept bool
	// RecvDir 非空时覆盖配置里的收件目录，嵌入式宿主用来指定私有目录。
	RecvDir string
	// Logf 非空时替代默认的 stdout 输出，宿主可以接到自己的日志里。
	Logf func(format string, args ...any)
}

// ErrRestartRequested 表示需要重启。桌面端由 Run 换进程，
// 嵌入式宿主可以在同一个进程里重新调用 RunWith。
var ErrRestartRequested = errors.New("restart requested")

// Run 是常驻服务的主体。所有 socket 都由它持有。
func Run(ctx context.Context, cfgDir string, autoAccept bool) error {
	err := RunWith(ctx, Options{ConfigDir: cfgDir, AutoAccept: autoAccept})
	if errors.Is(err, ErrRestartRequested) {
		// 桌面端换一个新进程，避免旧进程里残留的连接和状态。
		_ = spawn.Start(cfgDir, autoAccept)
		return nil
	}
	return err
}

// RunWith 与 Run 相同，但允许宿主注入收件目录和日志输出。
func RunWith(ctx context.Context, opts Options) error {
	cfgDir := opts.ConfigDir
	autoAccept := opts.AutoAccept
	logf := opts.Logf
	if logf == nil {
		logf = func(format string, args ...any) { fmt.Printf(format, args...) }
	}
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
		logf("xlan 服务已经在运行了：%s\n不用再起一个，直接用 xlan scan / send / text。\n", base)
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
			logf("xlan 服务已经在运行了：%s\n不用再起一个。\n", base)
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
			logf("xlan 服务已经在运行了：%s\n", base)
			return nil
		}
		return fmt.Errorf("绑定 UDP %d 失败: %w", core.DiscoverPort, err)
	}
	defer disc.Close()
	// 正常退出时广播下线，其他设备立即把本机标为离线；
	// 配置变更触发的重启不广播，避免对端看到一次假下线。
	sayBye := true
	defer func() {
		if sayBye {
			disc.SayBye()
		}
	}()

	var pub *pubtext.Broadcaster
	if err := bindUDP(func() error {
		b, err := pubtext.Listen(net.IPv4zero, core.PublicPort, dev.ID, dev.Name)
		if err == nil {
			pub = b
		}
		return err
	}); err != nil {
		if base := spawn.Find(context.Background(), cfg); base != "" {
			logf("xlan 服务已经在运行了：%s\n", base)
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
	if opts.RecvDir != "" {
		recvDir = opts.RecvDir
	}
	// 上次留下的空暂存目录顺手收掉；里面还有分片就留着，等续传或清理。
	_ = os.Remove(filepath.Join(recvDir, ".parts"))
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

	logf("--------------- xlan 服务已启动 ---------------\n")
	logf("  版本   %s\n", core.VersionString())
	logf("  设备   %s (%s)\n", dev.Name, dev.ID)
	logf("  接口   http://0.0.0.0:%d\n", port)
	if ips := localIPv4s(); len(ips) > 0 {
		addrs := make([]string, 0, len(ips))
		for _, ip := range ips {
			addrs = append(addrs, fmt.Sprintf("%s:%d", ip, port))
		}
		logf("  内网   %s\n", strings.Join(addrs, "，"))
	}
	logf("  发现   udp %d\n", core.DiscoverPort)
	logf("  公共   udp %d\n", core.PublicPort)
	logf("  收件   %s\n", recvDir)
	if autoAccept {
		logf("  模式   自动接收（任何人的文件都直接收下）\n")
	}
	if fingerprint != "" {
		logf("  加密   TLS（指纹 %s）\n", fingerprint[:16])
	} else {
		logf("  加密   未启用（明文）\n")
	}
	logf("  数据   %s\n", dataDir)
	logf("--------------------------------------------\n")
	logf("按 Ctrl-C 停止。另开一个终端用 xlan scan / send / text。\n\n")

	errCh := make(chan error, 1)
	go func() { errCh <- hs.Serve(ln) }()

	// 前台跑的时候，收到东西直接在终端说一声。
	go watchIncoming(bus, runCtx, dev.ID, logf)

	// 定期清理对方断线后挂着的传输。
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				if n := srv.ReapStale(); n > 0 {
					logf("清理了 %d 个长时间没有数据的传输\n", n)
				}
			}
		}
	}()

	disc.Announce()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-runCtx.Done():
			shutdown(hs)
			return nil
		case <-restartCh:
			sayBye = false
			logf("配置有变更，正在重启服务…\n")
			// 先放掉所有监听端口，避免新实例绑不上。
			shutdown(hs)
			_ = disc.Close()
			_ = pub.Close()
			return ErrRestartRequested
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

// localIPv4s 返回本机的局域网 IPv4 地址。有私有地址时优先只给私有地址，
// 避免把 Docker、虚拟网卡之类的地址混进来。
func localIPv4s() []string {
	var all, private []string
	seen := map[string]bool{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLinkLocalUnicast() {
				continue
			}
			s := ip4.String()
			if seen[s] {
				continue
			}
			seen[s] = true
			all = append(all, s)
			if ip4.IsPrivate() {
				private = append(private, s)
			}
		}
	}
	if len(private) > 0 {
		return private
	}
	return all
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

// watchIncoming 在前台终端里提示收到的东西，后台跑时输出到 /dev/null 无副作用。
func watchIncoming(bus *events.Bus, ctx context.Context, selfID string, logf func(string, ...any)) {
	ch, cancel := bus.Subscribe()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			it, ok := asItem(ev.Data)
			if !ok || it.Kind == core.KindText {
				continue
			}
			// doctor 自测是自己发给自己，不算“收到别人的文件”。
			if it.FromID == selfID {
				continue
			}
			ts := time.Now().Format("15:04:05")
			name := it.FromName
			if name == "" {
				name = "对方"
			}
			switch ev.Type {
			case core.EvItemNew:
				if it.Status == core.StatusPending {
					logf("[%s] 收到 %s 的文件：%s（%s），等待确认 —— 执行 xlan inbox accept\n",
						ts, name, it.Name, core.FormatBytes(it.Size))
				} else {
					logf("[%s] %s 发来 %s（%s），已自动接收\n",
						ts, name, it.Name, core.FormatBytes(it.Size))
				}
			case core.EvItemUpdate:
				if it.Status == core.StatusDone {
					logf("[%s] 已收完：%s → %s\n", ts, it.Name, it.Path)
				}
			}
		}
	}
}

func asItem(v any) (core.Item, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return core.Item{}, false
	}
	var it core.Item
	if err := json.Unmarshal(b, &it); err != nil || it.ID == "" {
		return core.Item{}, false
	}
	return it, true
}
