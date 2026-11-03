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
