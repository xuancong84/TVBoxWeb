//go:build !linux

package api

// lowerThreadPriority 在非 Linux 平台是空实现（macOS/Windows 没有跟
// syscall.Setpriority/Gettid 对应的、免额外依赖的标准库调用）。扫描功能本身
// 不受影响，只是不会真的把线程优先级降到最低——实际部署目标是 Linux
// （树莓派），这里只是让跨平台交叉编译（见 build.sh）能正常通过。
func lowerThreadPriority() {}
