package live

import (
	"context"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/model"
	"github.com/xxl6097/TVBoxWeb/internal/store"
)

// CacheMaxAge 单个直播源缓存结果的最长可信年龄，跟 crawler.SiteCacheMaxAge
// 对点播站源的设计完全一致：没超龄直接用磁盘缓存，服务重启/首次打开直播页
// 不必把几十个源全部重新联网拉一遍。
const CacheMaxAge = 24 * time.Hour

const (
	liveConc         = 6
	liveFetchTimeout = 20 * time.Second
)

var refreshState struct {
	mu      sync.Mutex
	running bool
}

// mergeCache 缓存 buildFromCache 的合并结果：全部直播源加起来常常是十万量级
// 频道（见 FilterJunk 的注释——大量同一个台换着名字出现在不同源里，没法
// 被 normalizeChannelName 这种保守归一化合并掉），MergeGroups 每次都要整个
// 重新跑一遍 map 归并+投票，在频繁轮询（扫描进度 5 秒一次、频道列表静默
// 刷新）下会把 CPU 跑得老高。这份合并结果在 mergeCacheTTL 内直接复用，
// 不等 force=true/源缓存变化（容忍最多 TTL 这么久的滞后，跟这个 app 里
// 其他「缓存，不实时」的设计一致）。
var mergeCache struct {
	mu     sync.Mutex
	groups []model.ChannelGroup
	errs   map[string]string
	at     time.Time
}

const mergeCacheTTL = 20 * time.Second

// Channels 返回「当前已知最好」的合并频道目录：永远不等网络，直接用磁盘
// 缓存拼出结果（哪怕某些源的缓存已经过期甚至从没抓过都没关系），缺失/
// 过期的源交给后台异步刷新，这次调用立刻返回——用户点开直播页或点一下
// 「刷新」都不会被几十个源逐个超时卡住；新结果抓回来后，下一次打开/轮询
// 自然就能看到。force=true（用户手动点刷新）额外让没超龄的缓存也重新抓一遍，
// 同时跳过 mergeCache（保证这次返回的是刚刷新过的最新合并结果）。
//
// 返回值永远是独立的一份拷贝（cloneGroups）：调用方（postProcessLive 等）
// 会就地排序/打状态标记，不克隆的话多个并发请求会共享同一份底层数组，
// 边读边写是数据竞争。
func Channels(sources []model.RawLive, force bool) (groups []model.ChannelGroup, errs map[string]string) {
	if !force {
		mergeCache.mu.Lock()
		if !mergeCache.at.IsZero() && time.Since(mergeCache.at) < mergeCacheTTL {
			groups, errs = cloneGroups(mergeCache.groups), mergeCache.errs
			mergeCache.mu.Unlock()
			go refreshStale(sources, force)
			return
		}
		mergeCache.mu.Unlock()
	}

	fresh, errs := buildFromCache(sources)
	mergeCache.mu.Lock()
	mergeCache.groups, mergeCache.errs, mergeCache.at = fresh, errs, time.Now()
	mergeCache.mu.Unlock()
	go refreshStale(sources, force)
	return cloneGroups(fresh), errs
}

// cloneGroups 浅拷贝到「每个 Channel 切片都是独立底层数组」的深度：Channel
// 结构体本身的字段（含 URLs 这个 slice）合并之后不会再被修改，拷贝 slice
// 头、共享同一份只读的 URLs 底层数组是安全的；真正需要隔离的是 Channels
// 这一层（调用方会 sort.Slice / 原地改 Status）。
func cloneGroups(gs []model.ChannelGroup) []model.ChannelGroup {
	out := make([]model.ChannelGroup, len(gs))
	for i, g := range gs {
		out[i] = g
		out[i].Channels = append([]model.Channel(nil), g.Channels...)
	}
	return out
}

func buildFromCache(sources []model.RawLive) ([]model.ChannelGroup, map[string]string) {
	var all [][]model.ChannelGroup
	errs := map[string]string{}
	for _, s := range sources {
		name := s.Name
		if name == "" {
			name = s.URL
		}
		e, ok := store.GetLiveCacheEntry(s.URL)
		if !ok {
			errs[name] = "首次加载中，请稍候…"
			continue
		}
		if !e.OK {
			errs[name] = "上次抓取失败"
		}
		all = append(all, e.Groups)
	}
	return MergeGroups(all), errs
}

