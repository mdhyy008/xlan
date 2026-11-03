package httpapi

import "sort"

type span struct {
	Start int64 `json:"s"`
	End   int64 `json:"e"`
}

// spanSet 记录一个文件已经收到哪些字节区间，支持乱序到达。
type spanSet struct {
	spans   []span
	covered int64
}

func (ss *spanSet) add(a, b int64) int64 {
	if b <= a {
		return ss.covered
	}
	ss.spans = append(ss.spans, span{Start: a, End: b})
	sort.Slice(ss.spans, func(i, j int) bool {
		if ss.spans[i].Start == ss.spans[j].Start {
			return ss.spans[i].End < ss.spans[j].End
		}
		return ss.spans[i].Start < ss.spans[j].Start
	})

	out := make([]span, 0, len(ss.spans))
	for _, sp := range ss.spans {
		if n := len(out); n > 0 && sp.Start <= out[n-1].End {
			if sp.End > out[n-1].End {
				out[n-1].End = sp.End
			}
			continue
		}
		out = append(out, sp)
	}
	ss.spans = out

	var cov int64
	for _, sp := range out {
		cov += sp.End - sp.Start
	}
	ss.covered = cov
	return cov
}

// prefix 返回从 0 开始连续收到多长，续传时用这个位置。
func (ss *spanSet) prefix() int64 {
	if len(ss.spans) == 0 || ss.spans[0].Start != 0 {
		return 0
	}
	return ss.spans[0].End
}
