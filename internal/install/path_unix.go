//go:build !windows

package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	pathBlockStart = "# >>> xlan path >>>"
	pathBlockEnd   = "# <<< xlan path <<<"
)

// rcFile 挑一个最可能被 shell 读到的启动文件。
func rcFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	shell := os.Getenv("SHELL")
	if strings.Contains(shell, "zsh") {
		if p := filepath.Join(home, ".zshrc"); fileExists(p) {
			return p, nil
		}
	}
	for _, c := range []string{".bashrc", ".zshrc", ".profile"} {
		if p := filepath.Join(home, c); fileExists(p) {
			return p, nil
		}
	}
	return filepath.Join(home, ".profile"), nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func PathInstall(dir string) (string, error) {
	p, err := rcFile()
	if err != nil {
		return "", err
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), pathBlockStart) {
		return fmt.Sprintf("%s 里已经配置过了", p), nil
	}
	block := fmt.Sprintf("\n%s\ncase \":$PATH:\" in *\":%s:\"*) ;; *) export PATH=\"$PATH:%s\" ;; esac\n%s\n",
		pathBlockStart, dir, dir, pathBlockEnd)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(block); err != nil {
		return "", err
	}
	return fmt.Sprintf("已写入 %s；重开终端，或执行 source %s 生效。", p, p), nil
}

func PathRemove(dir string) (string, error) {
	_ = dir
	p, err := rcFile()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "本来就没加过", nil
	}
	out := removeBlock(string(b))
	if out == string(b) {
		return "本来就没加过", nil
	}
	if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
		return "", err
	}
	return "已从 " + p + " 移除", nil
}

func PathStatus(dir string) (bool, string, error) {
	_ = dir
	p, err := rcFile()
	if err != nil {
		return false, "", err
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), pathBlockStart) {
		return true, "已写入 " + p, nil
	}
	return false, "未配置", nil
}

// removeBlock 删掉标记之间的内容。
func removeBlock(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	skip := false
	for _, l := range lines {
		switch {
		case strings.Contains(l, pathBlockStart):
			skip = true
		case strings.Contains(l, pathBlockEnd):
			skip = false
		case !skip:
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
