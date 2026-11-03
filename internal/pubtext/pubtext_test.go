package pubtext

import (
	"net"
	"testing"
	"time"

	"xlan/core"
)

func TestBroadcastAndAck(t *testing.T) {
	a, err := Listen(net.IPv4(127, 0, 0, 1), 0, "id-a", "A")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	b, err := Listen(net.IPv4(127, 0, 0, 1), 0, "id-b", "B")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	got := make(chan core.Packet, 1)
	b.OnMsg = func(p core.Packet, from *net.UDPAddr) { got <- p }

	bPort := b.conn.LocalAddr().(*net.UDPAddr).Port
	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: bPort}

	pkt := core.Packet{V: 1, T: "msg", ID: "m1", SID: "id-a", SName: "A",
		Channel: "public", Body: "hello", Ts: time.Now().Unix()}

	acked, err := a.Send([]*net.UDPAddr{target}, pkt, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case p := <-got:
		if p.Body != "hello" {
			t.Fatalf("bad body: %q", p.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("message never arrived")
	}

	if len(acked) != 1 || acked[0] != "B" {
		t.Fatalf("expected ack from B, got %v", acked)
	}
}

func TestDedup(t *testing.T) {
	a, _ := Listen(net.IPv4(127, 0, 0, 1), 0, "id-a", "A")
	defer a.Close()
	b, _ := Listen(net.IPv4(127, 0, 0, 1), 0, "id-b", "B")
	defer b.Close()

	count := make(chan int, 8)
	n := 0
	b.OnMsg = func(p core.Packet, from *net.UDPAddr) {
		n++
		count <- n
	}

	bPort := b.conn.LocalAddr().(*net.UDPAddr).Port
	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: bPort}
	pkt := core.Packet{V: 1, T: "msg", ID: "dup", SID: "id-a", Body: "x"}

	_, _ = a.Send([]*net.UDPAddr{target}, pkt, 200*time.Millisecond)
	_, _ = a.Send([]*net.UDPAddr{target}, pkt, 200*time.Millisecond)

	if len(count) != 1 {
		t.Fatalf("expected 1 delivery after dedup, got %d", len(count))
	}
}
