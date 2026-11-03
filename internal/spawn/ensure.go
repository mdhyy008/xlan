package spawn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"

	"xlan/internal/config"
)

// Healthy 判断某个地址上是否已经有 xlan 服务在跑。
func Healthy(ctx context.Context, base string) bool {
	ctx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/info", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
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

// Ensure 保证本机服务在跑，返回它的地址；不在跑就后台拉起来。
func Ensure(ctx context.Context, cfg *config.Config, cfgDir string) (string, error) {
	if base := Find(ctx, cfg); base != "" {
		return base, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	args := []string{"serve", "--detach"}
	if cfgDir != "" {
		args = append(args, "--config", cfgDir)
	}
	cmd := exec.Command(exe, args...)
	detach(cmd)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return "", err
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if base := Find(ctx, cfg); base != "" {
			return base, nil
		}
		time.Sleep(120 * time.Millisecond)
	}
	return "", errors.New("xlan 服务没有启动起来，试试手动跑 `xlan serve`")
}

// RestartSelf 拉起一个新的 serve 进程，用于重启自己。
func RestartSelf(cfgDir string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"serve", "--detach"}
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
