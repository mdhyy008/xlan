package core

// 网络与协议常量。
const (
	DiscoverPort  = 43501
	PublicPort    = 43502
	APIBase       = 43503
	APISlots      = 3
	MaxPacket     = 1200
	MaxPublicBody = 1000
)

// 退出码，与 README 中的约定一致。
const (
	ExitOK       = 0
	ExitError    = 1
	ExitUsage    = 2
	ExitNoPeer   = 3
	ExitDenied   = 4
	ExitTimeout  = 5
	ExitChecksum = 6
)
