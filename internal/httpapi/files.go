package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"xlan/core"
	"xlan/internal/client"
)

func (s *Server) handleOffer(w http.ResponseWriter, r *http.Request) {
	var req client.OfferReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	if req.ToID != "" && req.ToID != s.Dev.ID {
		writeErr(w, http.StatusForbidden, "NOT_FOR_ME", "offer addressed to another device")
		return
	}
	kind := core.Kind(req.Kind)
	if kind == "" {
		kind = core.KindText
	}
	if kind == core.KindText && strings.TrimSpace(req.Body) == "" {
		writeErr(w, http.StatusBadRequest, "EMPTY_BODY", "text body is empty")
		return
	}
	if kind == core.KindFolder && (req.Manifest == nil || req.Manifest.Root == "") {
		writeErr(w, http.StatusBadRequest, "BAD_MANIFEST", "folder offer needs a manifest")
		return
	}

	id := req.TransferID
	if id == "" {
		id = newID()
	}

	// 同名且内容相同：不用传了，直接指到已有文件。
	if kind == core.KindFile && req.SHA256 != "" && req.Name != "" {
		dest := filepath.Join(s.RecvDir(), sanitizeName(req.Name))
		if st, err := os.Stat(dest); err == nil && !st.IsDir() && st.Size() == req.Size {
			if sum, err := fileSHA256(dest); err == nil && strings.EqualFold(sum, req.SHA256) {
				it := core.Item{
					ID: id, Kind: kind, FromID: req.FromID, FromName: req.FromName,
					ToID: s.Dev.ID, Channel: core.ChanDM, Name: req.Name,
					Size: req.Size, SHA256: req.SHA256, Status: core.StatusDone,
					Path: dest, CreatedAt: time.Now().Unix(),
				}
				if err := s.Store.AddItem(it); err != nil {
					writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
					return
				}
				s.Bus.Publish(core.Event{Type: core.EvItemNew, Ts: it.CreatedAt, Data: it})
				writeJSON(w, http.StatusOK, map[string]any{
					"ok": true, "id": id, "accepted": true, "status": "done",
					"duplicate": true, "path": dest,
				})
				return
			}
		}
	}

	// 目录：同一个 transfer_id 已经收过一部分，就接着传，跳过已完成的文件。
	if kind == core.KindFolder && req.TransferID != "" {
		if old, ok, _ := s.Store.GetItem(req.TransferID); ok && old.Kind == core.KindFolder &&
			(old.Status == core.StatusActive || old.Status == core.StatusDone) {
			done := s.doneFiles(old.Path, *req.Manifest)
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "id": old.ID, "accepted": true, "status": string(old.Status),
				"resumed": true, "done_files": done,
			})
			return
		}
	}

	accepted := false
	if kind == core.KindText || s.AutoAccept {
		accepted = true // 小文本、以及自动接收模式下，都不用确认
	} else if peer, ok, _ := s.Store.GetPeer(req.FromID); ok && peer.Trusted {
		accepted = true
	}

	status := core.StatusPending
	switch {
	case kind == core.KindText:
		status = core.StatusDone
	case accepted:
		status = core.StatusActive
	}

	it := core.Item{
		ID: id, Kind: kind, FromID: req.FromID, FromName: req.FromName,
		ToID: s.Dev.ID, Channel: core.ChanDM,
		Body: req.Body, Name: req.Name, Size: req.Size, SHA256: req.SHA256,
		Status: status, CreatedAt: time.Now().Unix(),
	}
	if kind == core.KindFolder {
		it.Name = req.Manifest.Root
		it.Size = req.Manifest.TotalSize()
	}
	if err := s.Store.AddItem(it); err != nil {
		writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
		return
	}
	if kind == core.KindFolder {
		raw, err := json.Marshal(req.Manifest)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "BAD_MANIFEST", err.Error())
			return
		}
		if err := s.Store.SetManifest(id, string(raw)); err != nil {
			writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
			return
		}
		if accepted {
			root, err := s.acceptFolder(it)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "FOLDER", err.Error())
				return
			}
			it.Path = root
		}
	}
	s.Bus.Publish(core.Event{Type: core.EvItemNew, Ts: it.CreatedAt, Data: it})

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": id, "accepted": accepted, "status": string(status),
	})
}

func (s *Server) handleGetOffer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	it, ok, err := s.Store.GetItem(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such offer")
		return
	}
	var offset int64
	if st, err := os.Stat(s.partPath(id)); err == nil {
		offset = st.Size()
	}
	if p := s.rangePrefix(id); p > 0 {
		offset = p
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": it.ID, "status": string(it.Status),
		"accepted": it.Status == core.StatusActive || it.Status == core.StatusDone,
		"offset":   offset, "size": it.Size,
	})
}

