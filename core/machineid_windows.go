//go:build windows

package core

import "golang.org/x/sys/windows/registry"

// machineID 返回系统安装时生成的唯一标识，同一台机器上的所有 xlan 副本共用。
func machineID() string {
	k, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Cryptography`,
		registry.QUERY_VALUE|registry.WOW64_64KEY,
	)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return ""
	}
	return v
}
