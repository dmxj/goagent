//go:build windows

package procutil

import "os/exec"

// KillGroupOnCancel 在 Windows 上退化为只结束主进程（Windows 没有 Unix 进程组语义）。
func KillGroupOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
