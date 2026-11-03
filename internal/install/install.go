package install

import (
	"fmt"
	"os/exec"
	"strings"
)

// run 跑一个外部命令，失败时把输出带进错误里，方便排查。
func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s == "" {
			return "", fmt.Errorf("%s 执行失败: %v", name, err)
		}
		return s, fmt.Errorf("%s 执行失败: %v（%s）", name, err, s)
	}
	return s, nil
}
