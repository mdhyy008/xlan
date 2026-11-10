package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"xlan/core"
	"xlan/internal/cache"
	"xlan/internal/client"
	"xlan/internal/config"
	"xlan/internal/discover"
	"xlan/internal/install"
	"xlan/internal/secure"
	"xlan/internal/serve"
	"xlan/internal/spawn"
	"xlan/internal/store"
)

const chunkSize = 4 << 20

// App 保存一次命令调用的全局状态。
type App struct {
	Cfg     *config.Config
	CfgDir  string
	JSON    bool
	Quiet   bool
	Timeout time.Duration
	Limit   int64 // 字节/秒，0 表示不限速
	Out     io.Writer
	Err     io.Writer

	limiter *bucket
	pause   *pauser

	// outMu 让并发上传时进度行和结果行不会写串。
	outMu sync.Mutex

	// Dev 缓存本机身份；测试里可直接注入。
	Dev core.Device
}

// Run 是 CLI 的入口，返回进程退出码。
func Run(args []string) int {
	_ = config.EnsureMigrated()
	a := &App{Out: os.Stdout, Err: os.Stderr, Timeout: 60 * time.Second}
	if len(args) == 0 {
		a.usage()
		return core.ExitOK
	}
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "scan":
		return a.cmdScan(rest)
	case "status":
		return a.cmdStatus(rest)
	case "serve":
		return a.cmdServe(rest)
	case "stop":
		return a.cmdStop(rest)
	case "autostart":
		return a.cmdAutostart(rest)
	case "path":
		return a.cmdPath(rest)
	case "text":
		return a.cmdText(rest)
	case "send":
		return a.cmdSend(rest)
	case "inbox":
		return a.cmdInbox(rest)
	case "recv":
		return a.cmdRecv(rest)
	case "trust":
		return a.cmdTrust(rest)
	case "doctor":
		return a.cmdDoctor(rest)
	case "config":
		return a.cmdConfig(rest)
	case "cache":
		return a.cmdCache(rest)
	case "log":
		return a.cmdLog(rest)
	case "sends":
		return a.cmdSends(rest)
	case "pin":
		return a.cmdPin(rest)
	case "pause":
		return a.cmdSignal(rest, true)
	case "resume":
		return a.cmdSignal(rest, false)
	case "help", "-h", "--help":
		a.usage()
		return core.ExitOK
	case "version", "--version":
		fmt.Fprintf(a.Out, "xlan %s\n", core.VersionString())
		return core.ExitOK
	}

	fmt.Fprintf(a.Err, "xlan: 未知命令 %q\n\n", cmd)
	a.usage()
	return core.ExitUsage
}

func (a *App) usage() {
	fmt.Fprint(a.Out, `xlan — 局域网传输

  xlan scan [--deep]                发现局域网设备
  xlan status                       服务状态、端口、设备
  xlan stop                         停掉后台服务
  xlan autostart on|off|status      开机自启
  xlan path on|off|status           把程序目录加进 PATH
  xlan serve [-d] [--auto-accept]   前台跑服务，-d 放后台，--auto-accept 自动接收
  xlan text <内容|-> [--to 设备]     发文本，省略 --to 就是公共
  xlan send <文件|目录|->... (--to 设备)  发文件，可多个，--all 群发，--limit 限速
  xlan inbox [-n 20] [--since 2h]   收件箱
  xlan inbox --watch [-i]           实时跟随，-i 可边看边接收
  xlan inbox accept|reject [序号]  处理待确认的传输
  xlan inbox cancel [序号]         取消卡住的传输
  xlan inbox cancel --all          取消全部未完成的传输
  xlan inbox accept --all          接收全部待确认的传输
  xlan inbox clean [--older-than 30d] [--with-files]  清理旧记录
  xlan cache clean [--older-than 30d] [--dry-run]     清理临时分片和旧收件箱记录
  xlan recv <id> [--out 路径] [-o -]  取出内容
  xlan trust add|rm <设备> | list   信任白名单
  xlan sends                        正在进行的发送任务
  xlan pin list|forget [主机]      查看/清除已信任的证书指纹
  xlan pause|resume <id>           暂停 / 继续某个发送任务
  xlan log [-n 20]                  收发记录
  xlan log clean [--older-than 30d] 清理收发记录
  xlan config [get|set|unset]       查看和修改配置
  xlan doctor                       网络自检

全局参数：--json --quiet --timeout 60s --config 目录
`)
}

func (a *App) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.Err)
	fs.BoolVar(&a.JSON, "json", false, "输出 NDJSON")
	fs.BoolVar(&a.Quiet, "quiet", false, "不打印进度")
	fs.DurationVar(&a.Timeout, "timeout", 60*time.Second, "等待超时")
	fs.StringVar(&a.CfgDir, "config", "", "配置目录")
	return fs
}

// parseInterspersed 允许参数和选项混着写，例如 `xlan text hi --json`。
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return rest, nil
		}
		rest = append(rest, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func (a *App) load() error {
	cfg, err := config.Load(a.CfgDir)
	if err != nil {
		return err
	}
	a.Cfg = cfg
	a.setupSecure()
	return nil
}

// setupSecure 按配置决定要不要走加密，并准备指纹库。
func (a *App) setupSecure() {
	if !a.Cfg.UseTLS() {
		secure.Disable()
		return
	}
	dataDir, err := config.DataDir()
	if err != nil {
		secure.Disable()
		return
	}
	pins := secure.LoadPins(filepath.Join(dataDir, "pins.json"))
	pins.OnNew = func(host, fp string) {
		fmt.Fprintf(a.Err, "第一次连接 %s，已记住证书指纹 %s\n", host, fp[:16])
	}
	secure.Enable(pins)
}

func (a *App) emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	a.outMu.Lock()
	defer a.outMu.Unlock()
	fmt.Fprintln(a.Out, string(b))
}

func (a *App) fail(code string, exit int, msg string) int {
	if a.JSON {
		a.emit(map[string]any{"ok": false, "code": code, "msg": msg})
	} else {
		fmt.Fprintf(a.Err, "xlan: %s\n", msg)
	}
	return exit
}

func (a *App) warnf(format string, args ...any) {
	if a.Quiet {
		return
	}
	a.outMu.Lock()
	defer a.outMu.Unlock()
	fmt.Fprintf(a.Err, format+"\n", args...)
}

// requireService 只检查服务在不在，不会偷偷拉起来。
// 需求是：必须自己敲 xlan serve，才允许有服务在跑。
func (a *App) requireService() (string, int) {
	base, probeErr := spawn.FindErr(context.Background(), a.Cfg)
	if base == "" {
		if probeErr != nil {
			return "", a.fail("TLS_MISMATCH", core.ExitError, probeErr.Error())
		}
		return "", a.fail("NO_SERVICE", core.ExitError,
			"服务没有在运行。另开一个终端敲 `xlan serve`，它会一直在前台跑着。")
	}
	return base, 0
}

// ---------- scan ----------

func (a *App) cmdScan(args []string) int {
	fs := a.flags("scan")
	deep := fs.Bool("deep", false, "同时扫描本机 /24 网段")
	if err := fs.Parse(args); err != nil {
		return core.ExitUsage
	}
	if err := a.load(); err != nil {
		return a.fail("CONFIG", core.ExitError, err.Error())
	}
	base, code := a.requireService()
	if code != 0 {
		return code
	}
	peers, err := client.Scan(context.Background(), base, *deep)
	if err != nil {
		return a.fail("SCAN", core.ExitError, err.Error())
	}
	if a.JSON {
		a.emit(map[string]any{"ok": true, "peers": peers})
		return core.ExitOK
	}
	if len(peers) == 0 {
		fmt.Fprintln(a.Out, "没发现设备。换个网段或加 --deep 再试。")
		return core.ExitOK
	}
	fmt.Fprintf(a.Out, "%-20s %-10s %-16s %s\n", "名称", "平台", "地址", "id")
	for _, p := range peers {
		fmt.Fprintf(a.Out, "%-20s %-10s %-16s %s\n", p.Name, p.Platform, p.Addr, p.ID)
	}
	return core.ExitOK
}

// ---------- status ----------

func (a *App) cmdStatus(args []string) int {
	fs := a.flags("status")
	if err := fs.Parse(args); err != nil {
		return core.ExitUsage
	}
	if err := a.load(); err != nil {
		return a.fail("CONFIG", core.ExitError, err.Error())
	}
	ctx := context.Background()
	base, probeErr := spawn.FindErr(ctx, a.Cfg)

	peers := []core.Peer{}
	autoAccept := false
	if base != "" {
		if ps, err := client.Peers(ctx, base); err == nil {
			peers = ps
		}
		if info, err := client.GetInfo(ctx, base); err == nil {
			autoAccept = info.AutoAccept
		}
	}

	if a.JSON {
		a.emit(map[string]any{
			"ok": base != "", "base": base, "running": base != "",
			"ports": a.Cfg.APIPorts, "recv_dir": a.Cfg.RecvDirPath(),
			"auto_accept": autoAccept, "peers": peers,
		})
		return core.ExitOK
	}
	fmt.Fprintf(a.Out, "服务： %s\n", orDash(base))
	if base == "" && probeErr != nil {
		fmt.Fprintf(a.Out, "原因： %s\n", probeErr.Error())
	}
	fmt.Fprintf(a.Out, "模式： %s\n", map[bool]string{true: "自动接收", false: "需要确认"}[autoAccept])
	fmt.Fprintf(a.Out, "端口： %v\n", a.Cfg.APIPorts)
	fmt.Fprintf(a.Out, "收件： %s\n", a.Cfg.RecvDirPath())
	if len(peers) > 0 {
		fmt.Fprintf(a.Out, "设备： %d 台\n", len(peers))
		for _, p := range peers {
			mark := " "
			if p.Trusted {
				mark = "*"
			}
			fmt.Fprintf(a.Out, "  %s %-18s %-16s %s\n", mark, p.Name, p.Addr, p.ID)
		}
	}
	return core.ExitOK
}

func orDash(s string) string {
	if s == "" {
		return "没有在跑"
	}
	return s
}

// ---------- serve ----------

