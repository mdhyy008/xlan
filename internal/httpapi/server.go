package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"xlan/core"
	"xlan/internal/client"
	"xlan/internal/discover"
	"xlan/internal/events"
	"xlan/internal/pubtext"
	"xlan/internal/store"
)

// Server 是对外的 HTTP API。所有网络动作都在这里发生。
type Server struct {
	Dev       core.Device
	Port      int
	Store     *store.Store
	Disc      *discover.Discoverer
	Pub       *pubtext.Broadcaster
	Bus       *events.Bus
	RecvDir   func() string
	StartedAt time.Time

	// AutoAccept 为真时，任何人发来的文件都直接收下，不再等确认。
	AutoAccept bool
	// Fingerprint 是本机证书指纹，明文模式下为空。
	Fingerprint string

	// Shutdown 用于让常驻服务优雅退出。
	Shutdown func()
	// RequestRestart 让服务重启（退出后拉起新进程）。
	RequestRestart func()

	pendingRestart int32

	activityMu sync.Mutex
	// activity 记录每个进行中的传输最后一次动的时间，用于判断能否重启。
	activity map[string]int64

	rangeMu sync.Mutex
	ranges  map[string]*spanSet

	progMu sync.Mutex
	// progs 记录每个传输的进度状态，用于给客户端推实时速度。
	progs map[string]*progressState
}

// progressState 是一个传输的字节进度。
type progressState struct {
	total     int64
	committed int64 // 目录传输里已完成文件的总字节
	received  int64 // 当前绝对进度（含正在传的文件）
	lastEmit  time.Time
}

func (s *Server) progLocked(id string) *progressState {
	if s.progs == nil {
		s.progs = make(map[string]*progressState)
	}
	st := s.progs[id]
	if st == nil {
		st = &progressState{}
		s.progs[id] = st
	}
	return st
}

// noteReceived 记录某个传输的绝对进度，节流后推一条事件。
func (s *Server) noteReceived(id string, received, total int64, force bool) {
	s.progMu.Lock()
	st := s.progLocked(id)
	if total > 0 {
		st.total = total
	}
	if received > st.received {
		st.received = received
	}
	now := time.Now()
	if !force && now.Sub(st.lastEmit) < 250*time.Millisecond {
		s.progMu.Unlock()
		return
	}
	st.lastEmit = now
	got, tot := st.received, st.total
	s.progMu.Unlock()

	s.Bus.Publish(core.Event{Type: core.EvTransfer, Ts: now.Unix(), Data: map[string]any{
		"id": id, "received": got, "total": tot,
	}})
}

// committedOf 返回目录传输里已完成文件的累计字节。
func (s *Server) committedOf(id string) int64 {
	s.progMu.Lock()
	defer s.progMu.Unlock()
	return s.progLocked(id).committed
}

// bumpCommitted 在一个文件传完后累加已完成字节。
func (s *Server) bumpCommitted(id string, n int64) {
	s.progMu.Lock()
	defer s.progMu.Unlock()
	st := s.progLocked(id)
	st.committed += n
	if st.committed > st.received {
		st.received = st.committed
	}
}

// loadRanges 从库里恢复续传区间，服务重启后还能接着传。
func (s *Server) loadRanges(id string) {
	s.rangeMu.Lock()
	_, known := s.ranges[id]
	s.rangeMu.Unlock()
	if known {
		return
	}
	raw, err := s.Store.GetRanges(id)
	if err != nil || raw == "" {
		return
	}
	var spans []span
	if json.Unmarshal([]byte(raw), &spans) != nil {
		return
	}
	ss := &spanSet{}
	for _, sp := range spans {
		ss.add(sp.Start, sp.End)
	}
	s.rangeMu.Lock()
	if s.ranges == nil {
		s.ranges = make(map[string]*spanSet)
	}
	if _, ok := s.ranges[id]; !ok {
		s.ranges[id] = ss
	}
	s.rangeMu.Unlock()
}

