package discover

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"

	"xlan/core"
	"xlan/internal/store"
)

// ProbeTCP 是组播/广播被过滤时的兜底：并发探测本机所在 /24 网段的
// HTTP 服务，谁应答 /v1/info 就是一台设备。
func ProbeTCP(ctx context.Context, apiPort int, timeout time.Duration) []core.Peer {
	nets, err := localIPv4Nets()
	if err != nil {
		return nil
	}
	var (
		mu  sync.Mutex
		out []core.Peer
		wg  sync.WaitGroup
	)
	sem := make(chan struct{}, 64)
	client := &http.Client{Timeout: timeout}

	for _, n := range nets {
		base := n.IP.Mask(n.Mask).To4()
		if base == nil {
			continue
		}
		for i := 1; i < 255; i++ {
			ip := net.IPv4(base[0], base[1], base[2], byte(i)).String()
			wg.Add(1)
			go func(ip string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				if ctx.Err() != nil {
					return
				}
				p, ok := probeOne(ctx, client, ip, apiPort)
				if ok {
					mu.Lock()
					out = append(out, p)
					mu.Unlock()
				}
			}(ip)
		}
	}
	wg.Wait()
	return out
}

func probeOne(ctx context.Context, client *http.Client, ip string, port int) (core.Peer, bool) {
	url := "http://" + net.JoinHostPort(ip, itoa(port)) + "/v1/info"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return core.Peer{}, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return core.Peer{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return core.Peer{}, false
	}
	var info struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Platform string `json:"platform"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || info.ID == "" {
		return core.Peer{}, false
	}
	return core.Peer{
		ID: info.ID, Name: info.Name, Addr: ip, Port: port,
		Platform: info.Platform, LastSeen: time.Now().Unix(),
	}, true
}

// ScanTCP 探测并写库，返回本轮新增/刷新的设备。
func ScanTCP(ctx context.Context, st *store.Store, apiPort int, timeout time.Duration) []core.Peer {
	peers := ProbeTCP(ctx, apiPort, timeout)
	for _, p := range peers {
		_ = st.UpsertPeer(p)
	}
	return peers
}

func localIPv4Nets() ([]*net.IPNet, error) {
	var out []*net.IPNet
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
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
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			out = append(out, ipnet)
		}
	}
	return out, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