func (a *App) cmdServe(args []string) int {
	fs := a.flags("serve")
	var detach bool
	fs.BoolVar(&detach, "detach", false, "后台运行")
	fs.BoolVar(&detach, "d", false, "后台运行")
	autoAccept := fs.Bool("auto-accept", false, "自动接收所有文件，不再等确认")
	if err := fs.Parse(args); err != nil {
		return core.ExitUsage
	}
	if err := a.load(); err != nil {
		return a.fail("CONFIG", core.ExitError, err.Error())
	}

	ctx := context.Background()
	if detach {
		if base := spawn.Find(ctx, a.Cfg); base != "" {
			fmt.Printf("xlan 服务已经在运行了：%s\n", base)
			return core.ExitOK
		}
		if err := spawn.Start(a.CfgDir, *autoAccept); err != nil {
			return a.fail("SPAWN", core.ExitError, err.Error())
		}
		deadline := time.Now().Add(10 * time.Second)
		var lastProbeErr error
		for time.Now().Before(deadline) {
			base, probeErr := spawn.FindErr(ctx, a.Cfg)
			if probeErr != nil {
				lastProbeErr = probeErr
			}
			if base == "" {
				time.Sleep(150 * time.Millisecond)
				continue
			}
			info, _ := client.GetInfo(ctx, base)
			if a.JSON {
				a.emit(map[string]any{"ok": true, "base": base, "port": info.Port, "detached": true})
			} else {
				fmt.Printf("xlan 服务已在后台启动：%s\n设备 %s，收件 %s\n停掉用 xlan stop。\n",
					base, info.Name, a.Cfg.RecvDirPath())
			}
			return core.ExitOK
		}
		if lastProbeErr != nil {
			return a.fail("SPAWN", core.ExitError, lastProbeErr.Error())
		}
		return a.fail("SPAWN", core.ExitError, "后台服务没有起来")
	}

	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	if err := serve.Run(runCtx, a.CfgDir, *autoAccept); err != nil {
		return a.fail("SERVE", core.ExitError, err.Error())
	}
	return core.ExitOK
}

// cmdStop 停掉在跑的服务。
func (a *App) cmdStop(args []string) int {
	fs := a.flags("stop")
	if err := fs.Parse(args); err != nil {
		return core.ExitUsage
	}
	if err := a.load(); err != nil {
		return a.fail("CONFIG", core.ExitError, err.Error())
	}
	ctx := context.Background()
	base := spawn.Find(ctx, a.Cfg)
	if base == "" {
		if a.JSON {
			a.emit(map[string]any{"ok": true, "stopped": false})
		} else {
			fmt.Fprintln(a.Out, "服务本来就没在运行。")
		}
		return core.ExitOK
	}
	if err := client.Shutdown(ctx, base); err != nil {
		return a.fail("STOP", core.ExitError, err.Error())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if spawn.Find(ctx, a.Cfg) == "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if base := spawn.Find(ctx, a.Cfg); base != "" {
		return a.fail("STOP", core.ExitError, "服务还在运行（"+base+"）。可能卡住了，用 xlan serve -d 重启一次。")
	}
	if a.JSON {
		a.emit(map[string]any{"ok": true, "stopped": true})
	} else {
		fmt.Fprintln(a.Out, "服务已停止。")
	}
	return core.ExitOK
}

// ---------- text ----------

func (a *App) cmdText(args []string) int {
	fs := a.flags("text")
	to := fs.String("to", "", "目标设备")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return core.ExitUsage
	}
	if err := a.load(); err != nil {
		return a.fail("CONFIG", core.ExitError, err.Error())
	}

	body := strings.Join(rest, " ")
	if body == "-" || (body == "" && !isTTY()) {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 10<<20))
		if err != nil {
			return a.fail("STDIN", core.ExitError, err.Error())
		}
		body = string(b)
	}
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return a.fail("EMPTY", core.ExitUsage, "内容为空")
	}

	base, code := a.requireService()
	if code != 0 {
		return code
	}
	resp, err := client.SendText(context.Background(), base, body, *to)
	if err != nil {
		code := core.ExitError
		name := "SEND"
		if strings.Contains(err.Error(), "NOT_FOUND") {
			code, name = core.ExitNoPeer, "NOT_FOUND"
		}
		return a.fail(name, code, err.Error())
	}

	if a.JSON {
		a.emit(map[string]any{"ok": true, "channel": resp.Channel, "acked": resp.Acked,
			"degraded": resp.Degraded, "warn": resp.Warn})
		return core.ExitOK
	}
	if resp.Degraded {
		fmt.Fprintf(a.Out, "已逐台投递到 %d 台设备\n", len(resp.Acked))
		return core.ExitOK
	}
	if resp.Channel == "public" {
		if len(resp.Acked) == 0 {
			fmt.Fprintln(a.Out, "已广播（0 人回执）")
			a.warnf("没人回执，可能被 AP 隔离或省电策略挡了，跑一下 xlan doctor。")
		} else {
			fmt.Fprintf(a.Out, "已送达：%s\n", strings.Join(resp.Acked, "、"))
		}
		return core.ExitOK
	}
	fmt.Fprintf(a.Out, "已发送给 %s\n", strings.Join(resp.Acked, "、"))
	return core.ExitOK
}

// ---------- send ----------

func (a *App) cmdSend(args []string) int {
	fs := a.flags("send")
	to := fs.String("to", "", "目标设备")
	all := fs.Bool("all", false, "群发给所有在线设备")
	limit := fs.String("limit", "", "限速，比如 5M、500K")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return core.ExitUsage
	}
	if err := a.load(); err != nil {
		return a.fail("CONFIG", core.ExitError, err.Error())
	}
	if *limit != "" {
		v, err := parseLimit(*limit)
		if err != nil {
			return a.fail("LIMIT", core.ExitUsage, err.Error())
		}
		a.Limit = v
	}

	a.pause = newPauser()
	a.pause.Watch(func(paused bool) {
		if paused {
			fmt.Fprintln(a.Err, "已暂停，再按一次 Ctrl-Z 继续")
		} else {
			fmt.Fprintln(a.Err, "继续传输")
		}
	})
	if len(rest) == 0 {
		return a.fail("USAGE", core.ExitUsage, "用法：xlan send <文件|目录|->... --to 设备")
	}
	stdinCount := 0
	for _, p := range rest {
		if p == "-" {
			stdinCount++
		}
	}
	if stdinCount > 1 || (stdinCount == 1 && len(rest) > 1) {
		return a.fail("USAGE", core.ExitUsage, "stdin（-）只能单独发，不能和其它路径混用")
	}

	// 先全部检查一遍，避免发了一半才发现后面路径不存在
	type source struct {
		path  string
		size  int64
		isDir bool
	}
	sources := make([]source, 0, len(rest))
	cleanup := func() {}
	defer func() { cleanup() }()

	for _, p := range rest {
		src := p
		if p == "-" {
			tmp, err := bufferStdin()
			if err != nil {
				return a.fail("STDIN", core.ExitError, err.Error())
			}
			src = tmp
			prev := cleanup
			cleanup = func() { prev(); os.Remove(tmp) }
		}
		st, err := os.Stat(src)
		if err != nil {
			return a.fail("NOT_FOUND", core.ExitError, err.Error())
		}
		sources = append(sources, source{path: src, size: st.Size(), isDir: st.IsDir()})
	}

	base, code := a.requireService()
	if code != 0 {
		return code
	}
	// Ctrl-C 时先自己把已经建好的 offer 撤掉，对端马上就能看到
	// 「对方已取消」，不用干等十分钟过期。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	peers, err := client.Peers(ctx, base)
	if err != nil {
		return a.fail("PEERS", core.ExitError, err.Error())
	}

	var targets []core.Peer
	switch {
	case *all:
		targets = peers
	case *to != "":
		p, err := resolvePeer(peers, *to)
		if err != nil {
			return a.fail("NOT_FOUND", core.ExitNoPeer, err.Error())
		}
		targets = []core.Peer{p}
	default:
		switch len(peers) {
		case 0:
			return a.fail("NO_PEER", core.ExitNoPeer, "没发现在线设备，先跑 xlan scan")
		case 1:
			targets = peers
		default:
			if !isTTY() || a.JSON {
				return a.fail("NEED_TARGET", core.ExitUsage,
					"有多台设备，请用 --to 指定或加 --all。候选："+peerIDs(peers))
			}
			p, err := a.pickPeer(peers)
			if err != nil {
				return a.fail("CANCELED", core.ExitUsage, err.Error())
			}
			targets = []core.Peer{p}
		}
	}
	if len(targets) == 0 {
		return a.fail("NO_PEER", core.ExitNoPeer, "没发现在线设备")
	}

	paths := make([]string, 0, len(sources))
	var totalBytes int64
	for _, s := range sources {
		paths = append(paths, filepath.Base(s.path))
		totalBytes += s.size
	}
	targetName := ""
	if len(targets) > 0 {
		targetName = targets[0].Name
	}
	sendID, regErr := registerSend(sendEntry{
		PID: os.Getpid(), Started: time.Now().Unix(),
		Target: targetName, Paths: paths, Total: totalBytes,
	})
	if regErr == nil {
		defer unregisterSend(sendID)
		if !a.JSON {
			fmt.Fprintf(a.Out, "传输 id %s（Ctrl-Z 暂停，或 xlan pause %s）\n", sendID, sendID)
		}
	}

	tasks := make([]*sendTask, 0, len(sources)*len(targets))
	for _, srcIt := range sources {
		for _, p := range targets {
			tasks = append(tasks, &sendTask{
				src: srcIt.path, name: filepath.Base(srcIt.path),
				size: srcIt.size, isDir: srcIt.isDir, peer: p,
			})
		}
	}

	// 阶段一：先把整批文件的 offer 全部发过去，对端一次就能看到所有文件。
	var failedMu sync.Mutex
	total := len(tasks)
	failed := 0
	lastCode := "ERROR"
	ready := make([]*sendTask, 0, total)
	for _, t := range tasks {
		if err := a.offerTask(ctx, t); err != nil {
			if ctx.Err() == nil {
				a.reportTask(ctx, base, t, err, len(sources) == 1)
			}
			failed++
			lastCode = classify(err)
			continue
		}
		if t.done {
			a.reportTask(ctx, base, t, nil, len(sources) == 1)
			continue
		}
		ready = append(ready, t)
	}

	// 阶段二：最多三个文件同时上传，其余排队等空位。
	slots := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for _, t := range ready {
		wg.Add(1)
		go func(t *sendTask) {
			defer wg.Done()
			err := a.runTask(ctx, t, slots)
			if err != nil {
				failedMu.Lock()
				failed++
				lastCode = classify(err)
				failedMu.Unlock()
			}
			if ctx.Err() == nil {
				a.reportTask(ctx, base, t, err, len(sources) == 1)
			}
		}(t)
	}
	wg.Wait()

	// 被中断时，告诉每个已经建好 offer 的对端这批不发了。
	if ctx.Err() != nil {
		cancelCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
		for _, t := range tasks {
			if t.id == "" || t.done {
				continue
			}
			_ = client.DecisionBy(cancelCtx, t.base, t.id, "cancel", "sender")
		}
		stopCancel()
	}

	if failed == total {
		return exitForCode(lastCode)
	}
	return core.ExitOK
}