// markRange 记录收到的一段字节，返回累计覆盖量和是否收齐。
func (s *Server) markRange(id string, start, end, total int64) (int64, bool) {
	s.loadRanges(id)

	s.rangeMu.Lock()
	if s.ranges == nil {
		s.ranges = make(map[string]*spanSet)
	}
	ss := s.ranges[id]
	if ss == nil {
		ss = &spanSet{}
		s.ranges[id] = ss
	}
	cov := ss.add(start, end)
	spans := append([]span(nil), ss.spans...)
	s.rangeMu.Unlock()

	if b, err := json.Marshal(spans); err == nil {
		_ = s.Store.SetRanges(id, string(b))
	}

	complete := total > 0 && cov >= total
	return cov, complete
}

// rangePrefix 返回从 0 开始连续收到的长度。
func (s *Server) rangePrefix(id string) int64 {
	s.loadRanges(id)
	s.rangeMu.Lock()
	defer s.rangeMu.Unlock()
	if ss := s.ranges[id]; ss != nil {
		return ss.prefix()
	}
	return 0
}

func (s *Server) clearRanges(id string) {
	s.rangeMu.Lock()
	delete(s.ranges, id)
	s.rangeMu.Unlock()
	_ = s.Store.SetRanges(id, "")
}

// staleAfter 是判定"传输已经死了"的静默时长。
const staleAfter = 5 * time.Minute

// ReapStale 把长时间没有数据的 active 传输标记为失败，
// 避免对方断线后条目一直挂着。
func (s *Server) ReapStale() int {
	items, err := s.Store.ListActive()
	if err != nil {
		return 0
	}
	now := time.Now().Unix()
	n := 0
	for _, it := range items {
		last := it.CreatedAt
		s.activityMu.Lock()
		if ts, ok := s.activity[it.ID]; ok {
			last = ts / int64(time.Second)
		}
		s.activityMu.Unlock()

		if now-last < int64(staleAfter.Seconds()) {
			continue
		}
		s.clearProgress(it.ID)
		s.clearRanges(it.ID)
		s.clear(it.ID)
		if err := s.Store.UpdateItemStatus(it.ID, core.StatusFailed, it.Path); err != nil {
			continue
		}
		s.Bus.Publish(core.Event{Type: core.EvItemUpdate, Ts: now,
			Data: map[string]any{"id": it.ID, "status": string(core.StatusFailed), "reason": "stale"}})
		n++
	}
	if n > 0 {
		s.maybeRestart()
	}
	return n
}

// clearProgress 在传输结束时清掉状态。
func (s *Server) clearProgress(id string) {
	s.progMu.Lock()
	delete(s.progs, id)
	s.progMu.Unlock()
}

// busyWindow 是判断"还在传"的时间窗口：分块之间有间隔，不能看瞬时状态。
const busyWindow = 5 * time.Minute

func (s *Server) touch(id string) {
	if id == "" {
		return
	}
	s.activityMu.Lock()
	if s.activity == nil {
		s.activity = make(map[string]int64)
	}
	s.activity[id] = time.Now().UnixNano()
	s.activityMu.Unlock()
}

func (s *Server) clear(id string) {
	s.activityMu.Lock()
	delete(s.activity, id)
	s.activityMu.Unlock()
}

// transferCount 返回仍在进行的传输数量，顺手清掉过期记录。
func (s *Server) transferCount() int {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	cut := time.Now().Add(-busyWindow).UnixNano()
	n := 0
	for id, ts := range s.activity {
		if ts >= cut {
			n++
		} else {
			delete(s.activity, id)
		}
	}
	return n
}

