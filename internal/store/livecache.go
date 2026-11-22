package store

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/model"
)

// LiveCacheEntry 是某个直播源最近一次拉取+解析的缓存结果：跟 SiteCrawlEntry
// 对直播源的对应物，命中它就不用重新联网拉一遍 m3u/txt。
type LiveCacheEntry struct {
	At     int64                `json:"at"` // 这份结果的抓取时间（毫秒）
	OK     bool                 `json:"ok"`
	Groups []model.ChannelGroup `json:"groups"`
}

// liveCacheFileName 跟 sitecrawl_cache.json.gz 同样的理由分开存、用 gzip 压缩：
// 几十个直播源、每个源可能有成百上千个频道，不适合塞进 store.json。
const liveCacheFileName = "livecrawl_cache.json.gz"

// ChannelProbe 一个频道「测过一次」的快照：At 记录测试时间（毫秒），
// 用来判断 OK=false（播不通）这个结论是不是已经过期。OK=true 没有过期
// 这一说——频道只要被证实能播过一次（不管是用户真实播放成功，还是后台
// 扫描探测成功），就永久留在「可播频道」里，除非用户手动移除
// （RemoveChannelStatus）；只有「播不通」这个结论会随时间推移失去可信度，
// 需要 ChannelFailMaxAge 之后重新验证。
type ChannelProbe struct {
	OK bool  `json:"ok"`
	At int64 `json:"at"`
}

// ChannelFailMaxAge 「播不通」结论的最长可信期：超过这个岁数，后台扫描
// 会把这个频道当成「没测过」重新探测一次。只对 OK=false 生效，OK=true
// 永久有效。
const ChannelFailMaxAge = 24 * time.Hour

var (
	liveCacheMu    sync.RWMutex
	liveCache      = map[string]LiveCacheEntry{}
	liveCachePath  string
	liveCacheDirty bool
	// channelStatus 记录每个直播频道最近一次「测试」（真实播放 或 后台扫描
	// 探测）的结果，key 不存在="没测过/不知道"。跟直播源缓存存在同一个
	// 文件里（见 liveCacheFile）。
	channelStatus = map[string]ChannelProbe{}
)

// liveCacheFile 是落盘的整体结构：直播源抓取缓存 + 频道播放状态，两者生命周期/
// 用途不同但都挂在同一个「直播相关、可丢弃的缓存」文件下，没必要拆两个文件。
type liveCacheFile struct {
	Sources       map[string]LiveCacheEntry `json:"sources"`
	ChannelStatus map[string]ChannelProbe   `json:"channelStatus"`
	LineScores    map[string]LineScore      `json:"lineScores,omitempty"`
}