// exitForCode 把失败原因映射到约定好的退出码。
func exitForCode(code string) int {
	switch code {
	case "DENIED":
		return core.ExitDenied
	case "TIMEOUT":
		return core.ExitTimeout
	case "CHECKSUM":
		return core.ExitChecksum
	case "NO_PEER":
		return core.ExitNoPeer
	default:
		return core.ExitError
	}
}

// sendTask 是一次发送里最小的单位：一个文件或一个目录发给一台设备。
type sendTask struct {
	src       string
	name      string
	size      int64
	isDir     bool
	peer      core.Peer
	base      string
	id        string
	accepted  bool
	done      bool
	manifest  *core.Manifest
	doneFiles map[string]bool
}

// offerTask 只负责在对端建一条 offer，让文件立刻出现在接收方的列表里。
func (a *App) offerTask(ctx context.Context, t *sendTask) error {
	base := t.peer.BaseURL()
	if base == "" {
		return errors.New("这台设备没有可用地址")
	}
	t.base = base
	if t.isDir {
		m, err := buildManifest(t.src)
		if err != nil {
			return err
		}
		resp, err := client.Offer(ctx, base, client.OfferReq{
			FromID: a.self().ID, FromName: a.self().Name, ToID: t.peer.ID,
			Kind: string(core.KindFolder), Name: m.Root, Size: m.TotalSize(),
			Manifest: &m, TransferID: manifestID(m),
		})
		if err != nil {
			return err
		}
		t.manifest = &m
		t.size = m.TotalSize()
		t.id = resp.ID
		t.accepted = resp.Accepted
		t.doneFiles = map[string]bool{}
		for _, rel := range resp.DoneFiles {
			t.doneFiles[rel] = true
		}
		return nil
	}
	sum, err := fileSHA256(t.src)
	if err != nil {
		return err
	}
	resp, err := client.Offer(ctx, base, client.OfferReq{
		FromID: a.self().ID, FromName: a.self().Name, ToID: t.peer.ID,
		Kind: string(core.KindFile), Name: t.name, Size: t.size, SHA256: sum,
	})
	if err != nil {
		return err
	}
	if resp.Duplicate {
		t.done = true
		if a.JSON {
			a.emit(map[string]any{"ok": true, "file": t.name, "peer": t.peer.Name,
				"duplicate": true, "path": resp.Path})
		} else {
			a.outMu.Lock()
			fmt.Fprintf(a.Out, "%s 已经有同一个文件了，没再传。\n", t.peer.Name)
			a.outMu.Unlock()
		}
		return nil
	}
	t.id = resp.ID
	t.accepted = resp.Accepted
	return nil
}

// runTask 等对方确认，然后抢一个上传空位。排队期间持续查询，
// 免得对端把还没轮到、但已经确认过的 offer 当成断掉的传输收掉。
func (a *App) runTask(ctx context.Context, t *sendTask, slots chan struct{}) error {
	if !t.accepted {
		if a.JSON {
			a.emit(map[string]any{"event": "wait", "file": t.name, "peer": t.peer.Name})
		} else {
			a.warnf("等待 %s 确认…", t.peer.Name)
		}
		if err := a.waitAccepted(ctx, t.base, t.id); err != nil {
			return err
		}
	}
	queued := false
	for {
		select {
		case slots <- struct{}{}:
		default:
			if !queued {
				queued = true
				if a.JSON {
					a.emit(map[string]any{"event": "queue", "file": t.name, "peer": t.peer.Name})
				}
			}
			st, err := client.GetOffer(ctx, t.base, t.id)
			if err != nil {
				return err
			}
			if st.Status == string(core.StatusFailed) || st.Status == string(core.StatusExpired) {
				return errors.New("对方拒绝了")
			}
			select {
			case slots <- struct{}{}:
			case <-time.After(2 * time.Second):
			}
			continue
		}
		break
	}
	defer func() { <-slots }()
	if t.isDir {
		return a.uploadFolder(ctx, t)
	}
	return a.uploadFileTask(ctx, t)
}

func (a *App) uploadFileTask(ctx context.Context, t *sendTask) error {
	f, err := os.Open(t.src)
	if err != nil {
		return err
	}
	defer f.Close()

	prog := a.newTaskProgress(t, t.size)
	defer prog.finish()

	err = a.uploadOne(ctx, t.size, a.Cfg.Threads, prog.update, func(ctx context.Context, off, n int64) error {
		return a.putWithRetry(func() error {
			return client.PutRange(ctx, t.base, t.id, a.rate(io.NewSectionReader(f, off, n)), off, n, t.size)
		})
	})
	return err
}

// taskProgress 在终端里画进度条，在 --json 模式下发进度事件给调用方。
type taskProgress struct {
	a       *App
	t       *sendTask
	total   int64
	bar     *progress
	lastAt  time.Time
	lastPos int64
}

func (a *App) newTaskProgress(t *sendTask, total int64) *taskProgress {
	p := &taskProgress{a: a, t: t, total: total}
	if !a.JSON {
		p.bar = newProgress(a.Err, "发送 "+t.name, total, a.showProgress())
	}
	return p
}

func (p *taskProgress) update(sent int64) {
	if p.bar != nil {
		p.bar.update(sent)
		return
	}
	now := time.Now()
	if !p.lastAt.IsZero() && now.Sub(p.lastAt) < 300*time.Millisecond {
		return
	}
	dt := now.Sub(p.lastAt).Seconds()
	speed := int64(0)
	if dt > 0 {
		speed = int64(float64(sent-p.lastPos) / dt)
	}
	p.lastAt, p.lastPos = now, sent
	p.a.emit(map[string]any{
		"event": "progress", "file": p.t.name, "peer": p.t.peer.Name,
		"received": sent, "total": p.total, "speed": speed,
	})
}

func (p *taskProgress) finish() {
	if p.bar != nil {
		p.bar.finish()
	}
}

func (a *App) reportTask(ctx context.Context, base string, t *sendTask, sendErr error, single bool) {
	if recErr := client.AddHistory(ctx, base, core.TransferRecord{
		Ts: time.Now().Unix(), Direction: "send", PeerName: t.peer.Name,
		Name: t.name, Kind: map[bool]string{true: "folder", false: "file"}[t.isDir],
		Size: t.size, Status: map[bool]string{true: "done", false: "failed"}[sendErr == nil],
	}); recErr != nil {
		a.warnf("记录发送日志失败: %v", recErr)
	}
	if sendErr != nil {
		code := classify(sendErr)
		a.warnf("%s: %v", t.name, sendErr)
		if a.JSON {
			a.emit(map[string]any{"ok": false, "file": t.name, "peer": t.peer.Name,
				"code": code, "msg": sendErr.Error()})
		}
		return
	}
	if a.JSON {
		a.emit(map[string]any{"ok": true, "file": t.name, "peer": t.peer.Name,
			"id": t.peer.ID, "size": t.size})
		return
	}
	a.outMu.Lock()
	defer a.outMu.Unlock()
	if single {
		fmt.Fprintf(a.Out, "已发送给 %s\n", t.peer.Name)
	} else {
		fmt.Fprintf(a.Out, "已发送 %s → %s\n", t.name, t.peer.Name)
	}
}

func (a *App) sendOne(ctx context.Context, peer core.Peer, src string, size int64) error {
	t := &sendTask{src: src, name: filepath.Base(src), size: size, peer: peer}
	if err := a.offerTask(ctx, t); err != nil {
		return err
	}
	if t.done {
		return nil
	}
	return a.runTask(ctx, t, make(chan struct{}, 3))
}

func (a *App) pickPeer(peers []core.Peer) (core.Peer, error) {
	fmt.Fprintln(a.Err, "发给哪台？")
	for i, p := range peers {
		fmt.Fprintf(a.Err, "  %d) %s  %s\n", i+1, p.Name, p.Addr)
	}
	fmt.Fprint(a.Err, "> ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return core.Peer{}, errors.New("没有读到选择")
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(peers) {
		return core.Peer{}, errors.New("选择无效")
	}
	return peers[n-1], nil
}

// ---------- inbox ----------

func (a *App) cmdInbox(args []string) int {
	fs := a.flags("inbox")
	n := fs.Int("n", 20, "条数")
	since := fs.Duration("since", 0, "只看这段时间之后")
	watch := fs.Bool("watch", false, "实时跟随")
	interactiveFlag := fs.Bool("i", false, "监看模式：可以输入编号直接接收")
	fs.BoolVar(interactiveFlag, "interactive", false, "监看模式：可以输入编号直接接收")
	noWait := fs.Bool("no-wait", false, "接收后不等传完")
	all := fs.Bool("all", false, "全部取消")
	olderThan := fs.String("older-than", "30d", "清理多久之前的记录")
	withFiles := fs.Bool("with-files", false, "连文件一起删")
	dryRun := fs.Bool("dry-run", false, "只看看会删多少")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return core.ExitUsage
	}
	if err := a.load(); err != nil {
		return a.fail("CONFIG", core.ExitError, err.Error())
	}

	var sub, subID string
	if len(rest) > 0 {
		sub = rest[0]
	}
	if len(rest) > 1 {
		subID = rest[1]
	}
	if sub == "clean" {
		return a.cmdClean(*olderThan, *withFiles, *dryRun)
	}
	if sub == "cancel" && *all {
		return a.cancelAll()
	}
	if sub == "accept" && *all {
		return a.acceptAll()
	}
	if sub == "accept" || sub == "reject" || sub == "cancel" {
		return a.decide(sub, subID, *noWait)
	}

	base, code := a.requireService()
	if code != 0 {
		return code
	}
	ctx := context.Background()

	if *watch {
		return a.watchInbox(ctx, base, *interactiveFlag)
	}

	var sinceTs int64
	if *since > 0 {
		sinceTs = time.Now().Add(-*since).Unix()
	}
	items, err := client.Inbox(ctx, base, *n, sinceTs)
	if err != nil {
		return a.fail("INBOX", core.ExitError, err.Error())
	}
	if a.JSON {
		a.emit(map[string]any{"ok": true, "items": items})
		return core.ExitOK
	}
	if len(items) == 0 {
		fmt.Fprintln(a.Out, "收件箱是空的。")
		return core.ExitOK
	}
	for i, it := range items {
		fmt.Fprintf(a.Out, "%d) %s\n", i+1, formatItem(it))
	}
	return core.ExitOK
}

