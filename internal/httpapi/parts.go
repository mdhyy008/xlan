package httpapi

import (
	"os"
	"path/filepath"
	"strings"

	"xlan/core"
)

// cleanPartial 删掉一条没传完的传输留下的半成品。
// 单文件的分片在 .parts 里按 id 存放；文件夹传输在目标目录里按原名写 .part。
func (s *Server) cleanPartial(it core.Item) {
	part := s.partPath(it.ID)
	_ = os.Remove(part)
	// 这是单文件接收的暂存目录，空了就删掉，别留在收件目录里。
	_ = os.Remove(filepath.Dir(part))
	s.clearRanges(it.ID)
	if it.Kind != core.KindFolder || it.Path == "" {
		return
	}
	root := filepath.Clean(it.Path)
	recv := filepath.Clean(s.RecvDir())
	if !strings.HasPrefix(root, recv+string(os.PathSeparator)) {
		return
	}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".part") {
			_ = os.Remove(p)
		}
		return nil
	})
}
