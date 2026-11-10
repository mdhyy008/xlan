package core

import (
	"encoding/json"
	"errors"
)

var ErrPacketTooLarge = errors.New("packet exceeds 1200 bytes")

// Packet 是发现与公共文本共用的线上包，用 T 区分类型：
// probe / announce / bye / msg / ack。bye 表示设备正常退出，收到后立即标为离线。
type Packet struct {
	V        int    `json:"v"`
	T        string `json:"t"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	SID      string `json:"sid,omitempty"`
	SName    string `json:"sname,omitempty"`
	RID      string `json:"rid,omitempty"`
	RName    string `json:"rname,omitempty"`
	Port     int    `json:"port,omitempty"`
	Platform string `json:"platform,omitempty"`
	Channel  string `json:"channel,omitempty"`
	Body     string `json:"body,omitempty"`
	Ts       int64  `json:"ts,omitempty"`
}

func (p Packet) Marshal() ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxPacket {
		return nil, ErrPacketTooLarge
	}
	return b, nil
}

func ParsePacket(b []byte) (Packet, error) {
	var p Packet
	if err := json.Unmarshal(b, &p); err != nil {
		return Packet{}, err
	}
	if p.V != 1 {
		return Packet{}, errors.New("unsupported protocol version")
	}
	return p, nil
}
