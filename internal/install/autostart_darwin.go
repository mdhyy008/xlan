//go:build darwin

package install

import (
	"fmt"
	"os"
	"path/filepath"
)

// 开机自启：LaunchAgent。

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", "cn.mdhyy.xlan.plist"), nil
}

func AutostartInstall(exe string) (string, error) {
	p, err := plistPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>cn.mdhyy.xlan</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict>
</plist>
`, exe)
	if err := os.WriteFile(p, []byte(plist), 0o644); err != nil {
		return "", err
	}
	if _, err := run("launchctl", "load", "-w", p); err != nil {
		return "", fmt.Errorf("已写入 %s，但 launchctl 加载失败：%v", p, err)
	}
	return fmt.Sprintf("已启用 LaunchAgent：%s", p), nil
}

func AutostartRemove(_ string) (string, error) {
	p, err := plistPath()
	if err != nil {
		return "", err
	}
	_, _ = run("launchctl", "unload", "-w", p)
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return "本来就没设置开机自启", nil
		}
		return "", err
	}
	return "已取消开机自启", nil
}

func AutostartStatus() (bool, string, error) {
	p, err := plistPath()
	if err != nil {
		return false, "", err
	}
	if _, err := os.Stat(p); err != nil {
		return false, "未设置", nil
	}
	return true, "LaunchAgent " + p, nil
}
