package pubtext

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"sort"
	"sync"
	"time"

	"xlan/core"
)

// Broadcaster 负责公共文本的 UDP 广播与回执收集。
// 一个进程只应有一个实例，它独占 43002 端口。
type Broadcaster struct {
	selfID   string
	selfName string
	conn     *net.UDPConn
	send     func(*net.UDPAddr, []byte) error

	mu    sync.Mutex
	acked map[string]map[string]string
	seen  map[string]int64

	// OnMsg 在收到别人的公共消息时回调。
	OnMsg func(p core.Packet, from *net.UDPAddr)
}

func Listen(ip net.IP, port int, id, name string) (*Broadcaster, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: port})
	if err != nil {
		return nil, err
	}
	b := &Broadcaster{
		selfID:   id,
		selfName: name,
		conn:     conn,
		acked:    map[string]map[string]string{},
		seen:     map[string]int64{},
	}
	b.send = func(addr *net.UDPAddr, data []byte) error {
		_, err := conn.WriteToUDP(data, addr)
		return err
	}
	go b.readLoop()
	return b, nil
}

func (b *Broadcaster) Close() error {
	if b.conn != nil {
		return b.conn.Close()
	}
	return nil
}

func (b *Broadcaster) readLoop() {
	buf := make([]byte, 2048)
	for {
		n, from, err := b.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		p, err := core.ParsePacket(buf[:n])
		if err != nil {
			continue
		}
		b.handle(p, from)
	}
}

func (b *Broadcaster) handle(p core.Packet, from *net.UDPAddr) {
	switch p.T {
	case "msg":
		if p.SID == "" || p.SID == b.selfID {
			return
		}
		b.mu.Lock()
		if _, dup := b.seen[p.ID]; dup {
			b.mu.Unlock()
			return
		}
		b.seen[p.ID] = time.Now().Unix()
		if len(b.seen) > 2048 {
			b.seen = map[string]int64{p.ID: time.Now().Unix()}
		}
		cb := b.OnMsg
		b.mu.Unlock()

		if cb != nil {
			cb(p, from)
		}
		ack := core.Packet{
			V: 1, T: "ack", ID: p.ID,
			RID: b.selfID, RName: b.selfName, Ts: time.Now().Unix(),
		}
		if data, err := ack.Marshal(); err == nil {
			_ = b.send(from, data)
		}
	case "ack":
		b.mu.Lock()
		defer b.mu.Unlock()
		if m, ok := b.acked[p.ID]; ok && p.RID != "" {
			m[p.RID] = p.RName
		}
	}
}

// Send 广播一条公共消息，连发两遍，并在 wait 内收集回执。
func (b *Broadcaster) Send(targets []*net.UDPAddr, p core.Packet, wait time.Duration) ([]string, error) {
	if p.ID == "" {
		p.ID = newID()
	}
	b.mu.Lock()
	b.acked[p.ID] = map[string]string{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.acked, p.ID)
		b.mu.Unlock()
	}()

	data, err := p.Marshal()
	if err != nil {
		return nil, err
	}
	for round := 0; round < 2; round++ {
		for _, t := range targets {
			_ = b.send(t, data)
		}
		if round == 0 {
			time.Sleep(30 * time.Millisecond)
		}
	}

	time.Sleep(wait)

	b.mu.Lock()
	defer b.mu.Unlock()
	names := make([]string, 0, len(b.acked[p.ID]))
	for _, n := range b.acked[p.ID] {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

func newID() string {
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