func (a *App) watchInbox(ctx context.Context, base string, interactive bool) int {
	ch, err := client.Watch(ctx, base)
	if err != nil {
		return a.fail("WATCH", core.ExitError, err.Error())
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	// 每个传输上一次打印到的百分比和时刻，避免刷屏
	lastPct := map[string]int{}
	lastAt := map[string]time.Time{}

	// 终端里可以边看边按编号接收
	cmds := make(chan string, 8)
	interactive = interactive && !a.JSON && isTTY() && isTerminalWriter(a.Out)
	if interactive {
		fmt.Fprintln(a.Out, "按编号接收，a 接收全部，q 退出。")
		go readCommands(cmds)
	}
	pendingIdx := map[int]string{}
	nextIdx := 1

	for {
		select {
		case line, ok := <-cmds:
			if !ok {
				return core.ExitOK
			}
			cmd := strings.TrimSpace(strings.ToLower(line))
			switch {
			case cmd == "q" || cmd == "quit" || cmd == "exit":
				return core.ExitOK
			case cmd == "":
			case cmd == "a" || cmd == "all":
				if len(pendingIdx) == 0 {
					fmt.Fprintln(a.Out, "  现在没有待确认的传输。")
					continue
				}
				ids := make([]string, 0, len(pendingIdx))
				for _, id := range pendingIdx {
					ids = append(ids, id)
				}
				for _, id := range ids {
					if err := client.Decision(ctx, base, id, "accept"); err != nil {
						fmt.Fprintf(a.Out, "  接收失败：%v\n", err)
						continue
					}
				}
				pendingIdx = map[int]string{}
				fmt.Fprintln(a.Out, "  已接收全部待确认的传输。")
			default:
				n, err := strconv.Atoi(cmd)
				if err != nil {
					fmt.Fprintln(a.Out, "  看不懂，输入编号 / a / q。")
					continue
				}
				id, ok := pendingIdx[n]
				if !ok {
					fmt.Fprintln(a.Out, "  没有这个编号。")
					continue
				}
				if err := client.Decision(ctx, base, id, "accept"); err != nil {
					fmt.Fprintf(a.Out, "  接收失败：%v\n", err)
					continue
				}
				delete(pendingIdx, n)
				fmt.Fprintf(a.Out, "  已接收 #%d。\n", n)
			}
		case ev, ok := <-ch:
			if !ok {
				return core.ExitOK
			}
			if a.JSON {
				b, _ := json.Marshal(ev)
				fmt.Fprintln(a.Out, string(b))
				continue
			}
			switch ev.Type {
			case core.EvTransfer:
				if p, ok := decodeProgress(ev.Data); ok {
					pct := 0
					if p.Total > 0 {
						pct = int(p.Received * 100 / p.Total)
					}
					// 每 10% 或每 2 秒打一行，别把屏幕刷爆
					if pct/10 != lastPct[p.ID]/10 || time.Since(lastAt[p.ID]) >= 2*time.Second || pct >= 100 {
						lastPct[p.ID] = pct
						lastAt[p.ID] = time.Now()
						fmt.Fprintf(a.Out, "[%s] 接收中  %3d%%  %s / %s\n",
							time.Unix(ev.Ts, 0).Format("15:04:05"),
							pct, humanBytes(p.Received), humanBytes(p.Total))
					}
					continue
				}
			case core.EvItemNew:
				if it, ok := decodeItem(ev.Data); ok {
					if interactive && it.Status == core.StatusPending {
						pendingIdx[nextIdx] = it.ID
						fmt.Fprintf(a.Out, "[#%d] %s\n", nextIdx, formatItem(it))
						nextIdx++
					} else {
						fmt.Fprintln(a.Out, formatItem(it))
					}
					continue
				}
			case core.EvItemUpdate:
				if u, ok := decodeItemUpdate(ev.Data); ok {
					for k, id := range pendingIdx {
						if id == u.ID && (u.Status == "active" || u.Status == "done" || u.Status == "failed") {
							delete(pendingIdx, k)
						}
					}
					label := map[string]string{
						"done": "完成", "failed": "失败", "active": "已接受",
					}[u.Status]
					if label == "" {
						label = u.Status
					}
					if u.Name != "" {
						fmt.Fprintf(a.Out, "[%s] %s  %s (%s)\n",
							time.Unix(ev.Ts, 0).Format("15:04:05"), label, u.Name, humanBytes(u.Size))
					} else {
						fmt.Fprintf(a.Out, "[%s] %s  %s\n",
							time.Unix(ev.Ts, 0).Format("15:04:05"), label, u.ID)
					}
					continue
				}
			case core.EvPeerOnline:
				if p, ok := decodePeer(ev.Data); ok {
					fmt.Fprintf(a.Out, "%s 上线了  %s\n", p.Name, p.Addr)
					continue
				}
			}
			fmt.Fprintf(a.Out, "[%s] %s\n", time.Unix(ev.Ts, 0).Format("15:04:05"), ev.Type)
		case <-ctx.Done():
			return core.ExitOK
		}
	}
}

func (a *App) decide(action, arg string, noWait bool) int {
	base, code := a.requireService()
	if code != 0 {
		return code
	}
	ctx := context.Background()

	items, err := client.Inbox(ctx, base, inboxListLimit, 0)
	if err != nil {
		return a.fail("INBOX", core.ExitError, err.Error())
	}
	allow := []core.Status{core.StatusPending}
	if action == "cancel" {
		allow = []core.Status{core.StatusPending, core.StatusActive}
	}
	target, err := pickTarget(items, arg, allow...)
	if err != nil {
		return a.fail("USAGE", core.ExitUsage, err.Error())
	}

	if err := client.Decision(ctx, base, target.ID, action); err != nil {
		return a.fail("DECISION", core.ExitError, err.Error())
	}
	if a.JSON {
		a.emit(map[string]any{"ok": true, "id": target.ID, "action": action})
	} else {
		verb := map[string]string{"accept": "接收", "reject": "拒收", "cancel": "取消"}[action]
		fmt.Fprintf(a.Out, "已%s %s\n", verb, describeTarget(target))
	}

	if action == "accept" && !noWait && a.showProgress() && target.Kind != core.KindText {
		a.followTransfer(base, target)
	}
	return core.ExitOK
}

// inboxListLimit 是 xlan inbox 默认显示的条数，序号也按这个范围算。
const inboxListLimit = 20

// pickTarget 把"不带参数 / 序号 / id 前缀"统一解析成具体的条目 id。
func pickTarget(items []core.Item, arg string, allow ...core.Status) (core.Item, error) {
	if len(allow) == 0 {
		allow = []core.Status{core.StatusPending}
	}
	usable := func(s core.Status) bool {
		for _, a := range allow {
			if a == s {
				return true
			}
		}
		return false
	}
	arg = strings.TrimSpace(arg)

	if arg == "" {
		var pending []core.Item
		for _, it := range items {
			if usable(it.Status) {
				pending = append(pending, it)
			}
		}
		switch len(pending) {
		case 0:
			return core.Item{}, errors.New("没有待确认的传输")
		case 1:
			return pending[0], nil
		default:
			return core.Item{}, fmt.Errorf("有 %d 条待确认，用序号指定，例如：xlan inbox accept 1", len(pending))
		}
	}

	// 1..20 之间当序号；其余当 id 前缀
	if n, err := strconv.Atoi(arg); err == nil && n >= 1 && n <= len(items) {
		it := items[n-1]
		if !usable(it.Status) {
			return core.Item{}, fmt.Errorf("第 %d 条是 %s，不能这样操作", n, it.Status)
		}
		return it, nil
	}
	for _, it := range items {
		if it.ID == arg || strings.HasPrefix(it.ID, arg) {
			return it, nil
		}
	}
	return core.Item{}, fmt.Errorf("找不到这条记录：%s（先跑 xlan inbox 看序号）", arg)
}

// describeTarget 给人类看的一句话描述，比如"小王 发来的 报告.pdf"。
func describeTarget(it core.Item) string {
	who := it.FromName
	if who == "" {
		who = "对方"
	}
	if it.Kind == core.KindText {
		return who + " 发来的消息"
	}
	return who + " 发来的 " + it.Name
}

// ---------- recv ----------

func (a *App) cmdRecv(args []string) int {
	fs := a.flags("recv")
	out := fs.String("out", "", "输出路径，- 表示 stdout")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return core.ExitUsage
	}
	id := ""
	if len(rest) > 0 {
		id = rest[0]
	}
	if id == "" {
		return a.fail("USAGE", core.ExitUsage, "用法：xlan recv <id> [--out 路径]")
	}
	if err := a.load(); err != nil {
		return a.fail("CONFIG", core.ExitError, err.Error())
	}
	base, code := a.requireService()
	if code != 0 {
		return code
	}
	it, err := client.GetItem(context.Background(), base, id)
	if err != nil {
		return a.fail("NOT_FOUND", core.ExitError, err.Error())
	}

	switch {
	case it.Kind == core.KindText:
		if *out == "" || *out == "-" {
			fmt.Fprintln(a.Out, it.Body)
			return core.ExitOK
		}
		if err := os.WriteFile(*out, []byte(it.Body), 0o644); err != nil {
			return a.fail("WRITE", core.ExitError, err.Error())
		}
	case it.Kind == core.KindFolder:
		if it.Path == "" {
			return a.fail("NO_PATH", core.ExitError, "这个文件夹还没有落盘")
		}
		if *out == "" || *out == "-" {
			if a.JSON {
				a.emit(map[string]any{"ok": true, "kind": "folder", "path": it.Path})
			} else {
				fmt.Fprintf(a.Out, "文件夹已经在 %s\n", it.Path)
			}
			return core.ExitOK
		}
		if err := copyTree(it.Path, *out); err != nil {
			return a.fail("COPY", core.ExitError, err.Error())
		}
		if !a.JSON {
			fmt.Fprintf(a.Out, "已复制到 %s\n", *out)
		}
		return core.ExitOK
	case it.Path == "":
		return a.fail("NO_PATH", core.ExitError, "这条记录还没有落盘")
	default:
		dst := *out
		if dst == "" || dst == "-" {
			if err := copyFile(it.Path, a.Out); err != nil {
				return a.fail("READ", core.ExitError, err.Error())
			}
			return core.ExitOK
		}
		if err := copyFileTo(it.Path, dst); err != nil {
			return a.fail("WRITE", core.ExitError, err.Error())
		}
	}
	if !a.JSON {
		fmt.Fprintf(a.Out, "已写出 %s\n", *out)
	}
	return core.ExitOK
}

