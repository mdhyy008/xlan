package core

// Kind 区分文本与文件。
type Kind string

const (
	KindText   Kind = "text"
	KindFile   Kind = "file"
	KindFolder Kind = "folder"
)

// Channel 区分私聊与公共。
type Channel string

const (
	ChanDM     Channel = "dm"
	ChanPublic Channel = "public"
)

// Status 是收件箱条目的状态。
type Status string

const (
	StatusPending Status = "pending"
	StatusActive  Status = "active"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
	StatusExpired Status = "expired"
)

// PublicTo 是公共消息的收件方标识。
const PublicTo = "*"

// Item 是收件箱里的一条记录，文本与文件共用。
type Item struct {
	ID        string  `json:"id"`
	Kind      Kind    `json:"kind"`
	FromID    string  `json:"from_id"`
	FromName  string  `json:"from_name,omitempty"`
	ToID      string  `json:"to_id"`
	Channel   Channel `json:"channel"`
	Body      string  `json:"body,omitempty"`
	Name      string  `json:"name,omitempty"`
	Size      int64   `json:"size,omitempty"`
	SHA256    string  `json:"sha256,omitempty"`
	Status    Status  `json:"status"`
	Path      string  `json:"path,omitempty"`
	CreatedAt int64   `json:"created_at"`
}

func (i Item) IsText() bool { return i.Kind == KindText }
