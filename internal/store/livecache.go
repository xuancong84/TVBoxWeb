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

	"github.com/xxl6097/TVBoxWeb/internal/model"
)

// LiveCacheEntry 是某个直播源最近一次拉取+解析的缓存结果：跟 SiteCrawlEntry
// 对直播源的对应物，命中它就不用重新联网拉一遍 m3u/txt。
type LiveCacheEntry struct {
	At     int64               `json:"at"` // 这份结果的抓取时间（毫秒）
	OK     bool                `json:"ok"`
	Groups []model.ChannelGroup `json:"groups"`
}

// liveCacheFileName 跟 sitecrawl_cache.json.gz 同样的理由分开存、用 gzip 压缩：
// 几十个直播源、每个源可能有成百上千个频道，不适合塞进 store.json。
const liveCacheFileName = "livecrawl_cache.json.gz"

var (
	liveCacheMu    sync.RWMutex
	liveCache      = map[string]LiveCacheEntry{}
	liveCachePath  string
	liveCacheDirty bool
	// channelStatus 记录每个直播频道「真实播放过一次」的结果：true=播过且能播，
	// false=播过但播不出来，key 不存在="没播过/不知道"。跟直播源缓存存在
	// 同一个文件里（见 liveCacheFile），没有过期时间——频道能不能播不会随
	// 时间推移自动变化，只会被下一次真实播放结果覆盖，所以没必要像
	// LiveCacheEntry 那样带时间戳判断新鲜度。
	channelStatus = map[string]bool{}
)

// liveCacheFile 是落盘的整体结构：直播源抓取缓存 + 频道播放状态，两者生命周期/
// 用途不同但都挂在同一个「直播相关、可丢弃的缓存」文件下，没必要拆两个文件。
type liveCacheFile struct {
	Sources       map[string]LiveCacheEntry `json:"sources"`
	ChannelStatus map[string]bool           `json:"channelStatus"`
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
	channelStatus = map[string]bool{}
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
			var cs map[string]bool
			if err := json.Unmarshal(rawStatus, &cs); err == nil {
				channelStatus = cs
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

// GetChannelStatus 取某个直播频道上次真实播放的结果；known=false 表示从没播过。
func GetChannelStatus(name string) (ok bool, known bool) {
	liveCacheMu.RLock()
	defer liveCacheMu.RUnlock()
	ok, known = channelStatus[name]
	return
}

// SetChannelStatus 记录一次真实播放结果，立即落盘（这类写入很稀疏——用户看了
// 哪个台才会触发一次，不像 SetLiveCacheEntry 那样一轮抓取就是几十次，没必要
// 攒起来等 FlushLiveCache 统一落盘）。
func SetChannelStatus(name string, ok bool) {
	liveCacheMu.Lock()
	channelStatus[name] = ok
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
	status := make(map[string]bool, len(channelStatus))
	for k, v := range channelStatus {
		status[k] = v
	}
	liveCacheMu.RUnlock()

	b, err := json.Marshal(liveCacheFile{Sources: sources, ChannelStatus: status})
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
