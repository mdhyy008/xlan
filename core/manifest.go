package core

// FolderEntry 是目录传输里的一个条目。
// 目录条目只带 Rel，文件条目带大小与校验和。
type FolderEntry struct {
	Rel    string `json:"rel"`
	Dir    bool   `json:"dir,omitempty"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// Manifest 描述一次目录传输的完整结构，先发给对方再逐个传文件。
type Manifest struct {
	Root    string        `json:"root"`
	Entries []FolderEntry `json:"entries"`
}

func (m Manifest) FileCount() int {
	n := 0
	for _, e := range m.Entries {
		if !e.Dir {
			n++
		}
	}
	return n
}

func (m Manifest) TotalSize() int64 {
	var n int64
	for _, e := range m.Entries {
		if !e.Dir {
			n += e.Size
		}
	}
	return n
}
