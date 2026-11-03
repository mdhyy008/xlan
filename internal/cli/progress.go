package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"xlan/core"
	"xlan/internal/client"
)

// progress 在 stderr 上原地刷新一行进度：百分比、已传/总量、实时速度。
type progress struct {
	out     io.Writer
	label   string
	total   int64
	done    int64
	speed   float64
	lastAt  time.Time
	lastPos int64
	started time.Time
	enabled bool
}

func newProgress(out io.Writer, label string, total int64, enabled bool) *progress {
	now := time.Now()
	return &progress{out: out, label: label, total: total, enabled: enabled, lastAt: now, started: now}
}

func (p *progress) update(done int64) {
	if !p.enabled {
		return
	}
	p.done = done

	now := time.Now()
	if dt := now.Sub(p.lastAt).Seconds(); dt >= 0.4 {
		inst := float64(done-p.lastPos) / dt
		if p.speed == 0 {
			p.speed = inst
		} else {
			p.speed = p.speed*0.6 + inst*0.4
		}
		p.lastAt, p.lastPos = now, done
	}
	if p.speed == 0 {
		// 第一帧还没攒出速度，用平均速度先顶上，避免显示 0 B/s
		if el := now.Sub(p.started).Seconds(); el > 0.1 {
			p.speed = float64(done) / el
		}
	}
	p.draw()
}

func (p *progress) draw() {
	if !p.enabled {
		return
	}
	pct := 0.0
	if p.total > 0 {
		pct = float64(p.done) * 100 / float64(p.total)
		if pct > 100 {
			pct = 100
		}
	}
	fmt.Fprintf(p.out, "\r%s  %5.1f%%  %s / %s  %s/s\x1b[K",
		p.label, pct, humanBytes(p.done), humanBytes(p.total), humanBytes(int64(p.speed)))
}

func (p *progress) finish() {
	if !p.enabled {
		return
	}
	if p.total > 0 {
		p.done = p.total
	}
	p.draw()
	fmt.Fprintln(p.out)
}

// humanBytes 把字节数变成 "12.3 MB" 这样的可读形式。
func humanBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// isTerminal 判断这个文件是不是终端，用来决定要不要画进度条。
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// showProgress 只在真终端上画进度，管道和日志里不刷屏。
func (a *App) showProgress() bool {
	if a.Quiet {
		return false
	}
	f, ok := a.Err.(*os.File)
	return ok && isTerminal(f)
}

type progressEvent struct {
	ID       string `json:"id"`
	Received int64  `json:"received"`
	Total    int64  `json:"total"`
}

type itemUpdate struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func decodeProgress(v any) (progressEvent, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return progressEvent{}, false
	}
	var p progressEvent
	if err := json.Unmarshal(b, &p); err != nil || p.ID == "" {
		return progressEvent{}, false
	}
	return p, true
}

func decodeItemUpdate(v any) (itemUpdate, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return itemUpdate{}, false
	}
	var u itemUpdate
	if err := json.Unmarshal(b, &u); err != nil || u.ID == "" {
		return itemUpdate{}, false
	}
	return u, true
}

// followTransfer 接收后盯着这个传输的进度，直到完成或长时间没动静。
func (a *App) followTransfer(base string, it core.Item) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	ch, err := client.Watch(ctx, base)
	if err != nil {
		return
	}

	bar := newProgress(a.Err, "接收 "+it.Name, it.Size, true)
	defer bar.finish()

	// 订阅前可能已经传完了，先查一次。
	if st, err := client.GetOffer(ctx, base, it.ID); err == nil && st.Status == string(core.StatusDone) {
		return
	}

	idle := time.NewTimer(30 * time.Second)
	defer idle.Stop()

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			switch ev.Type {
			case core.EvTransfer:
				if p, ok := decodeProgress(ev.Data); ok && p.ID == it.ID {
					bar.update(p.Received)
					if !idle.Stop() {
						select {
						case <-idle.C:
						default:
						}
					}
					idle.Reset(30 * time.Second)
				}
			case core.EvItemUpdate:
				if u, ok := decodeItemUpdate(ev.Data); ok && u.ID == it.ID {
					if u.Status == string(core.StatusDone) || u.Status == string(core.StatusFailed) {
						return
					}
				}
			}
		case <-idle.C:
			fmt.Fprintln(a.Err, "30 秒没有新数据，对方可能已经中断。")
			return
		case <-ctx.Done():
			return
		}
	}
}

// bucket 是一个简单的令牌桶，用来限速。
type bucket struct {
	mu     sync.Mutex
	rate   int64
	tokens float64
	last   time.Time
}

func newBucket(rate int64) *bucket {
	if rate <= 0 {
		return nil
	}
	return &bucket{rate: rate, tokens: float64(rate) / 4, last: time.Now()}
}

func (b *bucket) wait(n int) {
	if b == nil || b.rate <= 0 || n <= 0 {
		return
	}
	for n > 0 {
		b.mu.Lock()
		now := time.Now()
		b.tokens += now.Sub(b.last).Seconds() * float64(b.rate)
		if b.tokens > float64(b.rate) {
			b.tokens = float64(b.rate) // 最多攒一秒的量
		}
		b.last = now
		avail := int(b.tokens)
		if avail <= 0 {
			need := time.Duration(float64(n) / float64(b.rate) * float64(time.Second))
			b.mu.Unlock()
			time.Sleep(need)
			continue
		}
		take := n
		if take > avail {
			take = avail
		}
		b.tokens -= float64(take)
		n -= take
		b.mu.Unlock()
	}
}

type rateReader struct {
	r io.Reader
	b *bucket
}

func (rr *rateReader) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	if n > 0 {
		rr.b.wait(n)
	}
	return n, err
}

// parseLimit 解析 "5M" / "500K" / "1048576" 这类限速写法。
func parseLimit(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" || s == "0" {
		return 0, nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "K"):
		mult, s = 1024, strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "M"):
		mult, s = 1024*1024, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "G"):
		mult, s = 1024*1024*1024, strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "KB"):
		mult, s = 1024, strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "MB"):
		mult, s = 1024*1024, strings.TrimSuffix(s, "MB")
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("限速写法不对：%q，例如 5M、500K", s)
	}
	return int64(n * float64(mult)), nil
}
