package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xlan/core"
)

// 准备一个带子目录、空目录、空文件、中文名和二进制内容的目录。
func makeTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "项目资料")
	if err := os.MkdirAll(filepath.Join(root, "空目录"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "文档", "子目录"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel string, b []byte) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("readme.txt", []byte("hello folder\n"))
	write("文档/说明.md", []byte("# 中文内容\n"))
	write("文档/子目录/空文件.txt", nil)
	write("binary.bin", bytes.Repeat([]byte{0x00, 0xff, 0x7f}, 5000))
	return root
}

func manifestOf(t *testing.T, root string) core.Manifest {
	t.Helper()
	var m core.Manifest
	m.Root = filepath.Base(root)
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			m.Entries = append(m.Entries, core.FolderEntry{Rel: rel, Dir: true})
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		m.Entries = append(m.Entries, core.FolderEntry{
			Rel: rel, Size: info.Size(), SHA256: hex.EncodeToString(sum[:]),
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func uploadFile(t *testing.T, url, id, rel string, content []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut,
		url+"/v1/offer/"+id+"/file/"+escapeForTest(rel), bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Range", "bytes 0-"+itoaTest(len(content)-1)+"/"+itoaTest(len(content)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func escapeForTest(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(p, " ", "%20")
	}
	return strings.Join(parts, "/")
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestFolderTransferEndToEnd(t *testing.T) {
	s, ts, dir := newTestServer(t)
	src := makeTree(t)
	m := manifestOf(t, src)

	// 从一台可信设备发来
	now := time.Now().Unix()
	if err := s.Store.UpsertPeer(core.Peer{ID: "peer-a", Name: "朋友", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetTrusted("peer-a", true); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{
		"from_id": "peer-a", "from_name": "朋友", "to_id": "self",
		"kind": "folder", "manifest": m,
	})
	resp, err := http.Post(ts.URL+"/v1/offer", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var o struct {
		ID       string `json:"id"`
		Accepted bool   `json:"accepted"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&o)
	resp.Body.Close()
	if !o.Accepted {
		t.Fatal("trusted peer's folder should be accepted")
	}

	for _, e := range m.Entries {
		if e.Dir || e.Size == 0 {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(e.Rel)))
		if err != nil {
			t.Fatal(err)
		}
		r := uploadFile(t, ts.URL, o.ID, e.Rel, b)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("upload %s: %d", e.Rel, r.StatusCode)
		}
	}

	r, err := http.Post(ts.URL+"/v1/offer/"+o.ID+"/finish", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("finish failed: %d", r.StatusCode)
	}

	dst := filepath.Join(dir, "recv", m.Root)
	for _, e := range m.Entries {
		p := filepath.Join(dst, filepath.FromSlash(e.Rel))
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("missing %s: %v", e.Rel, err)
		}
		if e.Dir && !st.IsDir() {
			t.Fatalf("%s should be a dir", e.Rel)
		}
		if !e.Dir && st.Size() != e.Size {
			t.Fatalf("%s size = %d, want %d", e.Rel, st.Size(), e.Size)
		}
	}

	it, ok, _ := s.Store.GetItem(o.ID)
	if !ok || it.Status != core.StatusDone {
		t.Fatalf("item not done: %+v", it)
	}
}

func TestFolderRejectsTraversal(t *testing.T) {
	s, ts, dir := newTestServer(t)
	now := time.Now().Unix()
	_ = s.Store.UpsertPeer(core.Peer{ID: "peer-a", Name: "坏蛋", LastSeen: now})
	_ = s.Store.SetTrusted("peer-a", true)

	m := core.Manifest{Root: "evil", Entries: []core.FolderEntry{
		{Rel: "../../escape.txt", Size: 3},
	}}
	body, _ := json.Marshal(map[string]any{
		"from_id": "peer-a", "to_id": "self", "kind": "folder", "manifest": m,
	})
	resp, _ := http.Post(ts.URL+"/v1/offer", "application/json", bytes.NewReader(body))
	var o struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&o)
	resp.Body.Close()

	r := uploadFile(t, ts.URL, o.ID, "../../escape.txt", []byte("bad"))
	r.Body.Close()
	if r.StatusCode == http.StatusOK {
		t.Fatal("traversal path should be rejected")
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "escape.txt")); err == nil {
		t.Fatal("file escaped the receive dir")
	}
}

func TestFinishDetectsMissingFiles(t *testing.T) {
	s, ts, _ := newTestServer(t)
	now := time.Now().Unix()
	_ = s.Store.UpsertPeer(core.Peer{ID: "peer-a", Name: "朋友", LastSeen: now})
	_ = s.Store.SetTrusted("peer-a", true)

	m := core.Manifest{Root: "half", Entries: []core.FolderEntry{
		{Rel: "a.txt", Size: 3, SHA256: "aa"},
		{Rel: "b.txt", Size: 3, SHA256: "bb"},
	}}
	body, _ := json.Marshal(map[string]any{
		"from_id": "peer-a", "to_id": "self", "kind": "folder", "manifest": m,
	})
	resp, _ := http.Post(ts.URL+"/v1/offer", "application/json", bytes.NewReader(body))
	var o struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&o)
	resp.Body.Close()

	r, _ := http.Post(ts.URL+"/v1/offer/"+o.ID+"/finish", "application/json", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for missing files, got %d", r.StatusCode)
	}
}

func TestSafeJoin(t *testing.T) {
	root := t.TempDir()
	ok := []string{"a.txt", "文档/说明.md", "a/../b.txt"}
	for _, rel := range ok {
		if _, err := safeJoin(root, rel); err != nil {
			t.Errorf("safeJoin(%q) should pass: %v", rel, err)
		}
	}
	bad := []string{"../x", "../../etc/passwd", "/etc/passwd", "", "."}
	for _, rel := range bad {
		if _, err := safeJoin(root, rel); err == nil {
			t.Errorf("safeJoin(%q) should fail", rel)
		}
	}
}

var _ = httptest.NewServer
