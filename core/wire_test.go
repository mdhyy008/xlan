package core

import (
	"strings"
	"testing"
)

func TestPacketRoundTrip(t *testing.T) {
	p := Packet{V: 1, T: "msg", ID: "abc", SID: "s1", Body: "你好", Ts: 123}
	b, err := p.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := ParsePacket(b)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Body != "你好" || got.T != "msg" || got.ID != "abc" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestPacketTooLarge(t *testing.T) {
	p := Packet{V: 1, T: "msg", Body: strings.Repeat("x", MaxPacket)}
	if _, err := p.Marshal(); err == nil {
		t.Fatal("expected ErrPacketTooLarge")
	}
}

func TestParseRejectsUnknownVersion(t *testing.T) {
	if _, err := ParsePacket([]byte(`{"v":9,"t":"probe"}`)); err == nil {
		t.Fatal("expected version error")
	}
}

func TestPeerBaseURL(t *testing.T) {
	p := Peer{Addr: "192.168.1.5", Port: 43003}
	if got := p.BaseURL(); got != "http://192.168.1.5:43003" {
		t.Fatalf("got %q", got)
	}
	if got := (Peer{Name: "x"}).BaseURL(); got != "" {
		t.Fatalf("expected empty base url, got %q", got)
	}
}
