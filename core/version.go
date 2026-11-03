package core

import "fmt"

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
	switch {
	case Commit != "" && BuiltAt != "":
		return fmt.Sprintf("%s (%s, %s)", Version, Commit, BuiltAt)
	case Commit != "":
		return fmt.Sprintf("%s (%s)", Version, Commit)
	default:
		return Version
	}
}
