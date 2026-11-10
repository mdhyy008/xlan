package store

import (
	"path/filepath"
	"testing"

	"xlan/core"
)

func TestItemCRUD(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	it := core.Item{ID: "a1", Kind: core.KindText, FromID: "p1", ToID: "*",
		Channel: core.ChanPublic, Body: "hi", Status: core.StatusDone, CreatedAt: 100}
	if err := s.AddItem(it); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetItem("a1")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Body != "hi" || got.Kind != core.KindText {
		t.Fatalf("mismatch: %+v", got)
	}

	items, err := s.ListItems(10, 0, 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("list: %d %v", len(items), err)
	}

	if err := s.UpdateItemStatus("a1", core.StatusFailed, "/tmp/x"); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.GetItem("a1")
	if got.Status != core.StatusFailed || got.Path != "/tmp/x" {
		t.Fatalf("update failed: %+v", got)
	}
}

func TestListItemsSince(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "x.db"))
	defer s.Close()
	for i, ts := range []int64{10, 20, 30} {
		_ = s.AddItem(core.Item{ID: string(rune('a' + i)), Kind: core.KindText,
			FromID: "p", ToID: "*", Channel: core.ChanPublic, Status: core.StatusDone, CreatedAt: ts})
	}
	items, _ := s.ListItems(0, 20, 0)
	if len(items) != 2 {
		t.Fatalf("expected 2 items since 20, got %d", len(items))
	}
}

func TestUpsertPeerKeepsTrusted(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "x.db"))
	defer s.Close()

	if err := s.UpsertPeer(core.Peer{ID: "p1", Name: "旧名", LastSeen: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTrusted("p1", true); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPeer(core.Peer{ID: "p1", Name: "新名", Addr: "10.0.0.2", Port: 43003, LastSeen: 2}); err != nil {
		t.Fatal(err)
	}
	p, ok, _ := s.GetPeer("p1")
	if !ok {
		t.Fatal("peer missing")
	}
	if !p.Trusted {
		t.Fatal("trusted flag was lost on upsert")
	}
	if p.Name != "新名" || p.Port != 43003 {
		t.Fatalf("peer not refreshed: %+v", p)
	}
}
