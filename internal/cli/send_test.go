package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"xlan/core"
	"xlan/internal/config"
	"xlan/internal/events"
	"xlan/internal/httpapi"
	"xlan/internal/store"
)

// TestSendOneDeliversFile 走完整链路：offer → 信任直通 → 分块上传 → 校验落盘。
func TestSendOneDeliversFile(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	recvDir := filepath.Join(dir, "recv")
	b := &httpapi.Server{
		Dev:  core.Device{ID: "self-b", Name: "B", Platform: "linux", Version: "test"},
		Port: 43003, Store: st, Bus: events.New(),
		RecvDir: recvDir, StartedAt: time.Now(),
	}
	ts := httptest.NewServer(b.Handler())
	defer ts.Close()

	// A 已在 B 的白名单里，offer 应直接通过
	now := time.Now().Unix()
	if err := st.UpsertPeer(core.Peer{ID: "self-a", Name: "A", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTrusted("self-a", true); err != nil {
		t.Fatal(err)
	}

	content := bytes.Repeat([]byte("xlan"), 3000) // 12KB，跨越多个读取块
	src := filepath.Join(dir, "payload.bin")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(src)

	addr, port := splitHostPort(t, ts.URL)
	peer := core.Peer{ID: "self-b", Name: "B", Addr: addr, Port: port}

	a := &App{
		Out: io.Discard, Err: io.Discard, Quiet: true,
		Timeout: 5 * time.Second, Cfg: config.Default(),
		Dev: core.Device{ID: "self-a", Name: "A"},
	}
	if err := a.sendOne(context.Background(), peer, src, fi.Size()); err != nil {
		t.Fatalf("sendOne: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(recvDir, "payload.bin"))
	if err != nil {
		t.Fatalf("file not delivered: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(content))
	}
}

func splitHostPort(t *testing.T, url string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(url[len("http://"):])
	if err != nil {
		t.Fatal(err)
	}
	port, err := net.LookupPort("tcp", portStr)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}
