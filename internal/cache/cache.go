// Package cache 清理 xlan 运行过程中留下的临时数据。
package cache

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"xlan/core"
	"xlan/internal/store"
)

// IdleAfter 是判断分片是否已经停止写入的时长。
const IdleAfter = 5 * time.Minute

// CleanParts 删掉 .parts 里已经不在传的临时分片。
// 最近 5 分钟还在写的跳过，避免打断正在进行的传输。
func CleanParts(recvDir string, dryRun bool, onRemove func(id string)) (int, int64, error) {
	dir := filepath.Join(recvDir, ".parts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	cut := time.Now().Add(-IdleAfter)
	var count int
	var freed int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cut) {
			continue
		}
		if !dryRun {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				continue
			}
			if onRemove != nil {
				onRemove(strings.TrimSuffix(e.Name(), ".part"))
			}
		}
		count++
		freed += info.Size()
	}
	return count, freed, nil
}

// CleanRecords 删掉早于 cutoff、已经结束的收件箱记录，文件本体保留。
func CleanRecords(st *store.Store, cutoff int64, dryRun bool, onRemove func(it core.Item)) (int, error) {
	items, err := st.ListBefore(cutoff, 0)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, it := range items {
		switch it.Status {
		case core.StatusDone, core.StatusFailed, core.StatusExpired:
		default:
			continue
		}
		n++
		if dryRun {
			continue
		}
		if onRemove != nil {
			onRemove(it)
		}
		_ = st.DeleteItem(it.ID)
	}
	return n, nil
}
