package core

import (
	"strings"
	"testing"
)

func TestVersionString(t *testing.T) {
	oldCommit, oldBuilt := Commit, BuiltAt
	t.Cleanup(func() { Commit, BuiltAt = oldCommit, oldBuilt })

	Commit, BuiltAt = "", ""
	if got := VersionString(); got != Version {
		t.Fatalf("没注入时应只有版本号，得到 %q", got)
	}

	Commit = "a1b2c3d"
	if got := VersionString(); !strings.Contains(got, "a1b2c3d") {
		t.Fatalf("应包含提交号，得到 %q", got)
	}

	BuiltAt = "2026-11-04 00:12"
	got := VersionString()
	if !strings.Contains(got, "a1b2c3d") || !strings.Contains(got, "2026-11-04 00:12") {
		t.Fatalf("应同时包含提交号和时间，得到 %q", got)
	}
}
