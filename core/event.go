package core

// 事件类型，CLI 输出与 SSE 共用一个结构。
const (
	EvPeerOnline  = "peer.online"
	EvPeerOffline = "peer.offline"
	EvItemNew     = "item.new"
	EvItemUpdate  = "item.update"
	EvTransfer    = "transfer.progress"
)

// Event 是一条推给订阅者的事件。
type Event struct {
	Type string `json:"type"`
	Ts   int64  `json:"ts"`
	Data any    `json:"data,omitempty"`
}
