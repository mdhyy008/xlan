package core

import (
	"crypto/rand"
	"crypto/sha256"
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
	// 机器标识优先：同一台机器上多份数据目录（开发版、打包版、CLI）必须收敛成同一个身份。
	if seed := machineID(); seed != "" {
		derived := deviceIDFromSeed(seed)
		if id != derived {
			id = derived
			if err := os.WriteFile(idPath, []byte(id+"\n"), 0o644); err != nil {
				return Device{}, err
			}
		}
	} else if id == "" {
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

// deviceIDFromSeed 把机器标识派生成 16 位十六进制设备 id，不直接外泄机器标识本身。
func deviceIDFromSeed(seed string) string {
	sum := sha256.Sum256([]byte("xlan:" + seed))
	return hex.EncodeToString(sum[:8])
}
