//go:build linux && !android

package core

import (
	"os"
	"strings"
)

// machineID 返回系统的稳定标识，同一台机器上的所有 xlan 副本共用。
func machineID() string {
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(path); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v
			}
		}
	}
	return ""
}
