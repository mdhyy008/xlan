package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"xlan/internal/config"
)

// sendEntry 是一个正在进行中的发送任务，落到文件里，别的终端也能看到。
type sendEntry struct {
	ID      string   `json:"id"`
	PID     int      `json:"pid"`
	Started int64    `json:"started"`
	Target  string   `json:"target"`
	Paths   []string `json:"paths"`
	Total   int64    `json:"total"`
}

func sendsDir() (string, error) {
	d, err := config.DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "sends"), nil
}

func newSendID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "s" + hex.EncodeToString(b)
}

func registerSend(e sendEntry) (string, error) {
	dir, err := sendsDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if e.ID == "" {
		e.ID = newSendID()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, e.ID+".json"), b, 0o644); err != nil {
		return "", err
	}
	return e.ID, nil
}

func unregisterSend(id string) {
	dir, err := sendsDir()
	if err != nil {
		return
	}
	_ = os.Remove(filepath.Join(dir, id+".json"))
}

// listSends 读出所有还活着的发送任务，顺手清掉已经退出的。
func listSends() []sendEntry {
	dir, err := sendsDir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := []sendEntry{}
	for _, de := range entries {
		if de.IsDir() || filepath.Ext(de.Name()) != ".json" {
			continue
		}
		p := filepath.Join(dir, de.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var e sendEntry
		if json.Unmarshal(b, &e) != nil || e.ID == "" {
			_ = os.Remove(p)
			continue
		}
		if !pidAlive(e.PID) {
			_ = os.Remove(p)
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started < out[j].Started })
	return out
}

func findSend(id string) (sendEntry, bool) {
	for _, e := range listSends() {
		if e.ID == id || (len(id) >= 2 && len(e.ID) > len(id) && e.ID[:len(id)] == id) {
			return e, true
		}
	}
	return sendEntry{}, false
}

func elapsedOf(e sendEntry) time.Duration {
	return time.Since(time.Unix(e.Started, 0)).Round(time.Second)
}
