package core

// 网络与协议常量。
const (
	DiscoverPort  = 43001
	PublicPort    = 43002
	APIBase       = 43003
	APISlots      = 3
	MaxPacket     = 1200
	MaxPublicBody = 1000
	Version       = "0.1.0"
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
