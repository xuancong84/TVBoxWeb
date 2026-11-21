//go:build linux

package api

import (
	"runtime"
	"syscall"
)

// lowerThreadPriority 把调用它的 goroutine 钉死在一个专属 OS 线程上，并把
// 该线程的调度优先级降到最低（nice 19）——直播频道可播性后台扫描是纯粹
// 「有资源就跑、没有就让」的任务，不该占用跟正常 HTTP 请求处理抢时间片的
// 那份优先级。只影响调用它的这一个线程，不影响进程里其他 goroutine。
//
// 只在 Linux 下生效（实际部署目标，比如树莓派）；其他平台是空实现
// （见 priority_other.go），失败也不影响功能，扫描该怎么跑还怎么跑，
// 只是优先级设不低而已，不值得因此让整个扫描失败。
func lowerThreadPriority() {
	runtime.LockOSThread()
	tid := syscall.Gettid()
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, tid, 19)
}
