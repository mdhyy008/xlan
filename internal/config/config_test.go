package config

import (
	"strings"
	"testing"
)

func TestLoadDefaultsWhenMissing(t *testing.T) {
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.APIPorts) != 3 || c.APIPorts[0] != 43003 {
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
	if !strings.HasSuffix(c.RecvDirPath(), "Downloads/xlan") {
		t.Fatalf("unexpected default: %s", c.RecvDirPath())
	}
	c.RecvDir = "~/Downloads/xlan-test"
	if strings.Contains(c.RecvDirPath(), "~") {
		t.Fatalf("tilde not expanded: %s", c.RecvDirPath())
	}
}
