package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"xlan/core"
)

func TestCacheClean(t *testing.T) {
	s, ts, dir := newTestServer(t)
	now := time.Now()
	old := now.Add(-31 * 24 * time.Hour)

	partsDir := filepath.Join(dir, "recv", ".parts")
	if err := os.MkdirAll(partsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPart := filepath.Join(partsDir, "dead.part")
	freshPart := filepath.Join(partsDir, "live.part")
	if err := os.WriteFile(oldPart, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(freshPart, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldPart, old, old); err != nil {
		t.Fatal(err)
	}

	items := []core.Item{
		{ID: "old-done", Kind: core.KindFile, FromID: "p", ToID: "self", Channel: core.ChanDM, Name: "a", Status: core.StatusDone, CreatedAt: old.Unix()},
		{ID: "old-failed", Kind: core.KindFile, FromID: "p", ToID: "self", Channel: core.ChanDM, Name: "b", Status: core.StatusFailed, CreatedAt: old.Unix()},
		{ID: "old-pending", Kind: core.KindFile, FromID: "p", ToID: "self", Channel: core.ChanDM, Name: "c", Status: core.StatusPending, CreatedAt: old.Unix()},
		{ID: "new-done", Kind: core.KindFile, FromID: "p", ToID: "self", Channel: core.ChanDM, Name: "d", Status: core.StatusDone, CreatedAt: now.Unix()},
	}
	for _, it := range items {
		if err := s.Store.AddItem(it); err != nil {
			t.Fatal(err)
		}
	}

	post := func(dryRun bool) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"older_than_sec": int64(30 * 24 * time.Hour / time.Second),
			"dry_run":        dryRun,
		})
		resp, err := http.Post(ts.URL+"/v1/cache/clean", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	out := post(true)
	if out["parts"].(float64) != 1 || out["records"].(float64) != 2 {
		t.Fatalf("dry run counts: %v", out)
	}
	if _, err := os.Stat(oldPart); err != nil {
		t.Fatal("dry run 不应删除分片")
	}

	out = post(false)
	if out["parts"].(float64) != 1 || out["records"].(float64) != 2 ||
		out["freed_bytes"].(float64) != 3 {
		t.Fatalf("clean counts: %v", out)
	}
	if _, err := os.Stat(oldPart); !os.IsNotExist(err) {
		t.Fatal("旧的临时分片应该被删除")
	}
	if _, err := os.Stat(freshPart); err != nil {
		t.Fatal("正在传的分片不应被删除")
	}

	for id, want := range map[string]bool{
		"old-done": false, "old-failed": false, "old-pending": true, "new-done": true,
	} {
		_, ok, err := s.Store.GetItem(id)
		if err != nil {
			t.Fatal(err)
		}
		if ok != want {
			t.Fatalf("item %s exists=%v, want %v", id, ok, want)
		}
	}
}
