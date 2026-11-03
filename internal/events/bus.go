package events

import (
	"sync"

	"xlan/core"
)

// Bus 是进程内的事件总线，SSE 与 CLI 订阅共用。
type Bus struct {
	mu   sync.Mutex
	subs map[int]chan core.Event
	next int
}

func New() *Bus {
	return &Bus{subs: make(map[int]chan core.Event)}
}

// Publish 非阻塞地广播事件，慢订阅者丢事件而不是拖住发布方。
func (b *Bus) Publish(ev core.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribe 返回事件通道和取消函数。
func (b *Bus) Subscribe() (<-chan core.Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan core.Event, 64)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
	}
}
