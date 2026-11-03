package discover

import (
	"net"
	"testing"
	"time"

	"path/filepath"
	"xlan/core"
	"xlan/internal/events"
	"xlan/internal/store"
)

func newDisc(t *testing.T, id string, apiPort int) (*Discoverer, *[]core.Packet) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	d := New(core.Device{ID: id, Name: id, Platform: "linux"}, apiPort, st, events.New())
	var sent []core.Packet
	d.send = func(_ *net.UDPAddr, b []byte) error {
		p, err := core.ParsePacket(b)
		if err == nil {
			sent = append(sent, p)
		}
		return nil
	}
	return d, &sent
}

func TestProbeGetsAnnounceReply(t *testing.T) {
	d, sent := newDisc(t, "self", 43003)
	from := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 9), Port: core.DiscoverPort}

	d.HandlePacket(core.Packet{V: 1, T: "probe", ID: "other", Name: "朋友"}, from)

	if len(*sent) != 1 {
		t.Fatalf("expected one reply, got %d", len(*sent))
	}
	got := (*sent)[0]
	if got.T != "announce" || got.ID != "self" || got.Port != 43003 {
		t.Fatalf("bad announce: %+v", got)
	}
}

func TestAnnounceUpsertsPeer(t *testing.T) {
	d, _ := newDisc(t, "self", 43003)
	from := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 9), Port: core.DiscoverPort}

	d.HandlePacket(core.Packet{V: 1, T: "announce", ID: "other", Name: "朋友",
		Port: 43003, Platform: "darwin", Ts: time.Now().Unix()}, from)

	peers, err := d.Store.ListPeers()
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(peers))
	}
	p := peers[0]
	if p.ID != "other" || p.Addr != "10.0.0.9" || p.Port != 43003 || p.Platform != "darwin" {
		t.Fatalf("bad peer: %+v", p)
	}
}

func TestSelfAnnounceIgnored(t *testing.T) {
	d, sent := newDisc(t, "self", 43003)
	from := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 9), Port: core.DiscoverPort}
	d.HandlePacket(core.Packet{V: 1, T: "probe", ID: "self"}, from)
	if len(*sent) != 0 {
		t.Fatal("should not answer its own probe")
	}
}

func TestBroadcastAddrsNotEmpty(t *testing.T) {
	addrs, err := BroadcastAddrs()
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) == 0 {
		t.Fatal("expected at least the limited broadcast address")
	}
	found := false
	for _, ip := range addrs {
		if ip.Equal(net.IPv4bcast) {
			found = true
		}
	}
	if !found {
		t.Fatal("255.255.255.255 missing")
	}
}