// maybeRestart 在最后一个传输结束时，兑现被挂起的重启。
func (s *Server) maybeRestart() {
	if s.transferCount() > 0 {
		return
	}
	if atomic.CompareAndSwapInt32(&s.pendingRestart, 1, 0) && s.RequestRestart != nil {
		s.RequestRestart()
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", s.handleInfo)
	mux.HandleFunc("GET /v1/peers", s.handlePeers)
	mux.HandleFunc("POST /v1/scan", localOnly(s.handleScan))
	mux.HandleFunc("GET /v1/events", s.handleEvents)
	mux.HandleFunc("POST /v1/text", localOnly(s.handleText))
	mux.HandleFunc("GET /v1/inbox", localOnly(s.handleInbox))
	mux.HandleFunc("GET /v1/item/{id}", localOnly(s.handleGetItem))
	mux.HandleFunc("POST /v1/inbox/clean", localOnly(s.handleClean))
	mux.HandleFunc("DELETE /v1/item/{id}", localOnly(s.handleDeleteItem))
	mux.HandleFunc("GET /v1/history", localOnly(s.handleHistory))
	mux.HandleFunc("POST /v1/history", localOnly(s.handleAddHistory))
	mux.HandleFunc("POST /v1/history/clean", localOnly(s.handleCleanHistory))
	mux.HandleFunc("POST /v1/trust", localOnly(s.handleTrust))
	mux.HandleFunc("POST /v1/offer", s.handleOffer)
	mux.HandleFunc("GET /v1/offer/{id}", s.handleGetOffer)
	mux.HandleFunc("POST /v1/offer/{id}/decision", localOnly(s.handleDecision))
	mux.HandleFunc("PUT /v1/offer/{id}/data", s.handlePutData)
	mux.HandleFunc("PUT /v1/offer/{id}/file/{rel...}", s.handlePutFile)
	mux.HandleFunc("POST /v1/offer/{id}/finish", s.handleFinish)
	mux.HandleFunc("POST /v1/shutdown", localOnly(s.handleShutdown))
	mux.HandleFunc("POST /v1/restart", localOnly(s.handleRestart))
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, name, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "code": name, "msg": msg})
}

// localOnly 只允许回环地址访问，防止局域网内被别人拿来当跳板。
func localOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			writeErr(w, http.StatusForbidden, "FORBIDDEN", "local only")
			return
		}
		h(w, r)
	}
}

// handleHistory 返回最近的收发记录。
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	items, err := s.Store.ListHistory(limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "items": items})
}

// handleAddHistory 由 CLI 上报发送结果。
func (s *Server) handleAddHistory(w http.ResponseWriter, r *http.Request) {
	var rec core.TransferRecord
	if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	if rec.Ts == 0 {
		rec.Ts = time.Now().Unix()
	}
	if err := s.Store.AddHistory(rec); err != nil {
		writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleCleanHistory 按时间清掉旧的收发记录。
func (s *Server) handleCleanHistory(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OlderThanSec int64 `json:"older_than_sec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	if req.OlderThanSec <= 0 {
		writeErr(w, http.StatusBadRequest, "BAD_AGE", "older_than_sec 必须大于 0")
		return
	}
	n, err := s.Store.DeleteHistoryBefore(time.Now().Unix() - req.OlderThanSec)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": n})
}

// handleDeleteItem 删掉一条记录，可选连文件一起删。
func (s *Server) handleDeleteItem(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	it, ok, err := s.Store.GetItem(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such item")
		return
	}
	if r.URL.Query().Get("files") == "1" && it.Path != "" {
		root := filepath.Clean(s.RecvDir())
		if strings.HasPrefix(filepath.Clean(it.Path), root+string(os.PathSeparator)) {
			_ = os.RemoveAll(it.Path)
		}
	}
	s.clearProgress(id)
	s.clearRanges(id)
	s.clear(id)
	if err := s.Store.DeleteItem(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleClean 按时间清掉旧记录，可选连文件一起删。
func (s *Server) handleClean(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OlderThanSec int64 `json:"older_than_sec"`
		WithFiles    bool  `json:"with_files"`
		DryRun       bool  `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	if req.OlderThanSec <= 0 {
		writeErr(w, http.StatusBadRequest, "BAD_AGE", "older_than_sec 必须大于 0")
		return
	}
	cutoff := time.Now().Unix() - req.OlderThanSec

	items, err := s.Store.ListBefore(cutoff, 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
		return
	}

	if req.DryRun {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "dry_run": true, "count": len(items), "items": items,
		})
		return
	}

	removedFiles := 0
	if req.WithFiles {
		root := filepath.Clean(s.RecvDir())
		sep := string(os.PathSeparator)
		for _, it := range items {
			if it.Path == "" {
				continue
			}
			p := filepath.Clean(it.Path)
			if !strings.HasPrefix(p, root+sep) {
				continue // 只删收件目录里的东西
			}
			if os.RemoveAll(p) == nil {
				removedFiles++
			}
		}
	}

	n, err := s.Store.DeleteBefore(cutoff)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "count": n, "removed_files": removedFiles,
	})
}