// refreshStale 真正联网刷新缺失/过期（或 force 时全部）的直播源，写回磁盘
// 缓存。同一时刻只跑一份：用户连续点刷新、连续切换到直播页，不会叠加出
// 好几份并发刷新互相抢资源。
func refreshStale(sources []model.RawLive, force bool) {
	refreshState.mu.Lock()
	if refreshState.running {
		refreshState.mu.Unlock()
		return
	}
	refreshState.running = true
	refreshState.mu.Unlock()
	defer func() {
		refreshState.mu.Lock()
		refreshState.running = false
		refreshState.mu.Unlock()
	}()

	keep := map[string]bool{}
	for _, s := range sources {
		keep[s.URL] = true
	}
	store.PruneLiveCache(keep)

	now := time.Now()
	var stale []model.RawLive
	for _, s := range sources {
		if s.URL == "" {
			continue
		}
		e, ok := store.GetLiveCacheEntry(s.URL)
		if !force && ok && now.Sub(time.UnixMilli(e.At)) < CacheMaxAge {
			continue
		}
		stale = append(stale, s)
	}
	if len(stale) == 0 {
		return
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, liveConc)
	for _, s := range stale {
		wg.Add(1)
		go func(s model.RawLive) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			gs, err := LoadCtx(context.Background(), s.URL, liveFetchTimeout)
			store.SetLiveCacheEntry(s.URL, store.LiveCacheEntry{
				At: time.Now().UnixMilli(), OK: err == nil, Groups: gs,
			})
		}(s)
	}
	wg.Wait()
	store.FlushLiveCache()
}

// ---------- 脏数据过滤 + 排序 ----------

// junkTimestamp 匹配「2026-08-27 14:06:22」这类纯时间戳：部分直播源会用
// 一行「更新时间」充当频道，被 m3u/txt 解析误当成真实频道收进来。
var junkTimestamp = regexp.MustCompile(`^\d{4}-\d{1,2}-\d{1,2}([ T]\d{1,2}:\d{2}(:\d{2})?)?$`)

// junkNameMaxLen 超过这个长度基本可以确定不是电视台名字（真实频道名极少
// 超过十几个字），而是源里混进来的 DJ 音乐合集/歌曲名之类的长标题。
const junkNameMaxLen = 30

func isJunkChannelName(name string) bool {
	if junkTimestamp.MatchString(name) {
		return true
	}
	if len([]rune(name)) > junkNameMaxLen {
		return true
	}
	return false
}

// FilterJunk 剔除明显不是频道的脏数据条目，过滤后变空的分组一并去掉。
func FilterJunk(groups []model.ChannelGroup) []model.ChannelGroup {
	out := make([]model.ChannelGroup, 0, len(groups))
	for _, g := range groups {
		chs := make([]model.Channel, 0, len(g.Channels))
		for _, c := range g.Channels {
			if !isJunkChannelName(c.Name) {
				chs = append(chs, c)
			}
		}
		if len(chs) == 0 {
			continue
		}
		g.Channels = chs
		out = append(out, g)
	}
	return out
}

// SortGroups 按名字给分组、分组内的频道都排一遍序（自然排序：数字部分按
// 数值比较，让 CCTV-2 排在 CCTV-10 前面），让顺序稳定、可预期——原来的
// 顺序是「并发抓取谁先返回谁在前」，每次重启/刷新都会洗一遍牌。
func SortGroups(groups []model.ChannelGroup) {
	sort.Slice(groups, func(i, j int) bool { return naturalLess(groups[i].Name, groups[j].Name) })
	for i := range groups {
		chs := groups[i].Channels
		sort.Slice(chs, func(a, b int) bool { return naturalLess(chs[a].Name, chs[b].Name) })
	}
}

// naturalLess 自然排序比较：把字符串切成「连续数字」/「非数字」片段交替
// 比较，数字片段按数值大小比较而不是逐字符比较，这样「CCTV-9」会排在
// 「CCTV-10」前面（逐字符比较会把 "10" 排在 "9" 前面，因为 '1' < '9'）。
func naturalLess(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	i, j := 0, 0
	for i < len(ra) && j < len(rb) {
		ca, cb := ra[i], rb[j]
		if isDigit(ca) && isDigit(cb) {
			si, ei := i, i
			for ei < len(ra) && isDigit(ra[ei]) {
				ei++
			}
			sj, ej := j, j
			for ej < len(rb) && isDigit(rb[ej]) {
				ej++
			}
			na, nb := string(ra[si:ei]), string(rb[sj:ej])
			na = trimLeadingZeros(na)
			nb = trimLeadingZeros(nb)
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			i, j = ei, ej
			continue
		}
		if ca != cb {
			return ca < cb
		}
		i++
		j++
	}
	return len(ra)-i < len(rb)-j
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func trimLeadingZeros(s string) string {
	i := 0
	for i < len(s)-1 && s[i] == '0' {
		i++
	}
	return s[i:]
}