// InitLiveCrawlCache 在 store.Init 之后调用一次，加载磁盘上的直播源抓取缓存。
// 文件不存在/损坏都当成「空缓存」处理，不影响正常启动。
// 兼容老版本只存了 map[string]LiveCacheEntry（没有 sources/channelStatus 包一层）
// 的文件格式，避免升级后把已经抓好的缓存白白丢掉重抓一遍。
func InitLiveCrawlCache(dataDir string) {
	liveCacheMu.Lock()
	defer liveCacheMu.Unlock()
	liveCachePath = filepath.Join(dataDir, liveCacheFileName)
	liveCache = map[string]LiveCacheEntry{}
	channelStatus = map[string]ChannelProbe{}
	lineScores = map[string]LineScore{}
	liveCacheDirty = false

	f, err := os.Open(liveCachePath)
	if err != nil {
		return
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		log.Printf("[store] 直播源抓取缓存损坏，忽略（将重新抓取）: %v", err)
		return
	}
	defer gr.Close()
	b, err := io.ReadAll(gr)
	if err != nil {
		log.Printf("[store] 读取直播源抓取缓存失败: %v", err)
		return
	}

	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		log.Printf("[store] 解析直播源抓取缓存失败: %v", err)
		return
	}
	if rawSources, ok := probe["sources"]; ok {
		var sources map[string]LiveCacheEntry
		if err := json.Unmarshal(rawSources, &sources); err == nil {
			liveCache = sources
		}
		if rawStatus, ok := probe["channelStatus"]; ok {
			var cs map[string]ChannelProbe
			if err := json.Unmarshal(rawStatus, &cs); err == nil {
				channelStatus = cs
			} else {
				// 兼容升级前的老格式：channelStatus 当时是 map[string]bool，
				// 没有时间戳。老的 true（能播）直接当永久有效；老的 false
				// （播不通）当成「刚测过」处理，给它一份完整的 24 小时
				// 宽限期重新验证，而不是直接当成从没测过。
				var old map[string]bool
				if err2 := json.Unmarshal(rawStatus, &old); err2 == nil {
					now := time.Now().UnixMilli()
					cs = make(map[string]ChannelProbe, len(old))
					for k, v := range old {
						cs[k] = ChannelProbe{OK: v, At: now}
					}
					channelStatus = cs
				}
			}
		}
		if rawScores, ok := probe["lineScores"]; ok {
			var ls map[string]LineScore
			if err := json.Unmarshal(rawScores, &ls); err == nil {
				lineScores = ls
			}
		}
		return
	}
	// 老格式：整份文件本身就是 map[string]LiveCacheEntry，没有 sources 这一层包装。
	var m map[string]LiveCacheEntry
	if err := json.Unmarshal(b, &m); err != nil {
		log.Printf("[store] 解析直播源抓取缓存失败: %v", err)
		return
	}
	liveCache = m
}

// GetChannelStatus 取某个直播频道最近一次测试结果；known=false 表示
// 「从没测过」或者「测出来播不通，但已经超过 ChannelFailMaxAge，需要
// 当作没测过重新验证」——能播的结果（OK=true）不会因为这个过期判断
// 而失效。
func GetChannelStatus(name string) (ok bool, known bool) {
	liveCacheMu.RLock()
	defer liveCacheMu.RUnlock()
	p, exists := channelStatus[name]
	if !exists {
		return false, false
	}
	if !p.OK && time.Since(time.UnixMilli(p.At)) > ChannelFailMaxAge {
		return false, false
	}
	return p.OK, true
}

// SetChannelStatus 记录一次测试结果，立即落盘——给「稀疏」写入场景用
// （用户真实播放了一次，触发一条；不像后台批量扫描那样一轮就是几百上
// 千条，见 SetChannelStatusBulk）。
func SetChannelStatus(name string, ok bool) {
	liveCacheMu.Lock()
	channelStatus[name] = ChannelProbe{OK: ok, At: time.Now().UnixMilli()}
	liveCacheMu.Unlock()
	flushLiveCache()
}

// SetChannelStatusBulk 跟 SetChannelStatus 语义一致，但不立即落盘——后台
// 扫描一轮动辄测几百上千个频道，每测一个就落一次盘会把 SD 卡写爆，调用方
// （后台扫描循环）测完一整轮自己调 FlushLiveCache 统一落一次盘。
func SetChannelStatusBulk(name string, ok bool) {
	liveCacheMu.Lock()
	channelStatus[name] = ChannelProbe{OK: ok, At: time.Now().UnixMilli()}
	liveCacheDirty = true
	liveCacheMu.Unlock()
}

// RemoveChannelStatus 清掉一个频道的测试记录——用户手动把它从「可播频道」
// 分类里移除时调用。清掉之后它变回「没测过」，下一轮后台扫描会重新测一次，
// 不是直接把它标记成「播不通」（标记播不通会被 24 小时过期重新“洗白”，
// 跟用户主动移除的意图不一样）。
func RemoveChannelStatus(name string) {
	liveCacheMu.Lock()
	delete(channelStatus, name)
	liveCacheMu.Unlock()
	flushLiveCache()
}