// doneFiles 找出目录清单里已经完整落盘的文件（尺寸对得上）。
func (s *Server) doneFiles(root string, m core.Manifest) []string {
	out := []string{}
	if root == "" {
		return out
	}
	for _, e := range m.Entries {
		if e.Dir {
			continue
		}
		p, err := safeJoin(root, e.Rel)
		if err != nil {
			continue
		}
		st, err := os.Stat(p)
		if err != nil || st.IsDir() || st.Size() != e.Size {
			continue
		}
		out = append(out, e.Rel)
	}
	return out
}

// handleRestart 空闲就重启，有传输在跑就等它们跑完再重启。
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if n := s.transferCount(); n > 0 {
		atomic.StoreInt32(&s.pendingRestart, 1)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "deferred": true, "inflight": n,
		})
		return
	}
	if s.RequestRestart != nil {
		go s.RequestRestart()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deferred": false, "inflight": 0})
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"id":          s.Dev.ID,
		"name":        s.Dev.Name,
		"platform":    s.Dev.Platform,
		"version":     s.Dev.Version,
		"port":        s.Port,
		"uptime_sec":  int64(time.Since(s.StartedAt).Seconds()),
		"auto_accept": s.AutoAccept,
		"tls":         s.Fingerprint != "",
		"fingerprint": s.Fingerprint,
	})
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	peers, err := s.Store.ListPeers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
		return
	}
	out := make([]core.Peer, 0, len(peers))
	for _, p := range peers {
		if p.ID != s.Dev.ID {
			out = append(out, p)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "peers": out})
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	if s.Disc == nil {
		writeErr(w, http.StatusServiceUnavailable, "NO_DISCOVERY", "discovery not ready")
		return
	}
	ctx := r.Context()
	peers, err := s.Disc.Scan(ctx, 700*time.Millisecond)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "SCAN", err.Error())
		return
	}
	if r.URL.Query().Get("deep") == "1" {
		seen := map[string]bool{s.Dev.ID: true}
		for _, p := range peers {
			seen[p.ID] = true
		}
		for _, p := range discover.ProbeTCP(ctx, s.Port, 400*time.Millisecond) {
			if seen[p.ID] {
				continue
			}
			seen[p.ID] = true
			_ = s.Store.UpsertPeer(p)
			peers = append(peers, p)
		}
	}
	if peers == nil {
		peers = []core.Peer{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "peers": peers})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "STREAM", "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")

	ch, cancel := s.Bus.Subscribe()
	defer cancel()

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	var since int64
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			since = n
		}
	}
	items, err := s.Store.ListItems(limit, since)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "items": items})
}

