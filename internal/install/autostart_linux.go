//go:build linux

package install

import (
	"fmt"
	"os"
	"path/filepath"
)

// 开机自启：systemd 用户服务。

func unitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", "xlan.service"), nil
}

func AutostartInstall(exe string) (string, error) {
	p, err := unitPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	unit := fmt.Sprintf(`[Unit]
Description=xlan 局域网传输服务
After=network-online.target

[Service]
Type=simple
ExecStart=%s serve
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
`, exe)
	if err := os.WriteFile(p, []byte(unit), 0o644); err != nil {
		return "", err
	}
	if _, err := run("systemctl", "--user", "enable", "xlan.service"); err != nil {
		return "", fmt.Errorf("单元文件已写到 %s，但当前环境没有可用的 systemd，没能启用", p)
	}
	_, _ = run("systemctl", "--user", "start", "xlan.service")
	return fmt.Sprintf("已启用 systemd 用户服务：%s", p), nil
}

func AutostartRemove(_ string) (string, error) {
	p, err := unitPath()
	if err != nil {
		return "", err
	}
	_, _ = run("systemctl", "--user", "disable", "--now", "xlan.service")
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return "本来就没设置开机自启", nil
		}
		return "", err
	}
	return "已取消开机自启", nil
}

func AutostartStatus() (bool, string, error) {
	p, err := unitPath()
	if err != nil {
		return false, "", err
	}
	if _, err := os.Stat(p); err != nil {
		return false, "未设置", nil
	}
	state := "未知"
	if out, err := run("systemctl", "--user", "is-enabled", "xlan.service"); err == nil {
		state = out
	} else if out != "" {
		state = out
	}
	return true, fmt.Sprintf("systemd 用户服务 %s（enable 状态：%s）", p, state), nil
}
