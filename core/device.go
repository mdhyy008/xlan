package core

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Device 是本机身份。
type Device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Version  string `json:"version"`
}

// LoadDevice 读取或生成稳定的设备 id，并确定显示名。
// override 为空时用系统主机名。
func LoadDevice(dataDir, override string) (Device, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return Device{}, err
	}
	idPath := filepath.Join(dataDir, "id")
	var id string
	if b, err := os.ReadFile(idPath); err == nil {
		id = strings.TrimSpace(string(b))
	}
	if id == "" {
		buf := make([]byte, 8)
		if _, err := rand.Read(buf); err != nil {
			return Device{}, err
		}
		id = hex.EncodeToString(buf)
		if err := os.WriteFile(idPath, []byte(id+"\n"), 0o644); err != nil {
			return Device{}, err
		}
	}

	name := strings.TrimSpace(override)
	if name == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			name = h
		} else {
			name = "xlan-" + id[:4]
		}
	}

	return Device{
		ID:       id,
		Name:     name,
		Platform: runtime.GOOS,
		Version:  Version,
	}, nil
}
