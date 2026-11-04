package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xlan/core"
)

func TestLoadDefaultsWhenMissing(t *testing.T) {
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.APIPorts) != 3 || c.APIPorts[0] != core.APIBase {
		t.Fatalf("bad default ports: %v", c.APIPorts)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := Default()
	c.Name = "测试机"
	c.RecvDir = "/tmp/收件"
	if err := c.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "测试机" || got.RecvDir != "/tmp/收件" {
		t.Fatalf("mismatch: %+v", got)
	}
}

func TestRecvDirPath(t *testing.T) {
	c := Default()
	got := c.RecvDirPath()
	if got != filepath.Join(DownloadsDir(), "xlan") {
		t.Fatalf("默认收件目录应跟着下载目录走: %s", got)
	}
	if _, err := os.Stat("/sdcard/Download"); err == nil {
		if got != "/sdcard/Download/xlan" {
			t.Fatalf("安卓上应该用 /sdcard/Download/xlan，实际 %s", got)
		}
	}
	c.RecvDir = "~/Downloads/xlan-test"
	if strings.Contains(c.RecvDirPath(), "~") {
		t.Fatalf("tilde not expanded: %s", c.RecvDirPath())
	}
}

func TestClampThreads(t *testing.T) {
	cases := map[int]int{-3: DefaultThreads, 0: DefaultThreads, 1: 1, 4: 4, MaxThreads: MaxThreads, 99: MaxThreads}
	for in, want := range cases {
		if got := ClampThreads(in); got != want {
			t.Errorf("ClampThreads(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestDefaultThreads(t *testing.T) {
	if got := Default().Threads; got != DefaultThreads {
		t.Fatalf("默认并发数应为 %d，实际 %d", DefaultThreads, got)
	}
}
