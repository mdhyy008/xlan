package spawn

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"xlan/internal/config"
	"xlan/internal/secure"
)

var plainProbe = &http.Client{Timeout: 1200 * time.Millisecond}

func probeHTTP() *http.Client {
	if secure.Enabled() {
		return &http.Client{
			Timeout:   1200 * time.Millisecond,
			Transport: secure.Transport(secure.PinsOf()),
		}
	}
	return plainProbe
}

// Healthy 判断某个地址上是否已经有 xlan 服务在跑。
func Healthy(ctx context.Context, base string) bool {
	ok, _ := healthyErr(ctx, base)
	return ok
}

// healthyErr 除了「通不通」，还把证书指纹不匹配这类错误带回来。
func healthyErr(ctx context.Context, base string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 900*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, secure.Rewrite(base)+"/v1/info", nil)
	if err != nil {
		return false, nil
	}
	resp, err := probeHTTP().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

// FindErr 在找不到服务时，把最后一次探测的错误带回来（比如指纹变了）。
func FindErr(ctx context.Context, cfg *config.Config) (string, error) {
	var lastErr, betterErr error
	for _, p := range cfg.APIPorts {
		base := fmt.Sprintf("http://127.0.0.1:%d", p)
		ok, err := healthyErr(ctx, base)
		if ok {
			return base, nil
		}
		if err == nil {
			continue
		}
		lastErr = err
		// 端口没人监听是最没信息量的，优先把证书这类错误带回去
		if betterErr == nil && !strings.Contains(err.Error(), "connection refused") &&
			!strings.Contains(err.Error(), "connect: connection refused") {
			betterErr = err
		}
	}
	if betterErr != nil {
		return "", betterErr
	}
	return "", lastErr
}

// Find 返回已在运行的服务地址，没有则返回空串。
func Find(ctx context.Context, cfg *config.Config) string {
	for _, p := range cfg.APIPorts {
		base := fmt.Sprintf("http://127.0.0.1:%d", p)
		if Healthy(ctx, base) {
			return base
		}
	}
	return ""
}

// Start 在后台拉起一个 serve 进程（脱离终端）。
// 既用于 xlan serve -d，也用于服务自我重启。
func Start(cfgDir string, autoAccept bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"serve"}
	if autoAccept {
		args = append(args, "--auto-accept")
	}
	if cfgDir != "" {
		args = append(args, "--config", cfgDir)
	}
	cmd := exec.Command(exe, args...)
	detach(cmd)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
