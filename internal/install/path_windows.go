//go:build windows

package install

import (
	"fmt"
	"strings"
)

// 用户级 PATH，写注册表 HKCU\Environment，不需要管理员权限。

const psGetUserPath = `[Environment]::GetEnvironmentVariable('Path','User')`

func PathInstall(dir string) (string, error) {
	cur, err := run("powershell", "-NoProfile", "-Command", psGetUserPath)
	if err != nil {
		return "", err
	}
	for _, p := range strings.Split(cur, ";") {
		if strings.EqualFold(strings.TrimSpace(p), dir) {
			return "用户 PATH 里已经有了", nil
		}
	}
	script := fmt.Sprintf(
		`$p=[Environment]::GetEnvironmentVariable('Path','User'); if(-not $p){$p=''}; `+
			`[Environment]::SetEnvironmentVariable('Path', ($p.TrimEnd(';') + ';' + '%s'), 'User')`, dir)
	if _, err := run("powershell", "-NoProfile", "-Command", script); err != nil {
		return "", err
	}
	return "已添加到用户 PATH；重开命令行窗口生效。", nil
}

func PathRemove(dir string) (string, error) {
	script := fmt.Sprintf(
		`$p=[Environment]::GetEnvironmentVariable('Path','User'); if(-not $p){exit 0}; `+
			`$parts=$p -split ';' | Where-Object { $_ -ne '%s' }; `+
			`[Environment]::SetEnvironmentVariable('Path', ($parts -join ';'), 'User')`, dir)
	if _, err := run("powershell", "-NoProfile", "-Command", script); err != nil {
		return "", err
	}
	return "已从用户 PATH 移除；重开命令行窗口生效。", nil
}

func PathStatus(dir string) (bool, string, error) {
	cur, err := run("powershell", "-NoProfile", "-Command", psGetUserPath)
	if err != nil {
		return false, "", err
	}
	if strings.Contains(strings.ToLower(cur), strings.ToLower(dir)) {
		return true, "用户 PATH: " + cur, nil
	}
	return false, "未配置", nil
}
