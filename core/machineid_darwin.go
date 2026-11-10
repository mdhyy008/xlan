//go:build darwin && !ios

package core

import (
	"os/exec"
	"strings"
)

// machineID 返回系统的稳定标识，同一台机器上的所有 xlan 副本共用。
func machineID() string {
	out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "IOPlatformUUID") {
			continue
		}
		if i := strings.Index(line, `"IOPlatformUUID"`); i >= 0 {
			rest := line[i+len(`"IOPlatformUUID"`):]
			if q := strings.Index(rest, `"`); q >= 0 {
				rest = rest[q+1:]
				if q := strings.Index(rest, `"`); q >= 0 {
					return strings.TrimSpace(rest[:q])
				}
			}
		}
	}
	return ""
}
