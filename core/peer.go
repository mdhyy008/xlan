package core

import (
	"net"
	"strconv"
)

// Peer 是一台被发现的对端设备。
type Peer struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Addr     string `json:"addr,omitempty"`
	Port     int    `json:"port,omitempty"`
	Platform string `json:"platform,omitempty"`
	LastSeen int64  `json:"last_seen"`
	Trusted  bool   `json:"trusted"`
}

// BaseURL 返回对端 HTTP API 的根地址，端口未知时为空。
func (p Peer) BaseURL() string {
	if p.Port == 0 {
		return ""
	}
	return "http://" + net.JoinHostPort(p.Addr, strconv.Itoa(p.Port))
}