// ClearAllChannelStatus 清空全部频道的测试记录，强制下一轮后台扫描把每个
// 频道都当成「没测过」重新测一遍——探测方法本身换过（比如从「看一眼响应头」
// 换成「真的连续拉流验证」）之后，旧方法测出来的结果不再可信，permanently
// 缓存的「能播」结论不会自己过期（只有「播不通」才有 24 小时有效期），
// 不整体清一次的话，旧方法误判出来的假阳性会一直赖在「可播频道」里。
func ClearAllChannelStatus() {
	liveCacheMu.Lock()
	channelStatus = map[string]ChannelProbe{}
	liveCacheMu.Unlock()
	flushLiveCache()
}

// GetLiveCacheEntry 取某个直播源上次缓存的抓取结果。
func GetLiveCacheEntry(url string) (LiveCacheEntry, bool) {
	liveCacheMu.RLock()
	defer liveCacheMu.RUnlock()
	e, ok := liveCache[url]
	return e, ok
}

// SetLiveCacheEntry 写入一个直播源的最新抓取结果。只更新内存，落盘由调用方
// 统一调 FlushLiveCache。
func SetLiveCacheEntry(url string, e LiveCacheEntry) {
	liveCacheMu.Lock()
	liveCache[url] = e
	liveCacheDirty = true
	liveCacheMu.Unlock()
}

// PruneLiveCache 删掉缓存里「现在已经不属于任何直播源」的条目。
func PruneLiveCache(keep map[string]bool) {
	liveCacheMu.Lock()
	defer liveCacheMu.Unlock()
	for k := range liveCache {
		if !keep[k] {
			delete(liveCache, k)
			liveCacheDirty = true
		}
	}
}

// FlushLiveCache 把直播源抓取缓存（连同频道播放状态）压缩落盘；
// 抓取缓存没有脏数据时直接跳过——一轮抓取结束才调一次，没必要每次都写。
func FlushLiveCache() {
	liveCacheMu.Lock()
	if !liveCacheDirty || liveCachePath == "" {
		liveCacheMu.Unlock()
		return
	}
	liveCacheDirty = false
	liveCacheMu.Unlock()
	writeLiveCacheFile()
}

// flushLiveCache 无条件落盘，不看 liveCacheDirty——给 SetChannelStatus 这种
// 稀疏写入用，不走「一轮抓取攒起来统一落盘」那套节流逻辑。
func flushLiveCache() {
	writeLiveCacheFile()
}

// writeLiveCacheFile 把当前内存里的直播源缓存 + 频道播放状态一起序列化压缩
// 落盘，两者共用一份文件（liveCacheFile）。
func writeLiveCacheFile() {
	liveCacheMu.RLock()
	path := liveCachePath
	if path == "" {
		liveCacheMu.RUnlock()
		return
	}
	sources := make(map[string]LiveCacheEntry, len(liveCache))
	for k, v := range liveCache {
		sources[k] = v
	}
	status := make(map[string]ChannelProbe, len(channelStatus))
	for k, v := range channelStatus {
		status[k] = v
	}
	scores := make(map[string]LineScore, len(lineScores))
	for k, v := range lineScores {
		scores[k] = v
	}
	liveCacheMu.RUnlock()

	b, err := json.Marshal(liveCacheFile{Sources: sources, ChannelStatus: status, LineScores: scores})
	if err != nil {
		log.Printf("[store] 直播源抓取缓存序列化失败: %v", err)
		return
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(b); err != nil {
		log.Printf("[store] 直播源抓取缓存压缩失败: %v", err)
		return
	}
	if err := gw.Close(); err != nil {
		log.Printf("[store] 直播源抓取缓存压缩失败: %v", err)
		return
	}
	if err := atomicWrite(path, buf.Bytes()); err != nil {
		log.Printf("[store] 直播源抓取缓存落盘失败: %v", err)
	}
}
