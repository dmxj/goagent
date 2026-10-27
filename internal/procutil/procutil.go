// Package procutil 封装与操作系统相关的子进程细节，
// 让 bash 工具与命令钩子在 Unix 与 Windows 上都能编译运行。
package procutil

import (
	"context"
	"os"
	"os/exec"
	"runtime"
)

// Shell 返回执行一段命令行所用的解释器及其参数前缀。
// Unix 上优先 /bin/bash（不存在时回退 /bin/sh）；Windows 上使用 cmd /C。
// preferred 非空时直接使用（Unix 下以 -c 传入命令）。
func Shell(preferred string) (string, []string) {
	if runtime.GOOS == "windows" {
		if preferred != "" {
			return preferred, []string{"-c"}
		}
		return "cmd", []string{"/C"}
	}
	if preferred != "" {
		return preferred, []string{"-c"}
	}
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash", []string{"-c"}
	}
	return "/bin/sh", []string{"-c"}
}

// Command 创建在 ctx 下执行 command 的 *exec.Cmd，并配置为"取消时杀掉整个进程树"。
func Command(ctx context.Context, shell, command string) *exec.Cmd {
	name, args := Shell(shell)
	cmd := exec.CommandContext(ctx, name, append(args, command)...)
	KillGroupOnCancel(cmd)
	return cmd
}