func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	it, ok, err := s.Store.GetItem(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such offer")
		return
	}
	// cancel 可以作用在 pending 和 active 上，用来清理卡死的传输。
	if req.Action == "cancel" {
		if it.Status == core.StatusDone || it.Status == core.StatusFailed {
			writeErr(w, http.StatusConflict, "FINISHED", "这条已经结束了")
			return
		}
		s.clearProgress(id)
		s.clearRanges(id)
		s.clear(id)
		s.maybeRestart()
		if err := s.Store.UpdateItemStatus(id, core.StatusFailed, it.Path); err != nil {
			writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
			return
		}
		s.Bus.Publish(core.Event{Type: core.EvItemUpdate, Ts: time.Now().Unix(),
			Data: map[string]any{"id": id, "status": string(core.StatusFailed),
				"name": it.Name, "size": it.Size}})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "failed"})
		return
	}

	if it.Status != core.StatusPending {
		writeErr(w, http.StatusConflict, "NOT_PENDING", "offer is "+string(it.Status))
		return
	}

	switch req.Action {
	case "accept":
		s.touch(id)
		if it.Kind == core.KindFolder {
			if _, err := s.acceptFolder(it); err != nil {
				writeErr(w, http.StatusInternalServerError, "FOLDER", err.Error())
				return
			}
			s.Bus.Publish(core.Event{Type: core.EvItemUpdate, Ts: time.Now().Unix(),
				Data: map[string]any{"id": id, "status": string(core.StatusActive),
					"name": it.Name, "size": it.Size}})
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "active"})
			return
		}
		if err := s.Store.UpdateItemStatus(id, core.StatusActive, it.Path); err != nil {
			writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
			return
		}
		s.Bus.Publish(core.Event{Type: core.EvItemUpdate, Ts: time.Now().Unix(),
			Data: map[string]any{"id": id, "status": string(core.StatusActive),
				"name": it.Name, "size": it.Size}})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "active"})
	case "reject":
		s.clear(id)
		defer s.maybeRestart()
		if err := s.Store.UpdateItemStatus(id, core.StatusFailed, it.Path); err != nil {
			writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
			return
		}
		s.Bus.Publish(core.Event{Type: core.EvItemUpdate, Ts: time.Now().Unix(),
			Data: map[string]any{"id": id, "status": string(core.StatusFailed),
				"name": it.Name, "size": it.Size}})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "failed"})
	default:
		writeErr(w, http.StatusBadRequest, "BAD_ACTION", "action must be accept or reject")
	}
}

func (s *Server) handlePutData(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.touch(id)
	defer s.touch(id)

	it, ok, err := s.Store.GetItem(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such offer")
		return
	}
	if it.Status != core.StatusActive {
		writeErr(w, http.StatusConflict, "NOT_ACTIVE", "offer is "+string(it.Status))
		return
	}

	start, end, total, err := parseContentRange(r.Header.Get("Content-Range"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_RANGE", err.Error())
		return
	}
	if total > 0 && it.Size > 0 && total != it.Size {
		writeErr(w, http.StatusBadRequest, "SIZE_MISMATCH", "total does not match offer size")
		return
	}

	part := s.partPath(id)
	if err := os.MkdirAll(filepath.Dir(part), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "FS", err.Error())
		return
	}
	f, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "FS", err.Error())
		return
	}
	defer f.Close()

	// 按偏移直接写，这样多线程可以乱序传同一个文件。
	n, err := io.Copy(&offsetWriter{f: f, off: start}, r.Body)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "WRITE", err.Error())
		return
	}
	if start+int64(n) != end+1 {
		writeErr(w, http.StatusBadRequest, "SHORT", "body shorter than declared range")
		return
	}

	covered, complete := s.markRange(id, start, end+1, total)
	s.noteReceived(id, covered, total, complete)

	if !complete {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "offset": covered})
		return
	}

	if err := s.finalize(it, part); err != nil {
		if errors.Is(err, ErrChecksum) {
			writeErr(w, http.StatusUnprocessableEntity, "CHECKSUM", err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "FINALIZE", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "offset": covered, "done": true})
}

// offsetWriter 用 WriteAt 写文件，支持多个请求并发写不同偏移。
type offsetWriter struct {
	f   *os.File
	off int64
}

func (w *offsetWriter) Write(p []byte) (int, error) {
	n, err := w.f.WriteAt(p, w.off)
	w.off += int64(n)
	return n, err
}

