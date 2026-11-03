package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func postLocal(s *Server, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestRestartWhenIdle(t *testing.T) {
	s, _, _ := newTestServer(t)
	fired := make(chan struct{}, 1)
	s.RequestRestart = func() { fired <- struct{}{} }

	rec := postLocal(s, "/v1/restart")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out struct {
		Deferred bool `json:"deferred"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Deferred {
		t.Fatal("空闲时不该推迟重启")
	}
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("空闲时没有触发重启")
	}
}

func TestRestartDeferredWhileTransferring(t *testing.T) {
	s, _, _ := newTestServer(t)
	fired := make(chan struct{}, 1)
	s.RequestRestart = func() { fired <- struct{}{} }

	s.touch("item-1")

	rec := postLocal(s, "/v1/restart")
	var out struct {
		Deferred bool  `json:"deferred"`
		Inflight int64 `json:"inflight"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.Deferred || out.Inflight != 1 {
		t.Fatalf("有传输时应该推迟重启: %+v", out)
	}
	select {
	case <-fired:
		t.Fatal("传输没结束时不该重启")
	case <-time.After(200 * time.Millisecond):
	}

	// 传输结束，挂起的重启要兑现
	s.clear("item-1")
	s.maybeRestart()
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("最后一个传输结束后没有重启")
	}
}

func TestTransferCountOnlyCountsActive(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.touch("a")
	s.touch("b")
	if n := s.transferCount(); n != 2 {
		t.Fatalf("expected 2, got %d", n)
	}
	s.clear("a")
	if n := s.transferCount(); n != 1 {
		t.Fatalf("expected 1, got %d", n)
	}
}
