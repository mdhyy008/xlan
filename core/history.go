package core

// TransferRecord 是一条收发记录，跟收件箱分开存，用来回溯历史。
type TransferRecord struct {
	ID         int64  `json:"id,omitempty"`
	Ts         int64  `json:"ts"`
	Direction  string `json:"direction"` // send | recv
	PeerName   string `json:"peer_name,omitempty"`
	Name       string `json:"name,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}