var ErrChecksum = errors.New("checksum mismatch")

func (s *Server) finalize(it core.Item, part string) error {
	if it.SHA256 != "" {
		got, err := fileSHA256(part)
		if err != nil {
			s.clearProgress(it.ID)
			return err
		}
		if !strings.EqualFold(got, it.SHA256) {
			os.Remove(part)
			s.clearProgress(it.ID)
			return fmt.Errorf("%w: want %s, got %s", ErrChecksum, it.SHA256, got)
		}
	}
	dir := s.RecvDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	final := uniquePath(dir, it.Name)
	if err := os.Rename(part, final); err != nil {
		return err
	}
	if err := s.Store.UpdateItemStatus(it.ID, core.StatusDone, final); err != nil {
		return err
	}
	s.Bus.Publish(core.Event{Type: core.EvItemUpdate, Ts: time.Now().Unix(),
		Data: map[string]any{"id": it.ID, "status": string(core.StatusDone),
			"path": final, "name": it.Name, "size": it.Size}})
	s.clear(it.ID)
	s.clearProgress(it.ID)
	s.clearRanges(it.ID)
	s.maybeRestart()

	d := time.Now().Unix() - it.CreatedAt
	if d < 0 {
		d = 0
	}
	_ = s.Store.AddHistory(core.TransferRecord{
		Ts: time.Now().Unix(), Direction: "recv", PeerName: it.FromName,
		Name: it.Name, Kind: string(it.Kind), Size: it.Size, Status: "done",
		DurationMS: d * 1000,
	})
	return nil
}

func (s *Server) partPath(id string) string {
	return filepath.Join(s.RecvDir(), ".parts", safeID(id)+".part")
}

func safeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func parseContentRange(h string) (start, end, total int64, err error) {
	h = strings.TrimSpace(h)
	if !strings.HasPrefix(h, "bytes ") {
		return 0, 0, 0, errors.New("missing or malformed Content-Range")
	}
	h = strings.TrimPrefix(h, "bytes ")
	parts := strings.SplitN(h, "/", 2)
	if len(parts) != 2 {
		return 0, 0, 0, errors.New("malformed Content-Range")
	}
	rg := strings.SplitN(parts[0], "-", 2)
	if len(rg) != 2 {
		return 0, 0, 0, errors.New("malformed Content-Range")
	}
	start, err1 := strconv.ParseInt(rg[0], 10, 64)
	end, err2 := strconv.ParseInt(rg[1], 10, 64)
	total, err3 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, 0, errors.New("malformed Content-Range")
	}
	if start < 0 || end < start || total < 0 || end >= total {
		return 0, 0, 0, errors.New("invalid range")
	}
	return start, end, total, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// uniquePath 在重名时自动加 " (1)"，绝不覆盖已有文件。
func uniquePath(dir, name string) string {
	name = sanitizeName(name)
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; i < 10000; i++ {
		cand := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%d%s", base, time.Now().UnixNano(), ext))
}

// uniqueDir 与 uniquePath 同理，但目录不加扩展名。
func uniqueDir(dir, name string) string {
	name = sanitizeName(name)
	if name == "unnamed" {
		name = "folder"
	}
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	for i := 1; i < 10000; i++ {
		cand := filepath.Join(dir, fmt.Sprintf("%s (%d)", name, i))
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%d", name, time.Now().UnixNano()))
}

var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// sanitizeName 防路径穿越，跨平台都能安全落盘。
func sanitizeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '|', '?', '*', 0:
			return '_'
		}
		if r < 0x20 {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	name = strings.Trim(name, ".")
	if name == "" {
		return "unnamed"
	}
	upper := strings.ToUpper(name)
	if i := strings.Index(upper, "."); i > 0 {
		upper = upper[:i]
	}
	if reservedNames[upper] {
		name = "_" + name
	}
	if len(name) > 200 {
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		base := strings.TrimSuffix(name, ext)
		limit := 200 - len(ext)
		var b strings.Builder
		for _, r := range base {
			if b.Len()+utf8.RuneLen(r) > limit {
				break
			}
			b.WriteRune(r)
		}
		name = b.String() + ext
	}
	return name
}

func newID() string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("%d-%x", time.Now().UnixNano(), buf)
}

func (s *Server) handleGetItem(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "item": it})
}

func (s *Server) handleTrust(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      string `json:"id"`
		Trusted bool   `json:"trusted"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	if req.ID == "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "missing id")
		return
	}
	if err := s.Store.SetTrusted(req.ID, req.Trusted); err != nil {
		writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": req.ID, "trusted": req.Trusted})
}
