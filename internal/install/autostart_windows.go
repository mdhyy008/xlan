//go:build windows

package install

import (
	"fmt"
	"os/exec"
)

// 开机自启：HKCU 的 Run 键，登录后自动启动，不需要管理员权限。

const runKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

func AutostartInstall(exe string) (string, error) {
	cmd := exec.Command("reg", "add", runKey, "/v", "xlan", "/t", "REG_SZ",
		"/d", fmt.Sprintf(`"%s" serve`, exe), "/f")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("写入注册表失败: %v（%s）", err, string(out))
	}
	return "已添加到开机启动（HKCU\\...\\Run）", nil
}

func AutostartRemove(_ string) (string, error) {
	cmd := exec.Command("reg", "delete", runKey, "/v", "xlan", "/f")
	if _, err := cmd.CombinedOutput(); err != nil {
		return "本来就没设置开机自启", nil
	}
	return "已取消开机自启", nil
}

func AutostartStatus() (bool, string, error) {
	cmd := exec.Command("reg", "query", runKey, "/v", "xlan")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, "未设置", nil
	}
	return true, "注册表 Run 键：" + string(out), nil
}
