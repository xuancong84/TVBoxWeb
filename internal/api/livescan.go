package api

import (
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/live"
	"github.com/xxl6097/TVBoxWeb/internal/model"
	"github.com/xxl6097/TVBoxWeb/internal/store"
)

// ---------- 直播频道「可播性」后台扫描 ----------
//
// 跟点播那边 crawler 常驻调度是同一个思路：程序启动后就常驻跑，不需要
// 用户手动触发。每一轮只测「从没测过」或者「测出来播不通但已经超过
// store.ChannelFailMaxAge」的频道——已经确认能播的频道永久跳过，所以
// 大多数轮次其实瞬间就结束（全是已知状态，没什么可测的），liveScanIdle
// 只决定「多快能发现新增频道 / 多快重测过期的播不通记录」。

const (
	// liveScanIdle 一轮扫描结束后的等待时间：发现「没有需要测的」之后多久
	// 再看一眼有没有新频道/过期记录需要重测。liveScanCandidates 现在有
	// 短 TTL 缓存（见下面），这个检查本身很便宜，不需要像以前（候选集
	// 没缓存、每次都要重新合并全部直播源）那样隔很久才查一次——缩短这个
	// 间隔也让「清空重测」（hLiveScanReset）之类的场景不用等到 5 分钟才
	// 被这个循环发现有活干。
	liveScanIdle = 30 * time.Second
	// liveScanParallel 后台扫描的并发探测数：纯后台任务，不阻塞任何用户
	// 请求，可以比前台「检测」按钮（4）给得更高。合并全部直播源后去重
	// 去重出来的频道名常常是十万量级（大量源之间同一个台改头换面叫不
	// 同名字，normalizeChannelName 只做了保守归一化，没法把这些都合并
	// 掉），每个无响应的频道都要等满探测超时，并发给太低的话一轮要扫到
	// 天荒地老；这里给到 24，仍然远低于「扫一次性打出几百并发连接」的
	// 量级。
	liveScanParallel = 24
	// liveScanStreamDur 判定「能播」要求连续拉到的最短时长：只看 manifest/
	// 头几个字节像不像流（旧实现）会把「连得上、manifest 也正常，但实际
	// 分片早就失效、播放器里一直转圈」误判成能播——真正要验证的是「打开
	// 之后能不能实际播出来」，所以这里换成 live.ProbeAnyStreamCtx，真的
	// 连续读够这么长时间的数据才算数。比字面的「至少 1 秒」多给一点点
	// 余量，避免卡在刚好 1000ms 边界上的抖动。
	liveScanStreamDur = 1200 * time.Millisecond
	// liveScanMaxLines 单个频道最多测几条线路：MergeGroups 会把全部源里
	// 同一个频道的线路都揉到一起，热门频道常年能攒到几十条；真实播放器
	// 也是「一条不行就切下一条」，只要有一条能播频道就该算能播——这里给
	// 到 20 条，覆盖「十几条备用线路」的常见情况，同时不让极端情况下的
	// 单个频道测试时间失控。
	liveScanMaxLines = 20
)

var (
	liveScanning atomic.Bool
	liveScanOnce sync.Once
)

// StartLiveScan 启动直播频道可播性后台扫描，程序启动后调用一次。这个循环
// 没有停止开关（不像 crawler 调度器那样接 s.stop），sync.Once 防的是同一
// 进程内重复调用（比如库的使用方意外调了两次 New/ListenAndServe）意外
// 叠出好几个扫描循环。
//
// store.LiveScanEnabled() 关掉时这里只是短间隔空转等着——不是停掉 goroutine，
// 这样设置页随时切回「开」都能立刻从上次的断点接着扫，不用重启进程。
func StartLiveScan() {
	liveScanOnce.Do(func() {
		go func() {
			for {
				if !store.LiveScanEnabled() {
					time.Sleep(10 * time.Second)
					continue
				}
				// 只有「这轮确实测完了候选集」才等 liveScanIdle；如果是半路被
				// 设置页关掉提前收手的，不额外等——直接回到循环顶部，被上面
				// 的 enabled 检查短间隔轮询接住，重新打开后几秒内就能接着扫，
				// 而不是要等满一整个 liveScanIdle（5 分钟）。
				if runLiveScanPass() {
					time.Sleep(liveScanIdle)
				}
			}
		}()
	})
}

