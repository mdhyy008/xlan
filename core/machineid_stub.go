//go:build !windows && !darwin && !linux || android || ios

package core

// machineID 在没有稳定机器标识的平台上返回空，沿用数据目录里的随机 id。
func machineID() string {
	return ""
}
