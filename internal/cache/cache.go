// Package cache 清理 xlan 运行过程中留下的临时数据。
package cache

import (
	"io/fs"
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
// keep 返回 true 的分片会跳过（通常用来保护正在传输的文件）；
// 传 nil 时只按修改时间判断，最近 IdleAfter 内写过的跳过。
func CleanParts(recvDir string, dryRun bool, keep func(id string) bool, onRemove func(id string)) (int, int64, error) {
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
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".part")
		if keep != nil {
			if keep(id) {
				continue
			}
		} else if info.ModTime().After(cut) {
			continue
		}
		if !dryRun {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				continue
			}
			if onRemove != nil {
				onRemove(id)
			}
		}
		count++
		freed += info.Size()
	}
	// 分片清空后别再在收件目录里留一个空文件夹。
	if !dryRun {
		_ = os.Remove(dir)
	}
	return count, freed, nil
}

// CleanStrayParts 删掉收件目录里不属于进行中任务的 .part 半成品。
// 文件夹传输是把每个文件以「原名.part」直接写进目标目录的，
// active 里的文件夹根目录会整棵保留，其余 .part 一律清掉。
func CleanStrayParts(recvDir string, dryRun bool, active []core.Item) (int, int64, error) {
	partsDir := filepath.Join(recvDir, ".parts")
	roots := make([]string, 0, len(active))
	for _, it := range active {
		if it.Kind == core.KindFolder && it.Path != "" {
			roots = append(roots, filepath.Clean(it.Path)+string(os.PathSeparator))
		}
	}
	var count int
	var freed int64
	err := filepath.WalkDir(recvDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if filepath.Clean(p) == partsDir {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".part") {
			return nil
		}
		clean := filepath.Clean(p)
		for _, root := range roots {
			if strings.HasPrefix(clean, root) {
				return nil
			}
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		count++
		freed += info.Size()
		if !dryRun {
			_ = os.Remove(p)
		}
		return nil
	})
	return count, freed, err
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