// runLiveScanPass 跑一轮扫描：挑出需要测试的频道，实测一遍，结果写回 store。
// 返回值表示这一轮是不是正常跑完全部候选集（而不是被设置页中途关掉打断）。
func runLiveScanPass() bool {
	all := liveScanCandidates()
	if len(all) == 0 {
		return true
	}

	var need []model.Channel
	for _, c := range all {
		if _, known := store.GetChannelStatus(c.Name); !known {
			need = append(need, c)
		}
	}
	if len(need) == 0 {
		return true
	}

	liveScanning.Store(true)
	defer liveScanning.Store(false)
	return scanChannelsLowPriority(need)
}

// scanChannelsLowPriority 用一组「钉死在各自专属 OS 线程上、线程优先级降到
// 最低」的固定 worker 探测一批频道——跟 liveprobe.Channels（给前台「检测」
// 按钮、测速排行等用户主动触发的场景用）刻意分开两套实现，不共用那份默认
// 优先级的逻辑：这是纯后台任务，不该在 CPU/调度上跟正常请求处理抢资源。
//
// worker 数量固定为 liveScanParallel 个，每个 worker 只在启动时调一次
// lowerThreadPriority（LockOSThread 把 goroutine 钉在专属线程上、调一次
// nice），然后反复从任务队列取号——而不是每探测一个频道就起一个新
// goroutine 各自锁一次线程：后者会导致成千上万次「锁线程→探测完 goroutine
// 退出→系统销毁这个线程」，白白产生大量线程创建/销毁开销。
//
// 每 liveScanFlushEvery 秒落一次盘（而不是整轮测完才落一次）：候选频道
// 常有几万条，一轮可能跑好几个小时，万一中途断电，至少不会把这期间测到
// 的结果全部丢光。
const liveScanFlushEvery = 30 * time.Second

func scanChannelsLowPriority(chs []model.Channel) (completed bool) {
	tasks := make(chan model.Channel)
	var wg sync.WaitGroup
	for i := 0; i < liveScanParallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lowerThreadPriority()
			for ch := range tasks {
				urls := ch.URLs
				if len(urls) == 0 && ch.URL != "" {
					urls = []string{ch.URL}
				}
				alive := live.ProbeAnyStream(urls, liveScanStreamDur, liveScanMaxLines)
				store.SetChannelStatusBulk(ch.Name, alive)
			}
		}()
	}

	flushDone := make(chan struct{})
	go func() {
		t := time.NewTicker(liveScanFlushEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				store.FlushLiveCache()
			case <-flushDone:
				return
			}
		}
	}()

	completed = true
feed:
	for _, ch := range chs {
		// 扫描期间随时可能被设置页关掉：不再派发新任务（已经在跑的几个
		// worker 会把手头这个探测做完才退出，不强行中断）。
		if !store.LiveScanEnabled() {
			completed = false
			break feed
		}
		tasks <- ch
	}
	close(tasks)
	wg.Wait()
	close(flushDone)
	store.FlushLiveCache()
	return completed
}

// liveCandidatesCache 缓存 liveScanCandidates 的结果：live.Channels 自己已经
// 做了 mergeCacheTTL 内的缓存，但 FilterJunk + 拍平成 []Channel 这一步每次
// 还是要整份重新分配/拷贝一遍（十万量级），而这个函数会被 hLiveScanStatus
// （每个打开直播页的浏览器 tab 每 5 秒轮询一次）和后台扫描循环反复调用，
// 值得在这一层也短期缓存一下。返回的切片只会被只读遍历（扫描循环拿它派
// 发任务、hLiveScanStatus/hLivePlayable 拿它数数/筛选），不会被调用方修改，
// 所以不需要像 live.Channels 那样克隆隔离。
var (
	liveCandMu   sync.Mutex
	liveCandAt   time.Time
	liveCandData []model.Channel
)

const liveCandTTL = 15 * time.Second