// ---------- trust ----------

func (a *App) cmdTrust(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Out, "用法：xlan trust add|rm <设备> | list")
		return core.ExitUsage
	}
	if err := a.load(); err != nil {
		return a.fail("CONFIG", core.ExitError, err.Error())
	}
	base, code := a.requireService()
	if code != 0 {
		return code
	}
	ctx := context.Background()

	if args[0] == "list" {
		peers, err := client.Peers(ctx, base)
		if err != nil {
			return a.fail("PEERS", core.ExitError, err.Error())
		}
		if a.JSON {
			a.emit(map[string]any{"ok": true, "peers": peers})
			return core.ExitOK
		}
		for _, p := range peers {
			mark := "-"
			if p.Trusted {
				mark = "*"
			}
			fmt.Fprintf(a.Out, "%s %-18s %s\n", mark, p.Name, p.ID)
		}
		return core.ExitOK
	}
	if len(args) < 2 {
		return a.fail("USAGE", core.ExitUsage, "用法：xlan trust add|rm <设备>")
	}
	peers, err := client.Peers(ctx, base)
	if err != nil {
		return a.fail("PEERS", core.ExitError, err.Error())
	}
	p, err := resolvePeer(peers, args[1])
	if err != nil {
		return a.fail("NOT_FOUND", core.ExitNoPeer, err.Error())
	}
	trusted := args[0] == "add"
	if err := client.SetTrusted(ctx, base, p.ID, trusted); err != nil {
		return a.fail("TRUST", core.ExitError, err.Error())
	}
	if a.JSON {
		a.emit(map[string]any{"ok": true, "id": p.ID, "trusted": trusted})
	} else if trusted {
		fmt.Fprintf(a.Out, "已信任 %s\n", p.Name)
	} else {
		fmt.Fprintf(a.Out, "已取消信任 %s\n", p.Name)
	}
	return core.ExitOK
}

// ---------- doctor ----------

func (a *App) cmdDoctor(args []string) int {
	fs := a.flags("doctor")
	if err := fs.Parse(args); err != nil {
		return core.ExitUsage
	}
	if err := a.load(); err != nil {
		return a.fail("CONFIG", core.ExitError, err.Error())
	}
	ctx := context.Background()

	checks := []doctorCheck{}
	lan := lanInterfaces()

	// ---- 网络 ----
	private := 0
	for _, li := range lan {
		if li.private {
			private++
		}
	}
	if len(lan) == 0 {
		checks = append(checks, doctorCheck{Name: "局域网", OK: false, Detail: "没有可用的网络接口（只找到 loopback）"})
	} else if private == 0 {
		checks = append(checks, doctorCheck{Name: "局域网", OK: true, Detail: "有网络，但没拿到私有地址，可能不在同一个局域网", Warn: true})
	} else {
		addrs := make([]string, 0, len(lan))
		for _, li := range lan {
			addrs = append(addrs, li.name+" "+li.ip+"/"+li.mask)
		}
		checks = append(checks, doctorCheck{Name: "局域网", OK: true, Detail: strings.Join(addrs, "，")})
	}
	if ips, err := discover.BroadcastAddrs(); err == nil && len(ips) > 0 {
		ss := make([]string, 0, len(ips))
		for _, ip := range ips {
			ss = append(ss, ip.String())
		}
		checks = append(checks, doctorCheck{Name: "广播地址", OK: true, Detail: strings.Join(ss, " ")})
	}

	// ---- 目录可写 ----
	checks = append(checks, writableCheck("收件目录", a.Cfg.RecvDirPath()))
	checks = append(checks, writableCheck("数据目录", exeDirOrEmpty()))

	// ---- 服务 ----
	base, probeErr := spawn.FindErr(ctx, a.Cfg)
	var info client.Info
	if base == "" {
		detail := "没有在运行"
		if probeErr != nil {
			detail = probeErr.Error()
		}
		checks = append(checks, doctorCheck{Name: "服务", OK: false, Detail: detail})
	} else {
		info, _ = client.GetInfo(ctx, base)
		checks = append(checks, doctorCheck{Name: "服务", OK: true, Detail: fmt.Sprintf("%s（已运行 %s）", base, (time.Duration(info.UptimeSec) * time.Second).Round(time.Second))})
		if info.TLS {
			fp := info.Fingerprint
			if len(fp) > 16 {
				fp = fp[:16]
			}
			checks = append(checks, doctorCheck{Name: "加密", OK: true, Detail: "TLS，指纹 " + fp})
		} else {
			checks = append(checks, doctorCheck{Name: "加密", OK: true, Detail: "明文（未加密）", Warn: true})
		}
		// 服务端保存的是发现过的全部设备，只有最近 12 秒还在广播的才算在线。
		peers, _ := client.Peers(ctx, base)
		now := time.Now().Unix()
		online := 0
		for _, p := range peers {
			if now-p.LastSeen <= 12 {
				online++
			}
		}
		checks = append(checks, doctorCheck{Name: "在线设备", OK: true, Detail: fmt.Sprintf("%d 台", online)})
	}

	// ---- 自测：发一个文件给自己 ----
	selftest := false
	if base != "" {
		st, ok := a.doctorSelftest(ctx, base)
		checks = append(checks, st...)
		selftest = ok
	}

	failed := 0
	for _, c := range checks {
		if !c.OK {
			failed++
		}
	}

	if a.JSON {
		a.emit(map[string]any{
			"ok": failed == 0, "checks": checks, "selftest": selftest,
			"running": base != "", "base": base,
		})
		if failed > 0 {
			return core.ExitError
		}
		return core.ExitOK
	}

	fmt.Fprintln(a.Out, "网络")
	for _, c := range checks[:2] {
		fmt.Fprintf(a.Out, "  %s %-10s %s\n", mark(c), c.Name, c.Detail)
	}
	fmt.Fprintln(a.Out, "\n目录")
	for _, c := range checks[2:4] {
		fmt.Fprintf(a.Out, "  %s %-10s %s\n", mark(c), c.Name, c.Detail)
	}
	fmt.Fprintln(a.Out, "\n服务")
	rest := checks[4:]
	for _, c := range rest {
		fmt.Fprintf(a.Out, "  %s %-10s %s\n", mark(c), c.Name, c.Detail)
	}

	fmt.Fprintln(a.Out)
	if failed == 0 {
		fmt.Fprintln(a.Out, "全部通过。")
	} else {
		fmt.Fprintf(a.Out, "有 %d 项没通过，看上面的原因。\n", failed)
		return core.ExitError
	}
	return core.ExitOK
}

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Warn   bool   `json:"warn,omitempty"`
}

func mark(c doctorCheck) string {
	switch {
	case !c.OK:
		return "✗"
	case c.Warn:
		return "!"
	default:
		return "✓"
	}
}

func exeDirOrEmpty() string {
	d, err := config.DataDir()
	if err != nil {
		return ""
	}
	return d
}

func writableCheck(name, dir string) doctorCheck {
	if dir == "" {
		return doctorCheck{Name: name, OK: false, Detail: "目录未知"}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return doctorCheck{Name: name, OK: false, Detail: dir + " 建不出来：" + err.Error()}
	}
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return doctorCheck{Name: name, OK: false, Detail: dir + " 不可写：" + err.Error()}
	}
	p := f.Name()
	f.Close()
	os.Remove(p)
	return doctorCheck{Name: name, OK: true, Detail: dir}
}

// lanInterfaces 列出可用的 IPv4 网卡地址。
type lanIface struct {
	name    string
	ip      string
	mask    string
	private bool
}

func lanInterfaces() []lanIface {
	var out []lanIface
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
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
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			ones, _ := ipnet.Mask.Size()
			out = append(out, lanIface{
				name: ifc.Name, ip: ip4.String(),
				mask: strconv.Itoa(ones), private: ip4.IsPrivate(),
			})
		}
	}
	return out
}

// doctorSelftest 真的走一遍「发文件」的完整链路，然后清理干净。
func (a *App) doctorSelftest(ctx context.Context, base string) ([]doctorCheck, bool) {
	info, err := client.GetInfo(ctx, base)
	if err != nil {
		return []doctorCheck{{Name: "自测", OK: false, Detail: "拿不到服务信息：" + err.Error()}}, false
	}

	payload := make([]byte, 64*1024)
	_, _ = rand.Read(payload)
	sum := sha256.Sum256(payload)
	hexSum := hex.EncodeToString(sum[:])

	name := fmt.Sprintf("xlan-自测-%d.bin", time.Now().Unix())

	resp, err := client.Offer(ctx, base, client.OfferReq{
		FromID: info.ID, FromName: info.Name, ToID: info.ID,
		Kind: "file", Name: name, Size: int64(len(payload)), SHA256: hexSum,
	})
	if err != nil {
		return []doctorCheck{{Name: "自测", OK: false, Detail: "发起传输失败：" + err.Error()}}, false
	}

	// 不是自动接收模式时，自己给自己点个接受
	if !resp.Accepted {
		if err := client.Decision(ctx, base, resp.ID, "accept"); err != nil {
			return []doctorCheck{{Name: "自测", OK: false, Detail: "接受传输失败：" + err.Error()}}, false
		}
	}

	reader := bytes.NewReader(payload)
	if err := client.PutRange(ctx, base, resp.ID, reader, 0, int64(len(payload)), int64(len(payload))); err != nil {
		_ = client.RemoveItem(ctx, base, resp.ID, true)
		return []doctorCheck{{Name: "自测", OK: false, Detail: "上传失败：" + err.Error()}}, false
	}

	st, err := client.GetOffer(ctx, base, resp.ID)
	if err != nil || st.Status != string(core.StatusDone) {
		_ = client.RemoveItem(ctx, base, resp.ID, true)
		return []doctorCheck{{Name: "自测", OK: false, Detail: "上传后状态不是 done，校验可能没通过"}}, false
	}

	// 清理：记录和落盘的文件都删掉
	_ = client.RemoveItem(ctx, base, resp.ID, true)

	return []doctorCheck{
		{Name: "自测", OK: true, Detail: "写了一块 64 KB 数据，走完整链路传完并校验通过，已清理"},
	}, true
}

