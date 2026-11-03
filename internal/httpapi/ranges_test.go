package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"xlan/core"
	"xlan/internal/events"
)

func TestSpanSetMerge(t *testing.T) {
	ss := &spanSet{}
	ss.add(0, 10)
	ss.add(20, 30)
	if ss.covered != 20 {
		t.Fatalf("covered = %d, want 20", ss.covered)
	}
	ss.add(5, 25) // 把两段连起来
	if ss.covered != 30 {
		t.Fatalf("合并后 covered = %d, want 30", ss.covered)
	}
	if ss.prefix() != 30 {
		t.Fatalf("prefix = %d, want 30", ss.prefix())
	}
}

func TestSpanSetPrefixStopsAtGap(t *testing.T) {
	ss := &spanSet{}
	ss.add(10, 20)
	if ss.prefix() != 0 {
		t.Fatalf("有洞时 prefix 应该为 0，得到 %d", ss.prefix())
	}
	ss.add(0, 10)
	if ss.prefix() != 20 {
		t.Fatalf("接上后 prefix = %d, want 20", ss.prefix())
	}
}

func TestRangesSurviveRestart(t *testing.T) {
	s, _, _ := newTestServer(t)
	it := core.Item{ID: "resume-1", Kind: core.KindFile, FromID: "p", ToID: "self",
		Channel: core.ChanDM, Name: "a.bin", Size: 1000, Status: core.StatusActive,
		CreatedAt: time.Now().Unix()}
	if err := s.Store.AddItem(it); err != nil {
		t.Fatal(err)
	}

	s.markRange("resume-1", 0, 400, 1000)
	if got := s.rangePrefix("resume-1"); got != 400 {
		t.Fatalf("prefix = %d, want 400", got)
	}

	// 模拟服务重启：换一个 Server，同一个数据库
	s2 := &Server{Dev: s.Dev, Store: s.Store, Bus: events.New(), RecvDir: s.RecvDir}
	if got := s2.rangePrefix("resume-1"); got != 400 {
		t.Fatalf("重启后 prefix = %d, want 400", got)
	}
}

func TestCancelClearsProgress(t *testing.T) {
	s, _, _ := newTestServer(t)
	it := core.Item{ID: "c1", Kind: core.KindFile, FromID: "p", ToID: "self",
		Channel: core.ChanDM, Name: "x", Size: 100, Status: core.StatusActive,
		CreatedAt: time.Now().Unix()}
	_ = s.Store.AddItem(it)
	s.markRange("c1", 0, 50, 100)
	s.touch("c1")

	req := httptest.NewRequest(http.MethodPost, "/v1/offer/c1/decision",
		strings.NewReader(`{"action":"cancel"}`))
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel 返回 %d: %s", rec.Code, rec.Body.String())
	}

	got, ok, _ := s.Store.GetItem("c1")
	if !ok || got.Status != core.StatusFailed {
		t.Fatalf("取消后状态应为 failed，得到 %+v", got.Status)
	}
	if n := s.transferCount(); n != 0 {
		t.Fatalf("取消后不该还有在传的任务，得到 %d", n)
	}
	if got := s.rangePrefix("c1"); got != 0 {
		t.Fatalf("取消后区间应清空，得到 %d", got)
	}
}
