package core

import (
	"fmt"
	"time"
)

var (
	// Version 是语义版本号。
	Version = "0.1.0"
	// Commit 和 BuiltAt 在构建时用 -ldflags 注入，源码里留空。
	Commit  = ""
	BuiltAt = ""
)

// VersionString 返回带构建信息的版本串，比如
// "0.1.0 (a1b2c3d, 2026-11-04 00:12)"。没注入时只返回版本号。
func VersionString() string {
	built := BuiltAt
	// 老构建注入的是 2026-11-04T00:12，统一显示成 2026-11-04 00:12。
	if t, err := time.Parse("2006-01-02T15:04", built); err == nil {
		built = t.Format("2006-01-02 15:04")
	}
	switch {
	case Commit != "" && built != "":
		return fmt.Sprintf("%s (%s, %s)", Version, Commit, built)
	case Commit != "":
		return fmt.Sprintf("%s (%s)", Version, Commit)
	default:
		return Version
	}
}

// FormatBytes 把字节数变成 "12.3 MB" 这样的可读形式。
func FormatBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