// ---------- helpers ----------

func (a *App) self() core.Device {
	if a.Dev.ID != "" {
		return a.Dev
	}
	dataDir, _ := config.DataDir()
	d, _ := core.LoadDevice(dataDir, a.Cfg.Name)
	a.Dev = d
	return d
}

func isTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func bufferStdin() (string, error) {
	dataDir, err := config.DataDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(dataDir, "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "stdin-*")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, os.Stdin); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func resolvePeer(peers []core.Peer, target string) (core.Peer, error) {
	// 1. 完整 id
	for _, p := range peers {
		if p.ID == target {
			return p, nil
		}
	}
	// 2. id 前缀（至少 4 位）
	if len(target) >= 4 {
		for _, p := range peers {
			if strings.HasPrefix(p.ID, target) {
				return p, nil
			}
		}
	}
	// 3. IP 地址
	if ip := net.ParseIP(target); ip != nil {
		for _, p := range peers {
			if p.Addr == target || (net.ParseIP(p.Addr) != nil && net.ParseIP(p.Addr).Equal(ip)) {
				return p, nil
			}
		}
	}
	// 4. 完整名字，重名时取第一个
	t := strings.ToLower(target)
	for _, p := range peers {
		if strings.ToLower(p.Name) == t {
			return p, nil
		}
	}
	// 5. 名字前缀
	for _, p := range peers {
		if strings.HasPrefix(strings.ToLower(p.Name), t) {
			return p, nil
		}
	}
	return core.Peer{}, fmt.Errorf("找不到设备 %q", target)
}

// ambiguousPeer 在重名或前缀撞车时，把候选和它们的 id 一起列出来。
func ambiguousPeer(target string, m []core.Peer) error {
	parts := make([]string, 0, len(m))
	for _, p := range m {
		parts = append(parts, fmt.Sprintf("%s(%s)", p.Name, shortID(p.ID)))
	}
	return fmt.Errorf("%q 匹配到 %d 台设备，请用 id 指定：%s", target, len(m), strings.Join(parts, "，"))
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
func peerIDs(ps []core.Peer) string {
	ids := make([]string, 0, len(ps))
	for _, p := range ps {
		ids = append(ids, p.ID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}

func formatItem(it core.Item) string {
	ts := time.Unix(it.CreatedAt, 0).Format("2006-01-02 15:04")
	who := it.FromName
	if who == "" {
		who = it.FromID
	}
	if it.Kind == core.KindText {
		tag := "私聊"
		if it.Channel == core.ChanPublic {
			tag = "公共"
		}
		return fmt.Sprintf("%s [%s] %s: %s", ts, tag, who, it.Body)
	}

	tag := "文件"
	if it.Kind == core.KindFolder {
		tag = "文件夹"
	}
	return fmt.Sprintf("%s [%s] %s: %s (%s) %s", ts, tag, who, it.Name, humanBytes(it.Size), it.Status)
}

func classify(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "拒绝"):
		return "DENIED"
	case strings.Contains(msg, "CHECKSUM"), strings.Contains(msg, "checksum"):
		return "CHECKSUM"
	case strings.Contains(msg, "超时"), strings.Contains(msg, "确认"):
		return "TIMEOUT"
	default:
		return "ERROR"
	}
}

func sha256Reader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return sha256Reader(f)
}

func copyFile(src string, w io.Writer) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// waitAccepted 等对方点确认，超时就放弃。
func (a *App) waitAccepted(ctx context.Context, base, id string) error {
	deadline := time.Now().Add(a.Timeout)
	for time.Now().Before(deadline) {
		time.Sleep(400 * time.Millisecond)
		s, err := client.GetOffer(ctx, base, id)
		if err != nil {
			return err
		}
		switch s.Status {
		case string(core.StatusActive), string(core.StatusDone):
			return nil
		case string(core.StatusFailed), string(core.StatusExpired):
			return errors.New("对方拒绝了")
		}
	}
	return errors.New("对方没有在超时前确认")
}

// sendFolder 先把目录结构作为 manifest 发过去，再逐个文件上传。
func (a *App) sendFolder(ctx context.Context, peer core.Peer, root string) error {
	t := &sendTask{src: root, name: filepath.Base(root), isDir: true, peer: peer}
	if err := a.offerTask(ctx, t); err != nil {
		return err
	}
	return a.runTask(ctx, t, make(chan struct{}, 3))
}

// uploadFolder 按 manifest 逐个上传目录里的文件。
func (a *App) uploadFolder(ctx context.Context, t *sendTask) error {
	m := t.manifest
	if m == nil {
		return errors.New("目录清单没有准备好")
	}
	prog := a.newTaskProgress(t, m.TotalSize())
	defer prog.finish()

	skip := t.doneFiles
	if len(skip) > 0 {
		a.warnf("续传：跳过 %d 个已经传完的文件", len(skip))
	}

	var sent int64
	for _, e := range m.Entries {
		if e.Dir {
			continue
		}
		if skip[e.Rel] {
			sent += e.Size
			prog.update(sent)
			continue
		}
		src := filepath.Join(t.src, filepath.FromSlash(e.Rel))
		base0 := sent
		if err := a.uploadFile(ctx, t.base, t.id, e.Rel, src, e.Size, func(n int64) {
			prog.update(base0 + n)
		}); err != nil {
			return err
		}
		sent += e.Size
		prog.update(sent)
	}
	return client.FinishFolder(ctx, t.base, t.id)
}

// uploadOne 传一个文件。threads>1 时把文件切成块并发上传。
func (a *App) uploadOne(ctx context.Context, size int64, threads int,
	onChunk func(int64), put func(ctx context.Context, off, n int64) error) error {

	threads = config.ClampThreads(threads)
	if threads <= 1 {
		var off int64
		for off < size {
			if a.pause != nil {
				a.pause.Wait()
			}
			n := int64(chunkSize)
			if size-off < n {
				n = size - off
			}
			if err := put(ctx, off, n); err != nil {
				return err
			}
			off += n
			if onChunk != nil {
				onChunk(off)
			}
		}
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		sent     int64
		firstErr error
	)
	sem := make(chan struct{}, threads)

	for off := int64(0); off < size; off += chunkSize {
		n := int64(chunkSize)
		if size-off < n {
			n = size - off
		}
		wg.Add(1)
		go func(off, n int64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			if a.pause != nil {
				a.pause.Wait()
			}
			if err := put(ctx, off, n); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				cancel()
				return
			}
			mu.Lock()
			sent += n
			if onChunk != nil {
				onChunk(sent)
			}
			mu.Unlock()
		}(off, n)
	}
	wg.Wait()
	return firstErr
}

func (a *App) uploadFile(ctx context.Context, base, id, rel, src string, size int64, onChunk func(sent int64)) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	return a.uploadOne(ctx, size, a.Cfg.Threads, onChunk, func(ctx context.Context, off, n int64) error {
		return a.putWithRetry(func() error {
			return client.PutFileRange(ctx, base, id, rel, a.rate(io.NewSectionReader(f, off, n)), off, n, size)
		})
	})
}

// buildManifest 遍历目录，跳过符号链接，给每个文件算好大小和校验和。
func buildManifest(root string) (core.Manifest, error) {
	clean := filepath.Clean(root)
	m := core.Manifest{Root: filepath.Base(clean)}
	err := filepath.WalkDir(clean, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == clean {
			return nil
		}
		rel, err := filepath.Rel(clean, p)
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			m.Entries = append(m.Entries, core.FolderEntry{Rel: filepath.ToSlash(rel), Dir: true})
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sum, err := fileSHA256(p)
		if err != nil {
			return err
		}
		m.Entries = append(m.Entries, core.FolderEntry{
			Rel: filepath.ToSlash(rel), Size: info.Size(), SHA256: sum,
		})
		return nil
	})
	return m, err
}

func copyFileTo(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func decodeItem(v any) (core.Item, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return core.Item{}, false
	}
	var it core.Item
	if err := json.Unmarshal(b, &it); err != nil || it.ID == "" {
		return core.Item{}, false
	}
	return it, true
}

func decodePeer(v any) (core.Peer, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return core.Peer{}, false
	}
	var p core.Peer
	if err := json.Unmarshal(b, &p); err != nil || p.ID == "" {
		return core.Peer{}, false
	}
	return p, true
}

// ---------- config ----------

func (a *App) cmdConfig(args []string) int {
	fs := a.flags("config")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return core.ExitUsage
	}
	if err := a.load(); err != nil {
		return a.fail("CONFIG", core.ExitError, err.Error())
	}

	if len(rest) == 0 || rest[0] == "list" {
		a.printConfig()
		return core.ExitOK
	}

	switch rest[0] {
	case "get":
		if len(rest) < 2 {
			return a.fail("USAGE", core.ExitUsage, "用法：xlan config get <键>")
		}
		v, ok := a.configValue(rest[1])
		if !ok {
			return a.fail("NO_KEY", core.ExitUsage, "没有这个配置项："+rest[1])
		}
		if a.JSON {
			a.emit(map[string]any{"ok": true, "key": rest[1], "value": v})
		} else {
			fmt.Fprintln(a.Out, v)
		}
		return core.ExitOK

	case "set", "unset":
		if len(rest) < 2 {
			return a.fail("USAGE", core.ExitUsage, "用法：xlan config "+rest[0]+" <键> [值]")
		}
		val := ""
		if rest[0] == "set" {
			if len(rest) < 3 {
				return a.fail("USAGE", core.ExitUsage, "用法：xlan config set <键> <值>")
			}
			val = rest[2]
		}
		if err := a.applyConfig(rest[1], val); err != nil {
			return a.fail("BAD_KEY", core.ExitUsage, err.Error())
		}
		if err := a.Cfg.Save(a.CfgDir); err != nil {
			return a.fail("SAVE", core.ExitError, err.Error())
		}
		// threads 只影响本机发送，服务不用重启；其余两项服务启动时读一次。
		outcome, inflight := restartNotNeeded, int64(0)
		if rest[1] != "threads" {
			outcome, inflight = a.applyByRestart()
		}
		if a.JSON {
			a.emit(map[string]any{
				"ok": true, "key": rest[1], "value": val,
				"restarted": outcome == restartNow,
				"deferred":  outcome == restartDeferred,
				"inflight":  inflight,
			})
			return core.ExitOK
		}
		fmt.Fprintf(a.Out, "%s = %s\n", rest[1], a.mustValue(rest[1]))
		switch outcome {
		case restartNow:
			fmt.Fprintln(a.Out, "服务已重启，立即生效。")
		case restartDeferred:
			fmt.Fprintf(a.Out, "已保存。还有 %d 个传输在进行，等它们跑完自动重启。\n", inflight)
		case restartNotNeeded:
			fmt.Fprintln(a.Out, "已保存，立即生效。")
		case restartNotRunning:
			fmt.Fprintln(a.Out, "已保存，服务没在跑，下次启动时生效。")
		default:
			fmt.Fprintln(a.Out, "已保存，但服务没能重启，手动跑一次 xlan serve 才能生效。")
		}
		return core.ExitOK
	}

	return a.fail("USAGE", core.ExitUsage, "用法：xlan config [list|get|set|unset]")
}

