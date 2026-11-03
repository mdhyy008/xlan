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
	if err := disc.Listen(net.IPv4zero, core.DiscoverPort); err != nil {
		return fmt.Errorf("绑定 UDP %d 失败: %w", core.DiscoverPort, err)
	}
	defer disc.Close()

	pub, err := pubtext.Listen(net.IPv4zero, core.PublicPort, dev.ID, dev.Name)
	if err != nil {
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

	srv := &httpapi.Server{
		Dev: dev, Port: port, Store: st, Disc: disc, Pub: pub, Bus: bus,
		RecvDir: cfg.RecvDirPath(), StartedAt: time.Now(),
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

	writeRuntime(dataDir, port, dev)
	defer os.Remove(runtimePath(dataDir))

	errCh := make(chan error, 1)
	go func() { errCh <- hs.Serve(ln) }()

	disc.Announce()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			shutdown(hs)
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
