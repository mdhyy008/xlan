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
}

func Default() *Config {
	ports := make([]int, 0, core.APISlots)
	for i := 0; i < core.APISlots; i++ {
		ports = append(ports, core.APIBase+i)
	}
	return &Config{APIPorts: ports}
}

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

// Dir 返回配置目录。
func Dir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		if v := os.Getenv("APPDATA"); v != "" {
			return filepath.Join(v, "xlan"), nil
		}
	case "darwin":
		return filepath.Join(home(), "Library", "Application Support", "xlan"), nil
	default:
		if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
			return filepath.Join(v, "xlan"), nil
		}
		return filepath.Join(home(), ".config", "xlan"), nil
	}
	return filepath.Join(home(), ".config", "xlan"), nil
}

// DataDir 返回数据目录（数据库、设备 id）。
func DataDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "xlan"), nil
		}
	case "darwin":
		return filepath.Join(home(), "Library", "Application Support", "xlan"), nil
	default:
		if v := os.Getenv("XDG_DATA_HOME"); v != "" {
			return filepath.Join(v, "xlan"), nil
		}
		return filepath.Join(home(), ".local", "share", "xlan"), nil
	}
	return filepath.Join(home(), ".local", "share", "xlan"), nil
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
