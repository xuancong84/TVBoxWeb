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

// SiteCrawlEntry 是某个站源最近一次「抓列表」的缓存结果：命中它就不用
// 重新联网抓一遍——重启/定时轮询不必把几千个站源全部重新敲一遍，只有
// 缓存已经过期（见 crawler.SiteCacheMaxAge）的站源才需要真的联网。
type SiteCrawlEntry struct {
	At    int64       `json:"at"` // 这份结果的抓取时间（毫秒）
	OK    bool        `json:"ok"`
	Total int         `json:"total"`
	Items []model.Vod `json:"items"`
}

// siteCacheFileName 跟 store.json 分开存的原因：条目数跟站源总数（几千）
// 同量级，每条还带最多 SitePool 条影片，塞进 store.json 会让它体积暴涨、
// 任何一个小改动（比如切个开关）都要整份重新序列化落盘；这份数据本质上
// 是可丢弃的缓存（丢了大不了重新抓一遍），拆成单独文件、用 gzip 压缩
//（JSON 高度重复，压缩比很高）更合适。
const siteCacheFileName = "sitecrawl_cache.json.gz"

var (
	siteCacheMu    sync.RWMutex
	siteCache      = map[string]SiteCrawlEntry{}
	siteCachePath  string
	siteCacheDirty bool
)

// InitSiteCrawlCache 在 store.Init 之后调用一次，加载磁盘上的站源抓取缓存。
// 文件不存在/损坏都当成「空缓存」处理（等同于所有站源都要重新抓一遍），
// 不影响正常启动。
func InitSiteCrawlCache(dataDir string) {
	siteCacheMu.Lock()
	defer siteCacheMu.Unlock()
	siteCachePath = filepath.Join(dataDir, siteCacheFileName)
	siteCache = map[string]SiteCrawlEntry{}
	siteCacheDirty = false

	f, err := os.Open(siteCachePath)
	if err != nil {
		return
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		log.Printf("[store] 站源抓取缓存损坏，忽略（将重新抓取）: %v", err)
		return
	}
	defer gr.Close()
	b, err := io.ReadAll(gr)
	if err != nil {
		log.Printf("[store] 读取站源抓取缓存失败: %v", err)
		return
	}
	m := map[string]SiteCrawlEntry{}
	if err := json.Unmarshal(b, &m); err != nil {
		log.Printf("[store] 解析站源抓取缓存失败: %v", err)
		return
	}
	siteCache = m
}

// GetSiteCrawlEntry 取某个站源上次缓存的抓取结果。
func GetSiteCrawlEntry(key string) (SiteCrawlEntry, bool) {
	siteCacheMu.RLock()
	defer siteCacheMu.RUnlock()
	e, ok := siteCache[key]
	return e, ok
}

// SetSiteCrawlEntry 写入一个站源的最新抓取结果。只更新内存，不立即落盘——
// 一轮抓取动辄几千个站源，逐条落盘太费 IO，由调用方在一轮结束后统一调用
// FlushSiteCrawlCache 落一次盘。
func SetSiteCrawlEntry(key string, e SiteCrawlEntry) {
	siteCacheMu.Lock()
	siteCache[key] = e
	siteCacheDirty = true
	siteCacheMu.Unlock()
}

// PruneSiteCrawlCache 删掉缓存里「现在已经不属于任何站源」的条目（订阅被
// 删掉/站源被上游移除），避免缓存文件随着历史出现过的站源无限增长。
func PruneSiteCrawlCache(keep map[string]bool) {
	siteCacheMu.Lock()
	defer siteCacheMu.Unlock()
	for k := range siteCache {
		if !keep[k] {
			delete(siteCache, k)
			siteCacheDirty = true
		}
	}
}

// FlushSiteCrawlCache 把站源抓取缓存压缩落盘；没有脏数据时直接跳过。
func FlushSiteCrawlCache() {
	siteCacheMu.Lock()
	if !siteCacheDirty || siteCachePath == "" {
		siteCacheMu.Unlock()
		return
	}
	m := make(map[string]SiteCrawlEntry, len(siteCache))
	for k, v := range siteCache {
		m[k] = v
	}
	siteCacheDirty = false
	siteCacheMu.Unlock()

	b, err := json.Marshal(m)
	if err != nil {
		log.Printf("[store] 站源抓取缓存序列化失败: %v", err)
		return
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(b); err != nil {
		log.Printf("[store] 站源抓取缓存压缩失败: %v", err)
		return
	}
	if err := gw.Close(); err != nil {
		log.Printf("[store] 站源抓取缓存压缩失败: %v", err)
		return
	}
	if err := atomicWrite(siteCachePath, buf.Bytes()); err != nil {
		log.Printf("[store] 站源抓取缓存落盘失败: %v", err)
	}
}
