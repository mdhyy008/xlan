package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"xlan/core"
)

// safeJoin 把相对路径拼到 root 下，任何越界都拒绝。
func safeJoin(root, rel string) (string, error) {
	rel = strings.ReplaceAll(rel, "\\", "/")
	if rel == "" {
		return "", errors.New("empty path")
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || clean == ".." || filepath.IsAbs(clean) ||
		strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe path: %q", rel)
	}
	// Windows 上把非法字符和设备名换掉，否则可能写到设备节点里。
	if runtime.GOOS == "windows" {
		parts := strings.Split(filepath.ToSlash(clean), "/")
		for i, p := range parts {
			parts[i] = sanitizeName(p)
		}
		clean = filepath.FromSlash(strings.Join(parts, "/"))
	}
	dest := filepath.Join(root, clean)
	base := filepath.Clean(root) + string(os.PathSeparator)
	if !strings.HasPrefix(dest, base) {
		return "", fmt.Errorf("unsafe path: %q", rel)
	}
	return dest, nil
}

// createFolderRoot 在收件目录下建出目录树，返回根路径。
func (s *Server) createFolderRoot(m core.Manifest) (string, error) {
	root := uniqueDir(s.RecvDir(), m.Root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	for _, e := range m.Entries {
		if !e.Dir {
			continue
		}
		p, err := safeJoin(root, e.Rel)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			return "", err
		}
	}
	return root, nil
}

func (s *Server) handlePutFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rel := r.PathValue("rel")
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
	if it.Kind != core.KindFolder {
		writeErr(w, http.StatusBadRequest, "NOT_A_FOLDER", "this offer is not a folder")
		return
	}
	if it.Status != core.StatusActive {
		writeErr(w, http.StatusConflict, "NOT_ACTIVE", "offer is "+string(it.Status))
		return
	}

	raw, err := s.Store.GetManifest(id)
	if err != nil || raw == "" {
		writeErr(w, http.StatusInternalServerError, "MANIFEST", "manifest missing")
		return
	}
	var m core.Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		writeErr(w, http.StatusInternalServerError, "MANIFEST", err.Error())
		return
	}

	var entry *core.FolderEntry
	for i := range m.Entries {
		if !m.Entries[i].Dir && m.Entries[i].Rel == rel {
			entry = &m.Entries[i]
			break
		}
	}
	if entry == nil {
		writeErr(w, http.StatusNotFound, "NOT_IN_MANIFEST", "unknown file: "+rel)
		return
	}

	start, end, total, err := parseContentRange(r.Header.Get("Content-Range"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_RANGE", err.Error())
		return
	}
	if total != entry.Size {
		writeErr(w, http.StatusBadRequest, "SIZE_MISMATCH", "total does not match manifest")
		return
	}

	dest, err := safeJoin(it.Path, rel)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "UNSAFE_PATH", err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "FS", err.Error())
		return
	}

	part := dest + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "FS", err.Error())
		return
	}
	defer f.Close()

	cur, _ := f.Seek(0, io.SeekEnd)
	if start > cur {
		writeErr(w, http.StatusConflict, "GAP", fmt.Sprintf("expected offset %d, got %d", cur, start))
		return
	}
	if start < cur {
		if err := f.Truncate(start); err != nil {
			writeErr(w, http.StatusInternalServerError, "FS", err.Error())
			return
		}
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		writeErr(w, http.StatusInternalServerError, "FS", err.Error())
		return
	}
	n, err := io.Copy(f, r.Body)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "WRITE", err.Error())
		return
	}
	if start+int64(n) != end+1 {
		writeErr(w, http.StatusBadRequest, "SHORT", "body shorter than declared range")
		return
	}
	s.noteReceived(id, s.committedOf(id)+start+int64(n), it.Size, end+1 >= total)
	if end+1 < total {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "offset": start + int64(n)})
		return
	}

	f.Close()
	if entry.SHA256 != "" {
		got, err := fileSHA256(part)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "FS", err.Error())
			return
		}
		if !strings.EqualFold(got, entry.SHA256) {
			os.Remove(part)
			writeErr(w, http.StatusUnprocessableEntity, "CHECKSUM", "checksum mismatch for "+rel)
			return
		}
	}
	if err := os.Rename(part, dest); err != nil {
		writeErr(w, http.StatusInternalServerError, "FS", err.Error())
		return
	}
	s.bumpCommitted(id, entry.Size)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "done": true, "rel": rel})
}

func (s *Server) handleFinish(w http.ResponseWriter, r *http.Request) {
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
	if it.Kind != core.KindFolder {
		writeErr(w, http.StatusBadRequest, "NOT_A_FOLDER", "this offer is not a folder")
		return
	}

	raw, _ := s.Store.GetManifest(id)
	var m core.Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		writeErr(w, http.StatusInternalServerError, "MANIFEST", err.Error())
		return
	}

	missing := 0
	for _, e := range m.Entries {
		p, err := safeJoin(it.Path, e.Rel)
		if err != nil {
			missing++
			continue
		}
		if e.Dir {
			if st, err := os.Stat(p); err != nil || !st.IsDir() {
				missing++
			}
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			if e.Size == 0 {
				// 空文件不会走上传，这里补出来
				if werr := os.MkdirAll(filepath.Dir(p), 0o755); werr == nil {
					if werr := os.WriteFile(p, nil, 0o644); werr == nil {
						continue
					}
				}
			}
			missing++
			continue
		}
		if st.Size() != e.Size {
			missing++
		}
	}

	if missing > 0 {
		writeErr(w, http.StatusConflict, "INCOMPLETE", fmt.Sprintf("%d of %d entries missing", missing, len(m.Entries)))
		return
	}

	if err := s.Store.UpdateItemStatus(id, core.StatusDone, it.Path); err != nil {
		writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
		return
	}
	s.Bus.Publish(core.Event{Type: core.EvItemUpdate, Ts: time.Now().Unix(),
		Data: map[string]any{"id": id, "status": string(core.StatusDone),
			"path": it.Path, "name": it.Name, "size": it.Size}})
	s.clear(id)
	s.clearProgress(id)
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "done": true, "path": it.Path})
}

// acceptFolder 在用户确认后建目录树。
func (s *Server) acceptFolder(it core.Item) (string, error) {
	raw, err := s.Store.GetManifest(it.ID)
	if err != nil || raw == "" {
		return "", errors.New("manifest missing")
	}
	var m core.Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return "", err
	}
	root, err := s.createFolderRoot(m)
	if err != nil {
		return "", err
	}
	if err := s.Store.UpdateItemStatus(it.ID, core.StatusActive, root); err != nil {
		return "", err
	}
	return root, nil
}

func digestFile(path string) (string, error) {
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
