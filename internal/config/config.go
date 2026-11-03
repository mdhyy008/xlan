package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"xlan/core"
)

// Config 是落盘的配置，字段缺省时走默认值。
type Config struct {
	Name     string `json:"name,omitempty"`
	RecvDir  string `json:"recv_dir,omitempty"`
	APIPorts []int  `json:"api_ports,omitempty"`
	Threads  int    `json:"threads,omitempty"`
}

func Default() *Config {
	ports := make([]int, 0, core.APISlots)
	for i := 0; i < core.APISlots; i++ {
		ports = append(ports, core.APIBase+i)
	}
	return &Config{APIPorts: ports, Threads: DefaultThreads}
}

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

// Dir 和 DataDir 都指向程序自己所在的目录，配置和数据跟着程序走。
// 只有收件目录是可配置的，其余一律写在程序目录里。
func Dir() (string, error) { return ExeDir() }

func DataDir() (string, error) { return ExeDir() }

// ExeDir 返回可执行文件所在的绝对目录。
func ExeDir() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Dir(p), nil
}

// legacyConfigDir / legacyDataDir 是 0.1 早期版本用过的系统目录，
// 只用于一次性搬迁旧数据。
func legacyConfigDir() string {
	switch runtime.GOOS {
	case "windows":
		if v := os.Getenv("APPDATA"); v != "" {
			return filepath.Join(v, "xlan")
		}
	case "darwin":
		return filepath.Join(home(), "Library", "Application Support", "xlan")
	default:
		if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
			return filepath.Join(v, "xlan")
		}
		return filepath.Join(home(), ".config", "xlan")
	}
	return filepath.Join(home(), ".config", "xlan")
}

func legacyDataDir() string {
	switch runtime.GOOS {
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "xlan")
		}
	case "darwin":
		return filepath.Join(home(), "Library", "Application Support", "xlan")
	default:
		if v := os.Getenv("XDG_DATA_HOME"); v != "" {
			return filepath.Join(v, "xlan")
		}
		return filepath.Join(home(), ".local", "share", "xlan")
	}
	return filepath.Join(home(), ".local", "share", "xlan")
}

// EnsureMigrated 把旧版本放在系统目录里的配置和数据搬进程序目录。
// 只搬一次，新目录已经有数据库就跳过。
func EnsureMigrated() error {
	dst, err := ExeDir()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dst, "xlan.db")); err == nil {
		return nil
	}
	for _, src := range []string{legacyConfigDir(), legacyDataDir()} {
		if src == dst || src == "" {
			continue
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			moveFile(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
		}
		_ = os.Remove(src)
	}
	return nil
}

func moveFile(src, dst string) {
	if _, err := os.Stat(dst); err == nil {
		return // 目标已存在，不覆盖
	}
	if err := os.Rename(src, dst); err == nil {
		return
	}
	// 跨分区时退化成复制
	if b, err := os.ReadFile(src); err == nil {
		if os.WriteFile(dst, b, 0o644) == nil {
			_ = os.Remove(src)
		}
	}
}

// Load 读取配置，文件不存在时返回默认配置。
func Load(dir string) (*Config, error) {
	if dir == "" {
		d, err := Dir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	c := Default()
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if len(c.APIPorts) == 0 {
		c.APIPorts = Default().APIPorts
	}
	c.Threads = ClampThreads(c.Threads)
	return c, nil
}

func (c *Config) Save(dir string) error {
	if dir == "" {
		d, err := Dir()
		if err != nil {
			return err
		}
		dir = d
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), append(b, '\n'), 0o644)
}

// DownloadsDir 返回本机默认的下载目录。
// 安卓（含 Termux/proot）用 /sdcard/Download，其余平台用 ~/Downloads。
func DownloadsDir() string {
	if st, err := os.Stat("/sdcard/Download"); err == nil && st.IsDir() {
		return "/sdcard/Download"
	}
	return filepath.Join(home(), "Downloads")
}

// RecvDirPath 返回收件目录的绝对路径，默认是下载目录下的 xlan。
func (c *Config) RecvDirPath() string {
	p := strings.TrimSpace(c.RecvDir)
	if p == "" {
		p = filepath.Join(DownloadsDir(), "xlan")
	}
	if p == "~" {
		return home()
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(home(), p[2:])
	}
	return p
}

// DefaultThreads 是默认的并行分块数。
const DefaultThreads = 4

// MaxThreads 是允许的最大并发数。
const MaxThreads = 8

// ClampThreads 把并发数限制在 1..MaxThreads；没设过（0 或负数）用默认值。
func ClampThreads(n int) int {
	if n <= 0 {
		return DefaultThreads
	}
	if n > MaxThreads {
		return MaxThreads
	}
	return n
}
