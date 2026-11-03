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
	"xlan/internal/events"
	"xlan/internal/store"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	s := &Server{
		Dev:       core.Device{ID: "self", Name: "本机", Platform: "linux", Version: "test"},
		Port:      43003,
		Store:     st,
		Bus:       events.New(),
		RecvDir:   filepath.Join(dir, "recv"),
		StartedAt: time.Now(),
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts, dir
}

func TestInfo(t *testing.T) {
	_, ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["id"] != "self" || out["ok"] != true {
		t.Fatalf("bad info: %v", out)
	}
}

func TestLocalOnlyGuard(t *testing.T) {
	s, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/scan", nil)
	req.RemoteAddr = "192.0.2.1:1234" // 非回环地址
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-loopback, got %d", rec.Code)
	}
}

func TestOfferRejectsOtherDevice(t *testing.T) {
	_, ts, _ := newTestServer(t)
	body := `{"from_id":"p1","to_id":"someone-else","kind":"text","body":"hi"}`
	resp, err := http.Post(ts.URL+"/v1/offer", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

func TestTextOfferAutoAccepts(t *testing.T) {
	_, ts, _ := newTestServer(t)
	body := `{"from_id":"p1","from_name":"朋友","to_id":"self","kind":"text","body":"在吗"}`
	resp, err := http.Post(ts.URL+"/v1/offer", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		OK       bool   `json:"ok"`
		Accepted bool   `json:"accepted"`
		Status   string `json:"status"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if !out.Accepted || out.Status != "done" {
		t.Fatalf("text should auto-accept: %+v", out)
	}
}

func TestFileOfferFlow(t *testing.T) {
	_, ts, dir := newTestServer(t)
	content := []byte("hello file\n")
	sum := sha256.Sum256(content)

	offer := `{"from_id":"p1","from_name":"朋友","to_id":"self","kind":"file",
		"name":"hello.txt","size":11,"sha256":"` + hex.EncodeToString(sum[:]) + `"}`
	resp, err := http.Post(ts.URL+"/v1/offer", "application/json", strings.NewReader(offer))
	if err != nil {
		t.Fatal(err)
	}
	var o struct {
		ID       string `json:"id"`
		Accepted bool   `json:"accepted"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&o)
	resp.Body.Close()
	if o.Accepted {
		t.Fatal("untrusted file offer should be pending")
	}

	// 未确认前不允许上传
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/offer/"+o.ID+"/data", bytes.NewReader(content))
	req.Header.Set("Content-Range", "bytes 0-10/11")
	r2, _ := http.DefaultClient.Do(req)
	r2.Body.Close()
	if r2.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 before accept, got %d", r2.StatusCode)
	}

	// 接受
	r3, err := http.Post(ts.URL+"/v1/offer/"+o.ID+"/decision", "application/json",
		strings.NewReader(`{"action":"accept"}`))
	if err != nil {
		t.Fatal(err)
	}
	r3.Body.Close()

	// 上传
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/v1/offer/"+o.ID+"/data", bytes.NewReader(content))
	req.Header.Set("Content-Range", "bytes 0-10/11")
	r4, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r4.Body.Close()
	if r4.StatusCode != http.StatusOK {
		t.Fatalf("upload failed: %d", r4.StatusCode)
	}

	got, err := os.ReadFile(filepath.Join(dir, "recv", "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch: %q", got)
	}
}

func TestChecksumMismatchRejected(t *testing.T) {
	_, ts, _ := newTestServer(t)
	content := []byte("hello file\n")
	bad := strings.Repeat("0", 64)

	offer := `{"from_id":"p1","to_id":"self","kind":"file","name":"x.txt","size":11,"sha256":"` + bad + `"}`
	resp, _ := http.Post(ts.URL+"/v1/offer", "application/json", strings.NewReader(offer))
	var o struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&o)
	resp.Body.Close()

	_, _ = http.Post(ts.URL+"/v1/offer/"+o.ID+"/decision", "application/json",
		strings.NewReader(`{"action":"accept"}`))

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/offer/"+o.ID+"/data", bytes.NewReader(content))
	req.Header.Set("Content-Range", "bytes 0-10/11")
	r, _ := http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on checksum mismatch, got %d", r.StatusCode)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":    "passwd",
		`C:\Windows\evil.exe`: "evil.exe",
		"a<b>c:d.txt":         "a_b_c_d.txt",
		"..":                  "unnamed",
		"CON":                 "_CON",
		"正常文件.txt":            "正常文件.txt",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUniquePath(t *testing.T) {
	dir := t.TempDir()
	first := uniquePath(dir, "a.txt")
	if first != filepath.Join(dir, "a.txt") {
		t.Fatalf("unexpected: %s", first)
	}
	if err := os.WriteFile(first, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := uniquePath(dir, "a.txt")
	if second != filepath.Join(dir, "a (1).txt") {
		t.Fatalf("expected a (1).txt, got %s", second)
	}
}

func TestParseContentRange(t *testing.T) {
	start, end, total, err := parseContentRange("bytes 0-99/100")
	if err != nil || start != 0 || end != 99 || total != 100 {
		t.Fatalf("got %d %d %d %v", start, end, total, err)
	}
	if _, _, _, err := parseContentRange("garbage"); err == nil {
		t.Fatal("expected error")
	}
}
