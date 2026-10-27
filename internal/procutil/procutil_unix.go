//go:build !windows

package procutil

import (
	"os/exec"
	"syscall"
)

// KillGroupOnCancel 让命令运行在独立的进程组中；ctx 取消（超时）时向整个进程组发送 SIGKILL，
// 这样 `sleep 100 &`、`go test` 派生出的子进程也会一起结束，不会变成孤儿进程。
func KillGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
}
