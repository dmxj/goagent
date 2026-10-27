package procutil

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestShell(t *testing.T) {
	name, args := Shell("/bin/zsh")
	if name != "/bin/zsh" || len(args) != 1 || args[0] != "-c" {
		t.Errorf("指定 shell 时应原样使用：%s %v", name, args)
	}
	name, args = Shell("")
	if runtime.GOOS == "windows" {
		if name != "cmd" || args[0] != "/C" {
			t.Errorf("Windows 默认应使用 cmd /C：%s %v", name, args)
		}
		return
	}
	if (name != "/bin/bash" && name != "/bin/sh") || args[0] != "-c" {
		t.Errorf("Unix 默认应使用 bash 或 sh：%s %v", name, args)
	}
}

func TestCommandKillsGroupOnCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 Unix shell")
	}
	out, err := Command(context.Background(), "", "echo hi").Output()
	if err != nil || strings.TrimSpace(string(out)) != "hi" {
		t.Fatalf("执行命令失败：%q %v", out, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	cmd := Command(ctx, "", "sleep 30 & sleep 30")
	cmd.WaitDelay = time.Second
	start := time.Now()
	_ = cmd.Run()
	if time.Since(start) > 5*time.Second {
		t.Errorf("取消后应杀掉整个进程组并尽快返回，耗时 %v", time.Since(start))
	}
}