func (s *Server) handleText(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body string `json:"body"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		writeErr(w, http.StatusBadRequest, "EMPTY_BODY", "body is empty")
		return
	}

	if req.To == "" {
		s.sendPublic(w, r, body)
		return
	}
	s.sendDM(w, r, req.To, body)
}

func (s *Server) sendPublic(w http.ResponseWriter, r *http.Request, body string) {
	if s.Pub == nil {
		writeErr(w, http.StatusServiceUnavailable, "NO_BROADCAST", "broadcast not ready")
		return
	}
	// 超长文本广播不了，降级为逐台 TCP 投递。
	if len(body) > core.MaxPublicBody {
		peers, err := s.Store.ListPeers()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
			return
		}
		var delivered []string
		var lastErr string
		for _, p := range peers {
			if err := s.pushText(r.Context(), p, core.ChanPublic, body); err != nil {
				lastErr = err.Error()
				continue
			}
			delivered = append(delivered, p.Name)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "channel": "public", "degraded": true,
			"acked": delivered, "warn": lastErr,
		})
		return
	}

	pkt := core.Packet{
		V: 1, T: "msg", ID: newID(), SID: s.Dev.ID, SName: s.Dev.Name,
		Channel: "public", Body: body, Ts: time.Now().Unix(),
	}
	acked, err := s.Pub.Send(s.broadcastTargets(), pkt, 600*time.Millisecond)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "BROADCAST", err.Error())
		return
	}

	// 自己也留一份，收件箱里能看到"我说过什么"。
	it := core.Item{
		ID: pkt.ID, Kind: core.KindText, FromID: s.Dev.ID, FromName: s.Dev.Name,
		ToID: core.PublicTo, Channel: core.ChanPublic, Body: body,
		Status: core.StatusDone, CreatedAt: pkt.Ts,
	}
	if err := s.Store.AddItem(it); err == nil {
		s.Bus.Publish(core.Event{Type: core.EvItemNew, Ts: it.CreatedAt, Data: it})
	}

	resp := map[string]any{"ok": true, "channel": "public", "acked": acked}
	if len(acked) == 0 {
		resp["warn"] = "no_ack"
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) sendDM(w http.ResponseWriter, r *http.Request, to, body string) {
	peer, ok := s.resolvePeer(to)
	if !ok {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "device not found: "+to)
		return
	}
	if err := s.pushText(r.Context(), peer, core.ChanDM, body); err != nil {
		writeErr(w, http.StatusBadGateway, "DELIVER", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "channel": "dm", "to": peer.Name, "acked": []string{peer.Name},
	})
}

func (s *Server) pushText(ctx context.Context, peer core.Peer, ch core.Channel, body string) error {
	base := peer.BaseURL()
	if base == "" {
		return fmt.Errorf("peer %s has no address", peer.Name)
	}
	_, err := client.Offer(ctx, base, client.OfferReq{
		FromID: s.Dev.ID, FromName: s.Dev.Name, ToID: peer.ID,
		Kind: string(core.KindText), Body: body,
	})
	return err
}

// resolvePeer 支持完整 id、id 前缀、设备名前缀，歧义时报错由调用方处理。
func (s *Server) resolvePeer(target string) (core.Peer, bool) {
	peers, err := s.Store.ListPeers()
	if err != nil {
		return core.Peer{}, false
	}
	t := strings.ToLower(target)
	var byName []core.Peer
	for _, p := range peers {
		if p.ID == target {
			return p, true
		}
		if strings.HasPrefix(p.ID, target) {
			return p, true
		}
		if strings.ToLower(p.Name) == t {
			return p, true
		}
		if strings.HasPrefix(strings.ToLower(p.Name), t) {
			byName = append(byName, p)
		}
	}
	if len(byName) == 1 {
		return byName[0], true
	}
	return core.Peer{}, false
}

func (s *Server) broadcastTargets() []*net.UDPAddr {
	ips, err := discover.BroadcastAddrs()
	if err != nil || len(ips) == 0 {
		ips = []net.IP{net.IPv4bcast}
	}
	out := make([]*net.UDPAddr, 0, len(ips))
	for _, ip := range ips {
		out = append(out, &net.UDPAddr{IP: ip, Port: core.PublicPort})
	}
	return out
}

// handleShutdown 让本机服务退出，仅限回环地址。
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	if s.Shutdown != nil {
		go s.Shutdown()
	}
}