func (a *App) printConfig() {
	dir, _ := config.Dir()
	if a.JSON {
		a.emit(map[string]any{
			"ok": true, "config": filepath.Join(dir, "config.json"),
			"recv_dir": a.Cfg.RecvDirPath(), "name": a.mustValue("name"),
			"api_ports": a.Cfg.APIPorts, "threads": config.ClampThreads(a.Cfg.Threads),
		})
		return
	}
	fmt.Fprintf(a.Out, "recv_dir  %s\n", a.Cfg.RecvDirPath())
	fmt.Fprintf(a.Out, "name      %s\n", a.mustValue("name"))
	fmt.Fprintf(a.Out, "api_ports %v\n", a.Cfg.APIPorts)
	fmt.Fprintf(a.Out, "threads   %d\n", config.ClampThreads(a.Cfg.Threads))
	fmt.Fprintf(a.Out, "tls       %s\n", map[bool]string{true: "on", false: "off"}[a.Cfg.UseTLS()])
	fmt.Fprintf(a.Out, "配置      %s\n", filepath.Join(dir, "config.json"))
}

func (a *App) configValue(key string) (string, bool) {
	switch key {
	case "recv_dir":
		return a.Cfg.RecvDirPath(), true
	case "name":
		if a.Cfg.Name != "" {
			return a.Cfg.Name, true
		}
		return a.self().Name, true
	case "threads":
		return fmt.Sprintf("%d", config.ClampThreads(a.Cfg.Threads)), true
	case "tls":
		if a.Cfg.UseTLS() {
			return "on", true
		}
		return "off", true
	}
	return "", false
}

func (a *App) mustValue(key string) string {
	v, _ := a.configValue(key)
	return v
}

func (a *App) applyConfig(key, val string) error {
	switch key {
	case "recv_dir":
		a.Cfg.RecvDir = val
	case "name":
		a.Cfg.Name = val
	case "threads":
		n, err := strconv.Atoi(val)
		if err != nil {
			return errors.New("threads 要填数字，比如 4")
		}
		a.Cfg.Threads = config.ClampThreads(n)
	case "tls":
		v := true
		switch strings.ToLower(val) {
		case "on", "true", "1", "yes":
			v = true
		case "off", "false", "0", "no":
			v = false
		default:
			return errors.New("tls 只能是 on 或 off")
		}
		a.Cfg.TLS = &v
	default:
		return errors.New("可改的项：recv_dir、name、threads、tls")
	}
	return nil
}

type restartOutcome int

const (
	restartFailed restartOutcome = iota
	restartNow
	restartDeferred
	restartNotRunning
	restartNotNeeded
)

// applyByRestart 让运行中的服务重启：空闲就立刻重启，有传输在跑就等它们跑完。
func (a *App) applyByRestart() (restartOutcome, int64) {
	ctx := context.Background()
	base := spawn.Find(ctx, a.Cfg)
	if base == "" {
		return restartNotRunning, 0
	}
	deferred, inflight, err := client.Restart(ctx, base)
	if err != nil {
		return restartFailed, 0
	}
	if deferred {
		return restartDeferred, inflight
	}
	if a.waitFresh(ctx) {
		return restartNow, 0
	}
	return restartFailed, 0
}

// waitFresh 等新实例接手，uptime 归零才算真的重启了。
func (a *App) waitFresh(ctx context.Context) bool {
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if b := spawn.Find(ctx, a.Cfg); b != "" {
			if info, err := client.GetInfo(ctx, b); err == nil && info.UptimeSec <= 5 {
				return true
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

// ---------- autostart / path ----------

func (a *App) cmdAutostart(args []string) int {
	return a.installCmd("autostart", args,
		install.AutostartInstall, install.AutostartRemove, install.AutostartStatus)
}

func (a *App) cmdPath(args []string) int {
	exe, err := os.Executable()
	if err != nil {
		return a.fail("EXE", core.ExitError, err.Error())
	}
	dir := filepath.Dir(exe)
	on := func(string, bool) (string, error) { return install.PathInstall(dir) }
	off := func(string) (string, error) { return install.PathRemove(dir) }
	status := func() (bool, string, error) { return install.PathStatus(dir) }
	return a.installCmd("path", args, on, off, status)
}

// installCmd 把 on/off/status 三个动作的公共流程收在一处。
func (a *App) installCmd(name string, args []string,
	on func(string, bool) (string, error),
	off func(string) (string, error),
	status func() (bool, string, error)) int {

	fs := a.flags(name)
	autoAccept := fs.Bool("auto-accept", false, "开机自启时也自动接收")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return core.ExitUsage
	}
	action := "status"
	if len(rest) > 0 {
		action = rest[0]
	}
	if err := a.load(); err != nil {
		return a.fail("CONFIG", core.ExitError, err.Error())
	}

	exe, err := os.Executable()
	if err != nil {
		return a.fail("EXE", core.ExitError, err.Error())
	}

	switch action {
	case "on":
		msg, err := on(exe, *autoAccept)
		if err != nil {
			return a.fail("INSTALL", core.ExitError, err.Error())
		}
		if a.JSON {
			a.emit(map[string]any{"ok": true, "action": "on", "msg": msg})
		} else {
			fmt.Fprintln(a.Out, msg)
		}
		return core.ExitOK
	case "off":
		msg, err := off(exe)
		if err != nil {
			return a.fail("REMOVE", core.ExitError, err.Error())
		}
		if a.JSON {
			a.emit(map[string]any{"ok": true, "action": "off", "msg": msg})
		} else {
			fmt.Fprintln(a.Out, msg)
		}
		return core.ExitOK
	case "status":
		installed, detail, err := status()
		if err != nil {
			return a.fail("STATUS", core.ExitError, err.Error())
		}
		if a.JSON {
			a.emit(map[string]any{"ok": true, "installed": installed, "detail": detail})
		} else if installed {
			fmt.Fprintln(a.Out, "已设置："+detail)
		} else {
			fmt.Fprintln(a.Out, "未设置")
		}
		return core.ExitOK
	}
	return a.fail("USAGE", core.ExitUsage, "用法：xlan "+name+" on|off|status")
}

// copyTree 把整个目录复制到 dst（dst 不存在就建）。
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFileTo(p, target)
	})
}

// rate 按 --limit 包一层限速。
func (a *App) rate(r io.Reader) io.Reader {
	if a.Limit <= 0 {
		return r
	}
	if a.limiter == nil {
		a.limiter = newBucket(a.Limit)
	}
	return &rateReader{r: r, b: a.limiter}
}

// putWithRetry 给单个分块加重试，断一下不至于整条重来。
func (a *App) putWithRetry(fn func() error) error {
	const attempts = 3
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		if i < attempts-1 {
			time.Sleep(time.Duration(i+1) * 400 * time.Millisecond)
		}
	}
	return err
}

// cancelAll 把所有还没结束的传输一次性取消。
func (a *App) cancelAll() int {
	return a.batch("cancel", []core.Status{core.StatusPending, core.StatusActive}, "取消")
}

// acceptAll 把所有待确认的传输一次性接收。
func (a *App) acceptAll() int {
	return a.batch("accept", []core.Status{core.StatusPending}, "接收")
}

// batch 对符合条件的条目批量执行同一个决定。
func (a *App) batch(action string, want []core.Status, verb string) int {
	base, code := a.requireService()
	if code != 0 {
		return code
	}
	ctx := context.Background()

	items, err := client.Inbox(ctx, base, 500, 0)
	if err != nil {
		return a.fail("INBOX", core.ExitError, err.Error())
	}

	match := func(s core.Status) bool {
		for _, w := range want {
			if w == s {
				return true
			}
		}
		return false
	}
	var targets []core.Item
	for _, it := range items {
		if match(it.Status) {
			targets = append(targets, it)
		}
	}
	if len(targets) == 0 {
		if a.JSON {
			a.emit(map[string]any{"ok": true, "done": 0})
		} else {
			fmt.Fprintln(a.Out, "没有需要处理的传输。")
		}
		return core.ExitOK
	}

	done, failed := 0, 0
	for _, it := range targets {
		if err := client.Decision(ctx, base, it.ID, action); err != nil {
			failed++
			a.warnf("%s: %v", describeTarget(it), err)
			continue
		}
		done++
		if !a.JSON {
			fmt.Fprintf(a.Out, "已%s %s\n", verb, describeTarget(it))
		}
	}

	if a.JSON {
		a.emit(map[string]any{"ok": failed == 0, "done": done, "failed": failed})
	} else {
		fmt.Fprintf(a.Out, "共%s %d 个", verb, done)
		if failed > 0 {
			fmt.Fprintf(a.Out, "，%d 个失败", failed)
		}
		fmt.Fprintln(a.Out)
	}
	if failed > 0 && done == 0 {
		return core.ExitError
	}
	return core.ExitOK
}

// manifestID 由目录结构算出一个稳定的 id，同一个目录重发时可以接着传。
func manifestID(m core.Manifest) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", m.Root)
	for _, e := range m.Entries {
		fmt.Fprintf(h, "%s|%v|%d|%s\n", e.Rel, e.Dir, e.Size, e.SHA256)
	}
	return "d" + hex.EncodeToString(h.Sum(nil))[:15]
}

