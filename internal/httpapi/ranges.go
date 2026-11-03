package httpapi

import "sort"

type span struct{ start, end int64 }

// spanSet 记录一个文件已经收到哪些字节区间，支持乱序到达。
type spanSet struct {
	spans   []span
	covered int64
}

func (ss *spanSet) add(a, b int64) int64 {
	if b <= a {
		return ss.covered
	}
	ss.spans = append(ss.spans, span{a, b})
	sort.Slice(ss.spans, func(i, j int) bool {
		if ss.spans[i].start == ss.spans[j].start {
			return ss.spans[i].end < ss.spans[j].end
		}
		return ss.spans[i].start < ss.spans[j].start
	})

	out := make([]span, 0, len(ss.spans))
	for _, sp := range ss.spans {
		if n := len(out); n > 0 && sp.start <= out[n-1].end {
			if sp.end > out[n-1].end {
				out[n-1].end = sp.end
			}
			continue
		}
		out = append(out, sp)
	}
	ss.spans = out

	var cov int64
	for _, sp := range out {
		cov += sp.end - sp.start
	}
	ss.covered = cov
	return cov
}

// prefix 返回从 0 开始连续收到多长，续传时用这个位置。
func (ss *spanSet) prefix() int64 {
	if len(ss.spans) == 0 || ss.spans[0].start != 0 {
		return 0
	}
	return ss.spans[0].end
}
