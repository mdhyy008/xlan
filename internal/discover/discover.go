package discover

import (
	"context"
	"net"
	"sync"
	"time"

	"xlan/core"
	"xlan/internal/events"
	"xlan/internal/store"
)

// Discoverer 负责 UDP 广播发现：应答别人的 probe，也主动发 probe。
type Discoverer struct {
	Dev   core.Device
	API   int
	Store *store.Store
	Bus   *events.Bus

	Port int

	conn   *net.UDPConn
	send   func(*net.UDPAddr, []byte) error
	mu     sync.RWMutex
	closed bool
}

func New(dev core.Device, apiPort int, st *store.Store, bus *events.Bus) *Discoverer {
	return &Discoverer{Dev: dev, API: apiPort, Store: st, Bus: bus, Port: core.DiscoverPort}
}

// Listen 绑定发现端口并开始收包。
func (d *Discoverer) Listen(ip net.IP, port int) error {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: port})
	if err != nil {
		return err
	}
	d.conn = conn
	d.Port = port
	d.send = func(addr *net.UDPAddr, b []byte) error {
		_, err := conn.WriteToUDP(b, addr)
		return err
	}
	go d.readLoop()
	return nil
}

func (d *Discoverer) readLoop() {
	buf := make([]byte, 2048)
	for {
		n, from, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		pkt, err := core.ParsePacket(buf[:n])
		if err != nil {
			continue
		}
		d.HandlePacket(pkt, from)
	}
}

func (d *Discoverer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	if d.conn != nil {
		return d.conn.Close()
	}
	return nil
}

// HandlePacket 处理一个已解析的包。send 为 nil 时只做接收侧处理。
func (d *Discoverer) HandlePacket(p core.Packet, from *net.UDPAddr) {
	if p.ID == "" || p.ID == d.Dev.ID {
		return
	}
	d.mu.RLock()
	send := d.send
	d.mu.RUnlock()

	switch p.T {
	case "probe":
		if send == nil {
			return
		}
		ann := core.Packet{
			V: 1, T: "announce", ID: d.Dev.ID, Name: d.Dev.Name,
			Port: d.API, Platform: d.Dev.Platform, Ts: time.Now().Unix(),
		}
		if b, err := ann.Marshal(); err == nil {
			_ = send(from, b)
		}
	case "announce":
		if p.Port == 0 {
			return
		}
		peer := core.Peer{
			ID: p.ID, Name: p.Name, Addr: from.IP.String(), Port: p.Port,
			Platform: p.Platform, LastSeen: time.Now().Unix(),
		}
		if err := d.Store.UpsertPeer(peer); err != nil {
			return
		}
		if d.Bus != nil {
			d.Bus.Publish(core.Event{Type: core.EvPeerOnline, Ts: time.Now().Unix(), Data: peer})
		}
	}
}

// targets 返回广播目标：受限广播地址 + 各网卡定向广播地址。
func (d *Discoverer) targets() []*net.UDPAddr {
	ips, err := BroadcastAddrs()
	if err != nil || len(ips) == 0 {
		ips = []net.IP{net.IPv4bcast}
	}
	out := make([]*net.UDPAddr, 0, len(ips))
	for _, ip := range ips {
		out = append(out, &net.UDPAddr{IP: ip, Port: d.Port})
	}
	return out
}

// Announce 向局域网广播本机在线。
func (d *Discoverer) Announce() {
	d.mu.RLock()
	send := d.send
	d.mu.RUnlock()
	if send == nil {
		return
	}
	p := core.Packet{
		V: 1, T: "announce", ID: d.Dev.ID, Name: d.Dev.Name,
		Port: d.API, Platform: d.Dev.Platform, Ts: time.Now().Unix(),
	}
	b, err := p.Marshal()
	if err != nil {
		return
	}
	for _, a := range d.targets() {
		_ = send(a, b)
	}
}

// Scan 发 probe 并等待应答，返回本轮刷新到的设备。
func (d *Discoverer) Scan(ctx context.Context, wait time.Duration) ([]core.Peer, error) {
	d.mu.RLock()
	send := d.send
	d.mu.RUnlock()

	start := time.Now().Unix()
	if send != nil {
		p := core.Packet{V: 1, T: "probe", ID: d.Dev.ID, Name: d.Dev.Name}
		if b, err := p.Marshal(); err == nil {
			for _, a := range d.targets() {
				_ = send(a, b)
			}
		}
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}

	peers, err := d.Store.ListPeers()
	if err != nil {
		return nil, err
	}
	out := make([]core.Peer, 0, len(peers))
	for _, p := range peers {
		if p.LastSeen >= start {
			out = append(out, p)
		}
	}
	return out, nil
}

// BroadcastAddrs 计算所有活动网卡的定向广播地址。
func BroadcastAddrs() ([]net.IP, error) {
	out := []net.IP{net.IPv4bcast}
	ifaces, err := net.Interfaces()
	if err != nil {
		return out, err
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			mask := ipnet.Mask
			if ip4 == nil || len(mask) != 4 {
				continue
			}
			b := make(net.IP, 4)
			for i := 0; i < 4; i++ {
				b[i] = ip4[i] | ^mask[i]
			}
			out = append(out, b)
		}
	}
	return out, nil
}