// parseAge 解析 "30d" / "12h" / "45m" 这类时长。
func parseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("时长写法不对：%q，例如 30d、12h", s)
		}
		return time.Duration(n * 24 * float64(time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("时长写法不对：%q，例如 30d、12h", s)
	}
	return d, nil
}

// cmdClean 清理收件箱里的旧记录。
func (a *App) cmdClean(olderThan string, withFiles, dryRun bool) int {
	d, err := parseAge(olderThan)
	if err != nil {
		return a.fail("AGE", core.ExitUsage, err.Error())
	}
	base, code := a.requireService()
	if code != 0 {
		return code
	}
	resp, err := client.CleanInbox(context.Background(), base, int64(d.Seconds()), withFiles, dryRun)
	if err != nil {
		return a.fail("CLEAN", core.ExitError, err.Error())
	}

	if a.JSON {
		a.emit(resp)
		return core.ExitOK
	}
	if dryRun {
		fmt.Fprintf(a.Out, "会删除 %d 条 %s 之前的记录", resp.Count, olderThan)
		if withFiles {
			fmt.Fprint(a.Out, "，以及对应文件")
		}
		fmt.Fprintln(a.Out)
		for i, it := range resp.Items {
			if i >= 20 {
				fmt.Fprintf(a.Out, "…还有 %d 条\n", len(resp.Items)-i)
				break
			}
			fmt.Fprintf(a.Out, "  %s %s\n", time.Unix(it.CreatedAt, 0).Format("2006-01-02"), it.Name)
		}
		return core.ExitOK
	}
	fmt.Fprintf(a.Out, "已删除 %d 条记录", resp.Count)
	if withFiles {
		fmt.Fprintf(a.Out, "，%d 个文件", resp.RemovedFiles)
	}
	fmt.Fprintln(a.Out)
	return core.ExitOK
}

// cmdCache 清理缓存：未完成传输的临时分片和旧的已结束收件箱记录。
func (a *App) cmdCache(args []string) int {
	fs := a.flags("cache")
	olderThan := fs.String("older-than", "30d", "清理多久之前的收件箱记录")
	dryRun := fs.Bool("dry-run", false, "只看看会删什么")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return core.ExitUsage
	}
	if len(rest) == 0 || rest[0] != "clean" {
		return a.fail("USAGE", core.ExitUsage, "用法：xlan cache clean [--older-than 30d] [--dry-run]")
	}
	if err := a.load(); err != nil {
		return a.fail("CONFIG", core.ExitError, err.Error())
	}
	d, err := parseAge(*olderThan)
	if err != nil {
		return a.fail("AGE", core.ExitUsage, err.Error())
	}
	if base, _ := spawn.FindErr(context.Background(), a.Cfg); base != "" {
		resp, err := client.CleanCache(context.Background(), base, int64(d.Seconds()), *dryRun)
		if err != nil {
			return a.fail("CACHE", core.ExitError, err.Error())
		}
		return a.printCacheResult(resp.DryRun, resp.Parts, resp.FreedBytes, resp.Records)
	}

	// 服务没起也能清：直接动程序目录里的数据库和收件目录。
	dataDir, err := config.DataDir()
	if err != nil {
		return a.fail("CONFIG", core.ExitError, err.Error())
	}
	st, err := store.Open(filepath.Join(dataDir, "xlan.db"))
	if err != nil {
		return a.fail("CACHE", core.ExitError, err.Error())
	}
	defer st.Close()

	parts, freed, err := cache.CleanParts(a.Cfg.RecvDirPath(), *dryRun, nil, nil)
	if err != nil {
		return a.fail("CACHE", core.ExitError, err.Error())
	}
	records, err := cache.CleanRecords(st, time.Now().Unix()-int64(d.Seconds()), *dryRun, nil)
	if err != nil {
		return a.fail("CACHE", core.ExitError, err.Error())
	}
	return a.printCacheResult(*dryRun, parts, freed, records)
}

func (a *App) printCacheResult(dryRun bool, parts int, freed int64, records int) int {
	if a.JSON {
		a.emit(map[string]any{
			"ok": true, "dry_run": dryRun,
			"parts": parts, "freed_bytes": freed, "records": records,
		})
		return core.ExitOK
	}
	verb := "已删除"
	if dryRun {
		verb = "会删除"
	}
	fmt.Fprintf(a.Out, "%s %d 个临时分片（%s），%d 条收件箱记录\n",
		verb, parts, humanBytes(freed), records)
	if dryRun {
		fmt.Fprintln(a.Out, "（--dry-run，没有改动）")
	}
	return core.ExitOK
}

// cmdLog 查看和清理收发记录。
func (a *App) cmdLog(args []string) int {
	fs := a.flags("log")
	n := fs.Int("n", 20, "条数")
	olderThan := fs.String("older-than", "30d", "清理多久之前的记录")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return core.ExitUsage
	}
	if err := a.load(); err != nil {
		return a.fail("CONFIG", core.ExitError, err.Error())
	}
	base, code := a.requireService()
	if code != 0 {
		return code
	}
	ctx := context.Background()

	if len(rest) > 0 && rest[0] == "clean" {
		d, err := parseAge(*olderThan)
		if err != nil {
			return a.fail("AGE", core.ExitUsage, err.Error())
		}
		cnt, err := client.CleanHistory(ctx, base, int64(d.Seconds()))
		if err != nil {
			return a.fail("CLEAN", core.ExitError, err.Error())
		}
		if a.JSON {
			a.emit(map[string]any{"ok": true, "count": cnt})
		} else {
			fmt.Fprintf(a.Out, "已删除 %d 条记录\n", cnt)
		}
		return core.ExitOK
	}

	items, err := client.History(ctx, base, *n)
	if err != nil {
		return a.fail("LOG", core.ExitError, err.Error())
	}
	if a.JSON {
		a.emit(map[string]any{"ok": true, "items": items})
		return core.ExitOK
	}
	if len(items) == 0 {
		fmt.Fprintln(a.Out, "还没有收发记录。")
		return core.ExitOK
	}
	for _, r := range items {
		dir := "发送"
		if r.Direction == "recv" {
			dir = "接收"
		}
		who := r.PeerName
		if who == "" {
			who = "-"
		}
		line := fmt.Sprintf("%s  %s  %-12s  %s (%s) %s",
			time.Unix(r.Ts, 0).Format("2006-01-02 15:04"), dir, who, r.Name,
			humanBytes(r.Size), r.Status)
		if r.DurationMS > 0 {
			line += "  " + (time.Duration(r.DurationMS) * time.Millisecond).Round(time.Second).String()
		}
		fmt.Fprintln(a.Out, line)
	}
	return core.ExitOK
}

// cmdPS 列出正在跑的发送任务。
func (a *App) cmdSends(args []string) int {
	fs := a.flags("sends")
	if _, err := parseInterspersed(fs, args); err != nil {
		return core.ExitUsage
	}
	list := listSends()
	if a.JSON {
		a.emit(map[string]any{"ok": true, "sends": list})
		return core.ExitOK
	}
	if len(list) == 0 {
		fmt.Fprintln(a.Out, "没有正在进行的发送任务。")
		return core.ExitOK
	}
	fmt.Fprintf(a.Out, "%-10s %-8s %-16s %-10s %s\n", "ID", "PID", "目标", "已用时", "内容")
	for _, e := range list {
		fmt.Fprintf(a.Out, "%-10s %-8d %-16s %-10s %s\n",
			e.ID, e.PID, e.Target, elapsedOf(e), strings.Join(e.Paths, ", "))
	}
	return core.ExitOK
}

// cmdSignal 暂停或继续某个发送任务。
func (a *App) cmdSignal(args []string, pause bool) int {
	fs := a.flags("pause")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return core.ExitUsage
	}
	verb := "暂停"
	if !pause {
		verb = "继续"
	}
	id := ""
	if len(rest) > 0 {
		id = rest[0]
	}
	if id == "" {
		return a.fail("USAGE", core.ExitUsage, "用法：xlan "+map[bool]string{true: "pause", false: "resume"}[pause]+" <任务 id>")
	}
	e, ok := findSend(id)
	if !ok {
		return a.fail("NOT_FOUND", core.ExitNoPeer, "没有这个发送任务："+id+"（用 xlan sends 看）")
	}
	if err := signalSend(e.PID, pause); err != nil {
		return a.fail("SIGNAL", core.ExitError, err.Error())
	}
	if a.JSON {
		a.emit(map[string]any{"ok": true, "id": e.ID, "paused": pause})
	} else {
		fmt.Fprintf(a.Out, "已%s %s\n", verb, e.ID)
	}
	return core.ExitOK
}

// cmdPin 查看或清除证书指纹。
func (a *App) cmdPin(args []string) int {
	fs := a.flags("pin")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return core.ExitUsage
	}
	if err := a.load(); err != nil {
		return a.fail("CONFIG", core.ExitError, err.Error())
	}
	dataDir, err := config.DataDir()
	if err != nil {
		return a.fail("CONFIG", core.ExitError, err.Error())
	}
	pins := secure.LoadPins(filepath.Join(dataDir, "pins.json"))

	action := "list"
	if len(rest) > 0 {
		action = rest[0]
	}
	switch action {
	case "list":
		m := pins.List()
		if a.JSON {
			a.emit(map[string]any{"ok": true, "pins": m})
			return core.ExitOK
		}
		if len(m) == 0 {
			fmt.Fprintln(a.Out, "还没有信任过任何设备。")
			return core.ExitOK
		}
		for host, fp := range m {
			fmt.Fprintf(a.Out, "%-24s %s\n", host, fp[:16])
		}
		return core.ExitOK
	case "forget":
		host := ""
		if len(rest) > 1 {
			host = rest[1]
		}
		if err := pins.Forget(host); err != nil {
			return a.fail("PIN", core.ExitError, err.Error())
		}
		if a.JSON {
			a.emit(map[string]any{"ok": true, "forgot": host})
		} else if host == "" {
			fmt.Fprintln(a.Out, "已清除全部证书指纹。")
		} else {
			fmt.Fprintf(a.Out, "已清除 %s 的指纹。\n", host)
		}
		return core.ExitOK
	}
	return a.fail("USAGE", core.ExitUsage, "用法：xlan pin list|forget [主机]")
}

// readCommands 从终端读命令，按行。
func readCommands(ch chan<- string) {
	defer close(ch)
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		ch <- sc.Text()
	}
}

func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isTerminal(f)
}
