package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
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
	RecvDir   string
	StartedAt time.Time
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
	mux.HandleFunc("POST /v1/trust", localOnly(s.handleTrust))
	mux.HandleFunc("POST /v1/offer", s.handleOffer)
	mux.HandleFunc("GET /v1/offer/{id}", s.handleGetOffer)
	mux.HandleFunc("POST /v1/offer/{id}/decision", localOnly(s.handleDecision))
	mux.HandleFunc("PUT /v1/offer/{id}/data", s.handlePutData)
	mux.HandleFunc("PUT /v1/offer/{id}/file/{rel...}", s.handlePutFile)
	mux.HandleFunc("POST /v1/offer/{id}/finish", s.handleFinish)
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

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"id":         s.Dev.ID,
		"name":       s.Dev.Name,
		"platform":   s.Dev.Platform,
		"version":    s.Dev.Version,
		"port":       s.Port,
		"uptime_sec": int64(time.Since(s.StartedAt).Seconds()),
	})
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	peers, err := s.Store.ListPeers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "peers": peers})
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
		extra := discover.ScanTCP(ctx, s.Store, s.Port, 400*time.Millisecond)
		seen := map[string]bool{}
		for _, p := range peers {
			seen[p.ID] = true
		}
		for _, p := range extra {
			if !seen[p.ID] {
				peers = append(peers, p)
			}
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
