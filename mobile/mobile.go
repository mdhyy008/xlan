// Package mobile 供 Android 通过 gomobile bind 在应用进程内启动 xlan 服务。
//
// 这里只负责生命周期：起服务、停服务、拿本地地址。发现、发送、收件箱等
// 业务操作仍然走服务暴露的本地 HTTP API，和命令行客户端完全一致。
package mobile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"xlan/core"
	"xlan/internal/config"
	"xlan/internal/serve"
	"xlan/internal/spawn"
)

// Logger 接收服务日志，由宿主实现。
type Logger interface {
	Log(msg string)
}

var (
	// startMu 串行化 Start/Stop，保证不会同时拉起两份服务。
	startMu sync.Mutex

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	baseURL string
	running bool
	logSink Logger
)

// SetLogger 设置日志接收器，nil 表示丢弃日志。
func SetLogger(l Logger) {
	mu.Lock()
	logSink = l
	mu.Unlock()
}

// Version 返回当前二进制版本，与 xlan --version 一致。
func Version() string { return core.VersionString() }

// GetBaseURL 返回本地 API 地址，例如 http://127.0.0.1:43003；未运行时为空。
func GetBaseURL() string {
	mu.Lock()
	defer mu.Unlock()
	return baseURL
}

// IsRunning 表示服务是否已经起来并可以访问。
func IsRunning() bool {
	mu.Lock()
	defer mu.Unlock()
	return running
}

// Start 在进程内启动服务，返回本地 API 地址。
//
// dataDir 用来放设备 id、数据库、证书和配置，必须是宿主可写的私有目录；
// recvDir 为空时按配置决定（Android 上建议显式传应用私有目录）；
// autoAccept 为 true 时不等待确认，直接接收所有人的文件。
//
// 重复调用是安全的：已经在运行时直接返回现有地址。
func Start(dataDir string, recvDir string, autoAccept bool) (string, error) {
	startMu.Lock()
	defer startMu.Unlock()

	mu.Lock()
	if running && baseURL != "" {
		b := baseURL
		mu.Unlock()
		return b, nil
	}
	mu.Unlock()

	if dataDir == "" {
		return "", errors.New("dataDir 不能为空")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", err
	}
	if recvDir != "" {
		if err := os.MkdirAll(recvDir, 0o755); err != nil {
			return "", err
		}
	}

	config.SetDataDir(dataDir)

	ctx, cancelFn := context.WithCancel(context.Background())
	doneCh := make(chan struct{})

	mu.Lock()
	cancel, done, running, baseURL = cancelFn, doneCh, true, ""
	mu.Unlock()

	opts := serve.Options{
		ConfigDir:  dataDir,
		AutoAccept: autoAccept,
		RecvDir:    recvDir,
		Logf: func(format string, args ...any) {
			mu.Lock()
			sink := logSink
			mu.Unlock()
			if sink != nil {
				sink.Log(fmt.Sprintf(format, args...))
			}
		},
	}

	go func() {
		defer close(doneCh)
		for {
			err := serve.RunWith(ctx, opts)
			if ctx.Err() != nil {
				return
			}
			// 改配置触发的重启在同一个进程里完成，不 exec 新进程。
			if errors.Is(err, serve.ErrRestartRequested) {
				continue
			}
			// nil 表示端口上已经有服务在跑，状态留给 Start 去填。
			if err != nil {
				mu.Lock()
				running = false
				baseURL = ""
				mu.Unlock()
				opts.Logf("xlan 服务退出：%v\n", err)
			}
			return
		}
	}()

	cfg, err := config.Load(dataDir)
	if err != nil {
		cancelFn()
		mu.Lock()
		running = false
		mu.Unlock()
		return "", err
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if b := spawn.Find(context.Background(), cfg); b != "" {
			// 返回宿主可以直接请求的地址：默认 TLS 时给出 https。
			if cfg.UseTLS() {
				b = "https://" + strings.TrimPrefix(b, "http://")
			}
			mu.Lock()
			baseURL = b
			mu.Unlock()
			return b, nil
		}
		select {
		case <-doneCh:
			mu.Lock()
			running = false
			mu.Unlock()
			return "", errors.New("xlan 服务启动失败")
		case <-time.After(100 * time.Millisecond):
		}
	}

	cancelFn()
	mu.Lock()
	running = false
	mu.Unlock()
	return "", errors.New("xlan 服务启动超时")
}

// Stop 停止服务并释放端口。没有在跑时也返回 nil。
func Stop() error {
	startMu.Lock()
	defer startMu.Unlock()

	mu.Lock()
	if !running {
		mu.Unlock()
		return nil
	}
	cancelFn, doneCh := cancel, done
	mu.Unlock()

	cancelFn()
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		return errors.New("停止服务超时")
	}

	mu.Lock()
	running = false
	baseURL = ""
	mu.Unlock()
	return nil
}