// liveScanCandidates 返回「值得测」的全量频道：永远先过一遍 live.FilterJunk，
// 不看 store.LiveJunkFilterOn()——那个开关是前端「要不要显示」的口味问题，
// 这里剔除的是「时间戳/DJ 歌单这种本来就不可能是真实频道」的脏数据，不
// 浪费扫描/带宽在这些永远不会被当成正经频道用的条目上（合并后的全量列表
// 脏数据占比很高，不过滤的话 total 会膨胀到几十万条，扫一轮要扫到天荒
// 地老）。hLiveScanStatus 的进度统计也用这同一份候选集，保证「总数」跟
// 实际扫描的是同一回事。
func liveScanCandidates() []model.Channel {
	liveCandMu.Lock()
	if liveCandData != nil && time.Since(liveCandAt) < liveCandTTL {
		out := liveCandData
		liveCandMu.Unlock()
		return out
	}
	liveCandMu.Unlock()

	sources := liveAllSources()
	if len(sources) == 0 {
		return nil
	}
	// live.Channels 永远不等网络：直接用磁盘缓存拼结果，顺带在后台把
	// 过期/缺失的直播源缓存自己刷新一遍（refreshStale），这里不用额外
	// 操心直播源本身的抓取。
	groups, _ := live.Channels(sources, false)
	groups = live.FilterJunk(groups)
	var all []model.Channel
	for _, g := range groups {
		all = append(all, g.Channels...)
	}

	liveCandMu.Lock()
	liveCandData, liveCandAt = all, time.Now()
	liveCandMu.Unlock()
	return all
}

// hLiveScanStatus GET：「可播频道」分类用的扫描进度。total 是当前已知的
// 全部频道数，scanned 是其中「有测试结果且没过期」的数量，playable 是
// 其中确认能播的数量，scanning 表示后台扫描这会儿是不是正在跑。
func hLiveScanStatus(w http.ResponseWriter, r *http.Request) {
	all := liveScanCandidates()
	var scanned, playable int
	for _, c := range all {
		if alive, known := store.GetChannelStatus(c.Name); known {
			scanned++
			if alive {
				playable++
			}
		}
	}
	total := len(all)
	ok(w, map[string]any{
		"total":    total,
		"scanned":  scanned,
		"playable": playable,
		"scanning": liveScanning.Load(),
	})
}

// hLivePlayable GET：「可播频道」分类专用的轻量频道列表——只返回真正确认
// 能播的那一小撮（几千条量级），不像 /api/live/channels 那样把全部直播源
// 合并后的十几万条频道目录整个序列化下发（那个接口单次响应能有几十 MB，
// 「可播频道」视图完全用不到里面 95%+ 看不到的频道，之前前端是从那份全量
// 目录里用 status==='ok' 筛出来用的，白白传输+反序列化+转成 Vue 响应式
// 对象，是「停在直播页 CPU 一直很高」的主因之一）。
func hLivePlayable(w http.ResponseWriter, r *http.Request) {
	all := liveScanCandidates()
	out := make([]model.Channel, 0, 512)
	for _, c := range all {
		if alive, known := store.GetChannelStatus(c.Name); known && alive {
			c.Status = "ok"
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	ok(w, map[string]any{"channels": out})
}

// hLiveChannelStatusRemove POST {name}：用户手动把一个频道从「可播频道」
// 分类里移除——清掉它的测试记录而不是标记成「播不通」，下一轮后台扫描会
// 重新测一次，而不是直接认定它播不出来。
func hLiveChannelStatusRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := body(r, &req); err != nil || req.Name == "" {
		fail(w, http.StatusBadRequest, "参数错误")
		return
	}
	store.RemoveChannelStatus(req.Name)
	ok(w, map[string]any{"ok": true})
}

// hLiveScanReset POST：清空全部频道的测试记录，强制下一轮后台扫描从头
// 重新测一遍。探测方法本身改过之后（比如加强了验证逻辑）留着，给需要
// 「旧方法测出来的结果不可信，想让可播频道整个重新来一遍」的场景用——
// 正常不会在前端暴露成按钮，是运维/排障用的口子。
func hLiveScanReset(w http.ResponseWriter, r *http.Request) {
	store.ClearAllChannelStatus()
	ok(w, map[string]any{"ok": true})
}
