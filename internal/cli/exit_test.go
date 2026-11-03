package cli

import (
	"errors"
	"testing"

	"xlan/core"
)

func TestClassifyAndExitCode(t *testing.T) {
	cases := []struct {
		err  error
		code string
		exit int
	}{
		{errors.New("对方拒绝了"), "DENIED", core.ExitDenied},
		{errors.New("对方没有在超时前确认"), "TIMEOUT", core.ExitTimeout},
		{errors.New("upload failed: 422 Unprocessable Entity {\"code\":\"CHECKSUM\"}"), "CHECKSUM", core.ExitChecksum},
		{errors.New("connection refused"), "ERROR", core.ExitError},
	}
	for _, c := range cases {
		got := classify(c.err)
		if got != c.code {
			t.Errorf("classify(%q) = %q, want %q", c.err, got, c.code)
		}
		if e := exitForCode(got); e != c.exit {
			t.Errorf("exitForCode(%q) = %d, want %d", got, e, c.exit)
		}
	}
}

func TestParseLimit(t *testing.T) {
	ok := map[string]int64{
		"":        0,
		"0":       0,
		"1048576": 1048576,
		"5M":      5 << 20,
		"500K":    500 << 10,
		"1G":      1 << 30,
		"2MB":     2 << 20,
		"1.5M":    1572864,
	}
	for in, want := range ok {
		got, err := parseLimit(in)
		if err != nil || got != want {
			t.Errorf("parseLimit(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"abc", "5X"} {
		if _, err := parseLimit(bad); err == nil {
			t.Errorf("parseLimit(%q) 应该报错", bad)
		}
	}
}
