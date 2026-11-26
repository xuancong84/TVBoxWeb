//go:build !windows

package tvbox

import (
	"os"
	"syscall"
)

// reexec 原地替换当前进程（保持 PID，监听 fd 因 CLOEXEC 会被内核关掉）。
func reexec(exe string, args []string) error {
	return syscall.Exec(exe, args, os.Environ())
}
