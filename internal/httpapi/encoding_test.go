package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"xlan/core"
	"xlan/internal/client"
)

func TestFilenameEncodingRoundTrip(t *testing.T) {
	s, ts, dir := newTestServer(t)
	now := time.Now().Unix()
	if err := s.Store.UpsertPeer(core.Peer{ID: "peer-a", Name: "朋友", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetTrusted("peer-a", true); err != nil {
		t.Fatal(err)
	}

	names := []string{
		"带 空格 的文件.txt",
		"井号#和百分号%.txt",
		"加号+和和号&号.txt",
		`引号'和"双引号.txt`,
		"emoji😀🎉文件.txt",
		"NFC-é.txt",
		"NFD-e\u0301.txt",
		"括号(和)方括号[].txt",
		"分号;逗号,等号=.txt",
		"@at$dollar.txt",
		"波浪~星号*.txt",
	}
	contents := map[string][]byte{}
	var entries []core.FolderEntry
	for i, n := range names {
		b := bytes.Repeat([]byte{byte(i), 0x00, 0xff, 'a'}, 100)
		contents[n] = b
		sum := sha256.Sum256(b)
		entries = append(entries, core.FolderEntry{
			Rel: n, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:]),
		})
	}

	m := core.Manifest{Root: "编码测试", Entries: entries}
	resp, err := client.Offer(context.Background(), ts.URL, client.OfferReq{
		FromID: "peer-a", FromName: "朋友", ToID: "self",
		Kind: "folder", Name: m.Root, Manifest: &m,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Accepted {
		t.Fatal("offer not accepted")
	}

	for _, e := range entries {
		b := contents[e.Rel]
		if err := client.PutFileRange(context.Background(), ts.URL, resp.ID, e.Rel,
			bytes.NewReader(b), 0, int64(len(b)), int64(len(b))); err != nil {
			t.Fatalf("upload %q: %v", e.Rel, err)
		}
	}
	if err := client.FinishFolder(context.Background(), ts.URL, resp.ID); err != nil {
		t.Fatalf("finish: %v", err)
	}

	gotDir := filepath.Join(dir, "recv", m.Root)
	des, err := os.ReadDir(gotDir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, de := range des {
		got = append(got, de.Name())
	}
	sort.Strings(got)

	want := append([]string(nil), names...)
	sort.Strings(want)

	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("文件名不一致\n got: %q\nwant: %q", got, want)
	}
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(gotDir, n))
		if err != nil {
			t.Fatalf("read %q: %v", n, err)
		}
		if !bytes.Equal(b, contents[n]) {
			t.Fatalf("内容不一致: %q", n)
		}
	}
}

func TestLongFilenameStaysValidUTF8(t *testing.T) {
	// 120 个汉字 = 360 字节，超过 200 字节上限，必须按 rune 截断
	long := strings.Repeat("很长的名字", 24)
	if len(long) <= 200 {
		t.Fatalf("测试前提不成立: %d 字节", len(long))
	}
	got := sanitizeName(long)
	if !utf8.ValidString(got) {
		t.Fatalf("截断产生了非法 UTF-8: %q", got)
	}
	if len(got) > 200 {
		t.Fatalf("截断后仍然超长: %d", len(got))
	}
}

func TestInvalidUTF8NameDoesNotPanic(t *testing.T) {
	raw := string([]byte{0x61, 0xff, 0xfe, 0x62})
	got := sanitizeName(raw)
	if got == "" {
		t.Fatal("empty result")
	}
}
