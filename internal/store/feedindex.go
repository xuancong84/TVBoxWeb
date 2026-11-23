package store

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/xxl6097/TVBoxWeb/internal/model"
)

// FeedIndexMax 每个排行指标最多保留多少条（去重后）。
const FeedIndexMax = 1000

// feedMetrics 是首页可按其排序的指标（键名同前端 FEED_SORTS 和 model.Vod 的
// JSON 字段名）。「站点排序」= 原始顺序，不需要索引，前端直接用 Feed 本身。
var feedMetrics = map[string]func(v *model.Vod) float64{
	"ts":        func(v *model.Vod) float64 { return float64(v.Time) },
	"hits":      func(v *model.Vod) float64 { return v.Hits },
	"hitsWeek":  func(v *model.Vod) float64 { return v.HitsWeek },
	"hitsMonth": func(v *model.Vod) float64 { return v.HitsMonth },
	"up":        func(v *model.Vod) float64 { return v.Up },
	"score":     func(v *model.Vod) float64 { return v.Score },
	"douban":    func(v *model.Vod) float64 { return v.Douban },
}

// BuildFeedIndex 在「首页列表 feed（原始顺序）+ 候选池 extra（各站源更深的
// 分页、没进首页的候选）」上，对每个指标算出降序排好的下标。下标指向
// 「feed followed by 返回的 pool」这一份拼接列表：列表只存一份，各指标只存下标。
//
//   - 某个站源只要在这个指标上一条有效值（>0）都没给过（feed+extra 里都没有），
//     就认为该站源不提供这个指标，它的影片不进这个指标的索引；提供了的站源，
//     其全部影片都进候选（值为 0 的排最后）。没有任何站源提供的指标不出现。
//   - 同一部片子（片名+年份，年份为空当通配）在多个站源重复出现时只保留指标
//     值最高的那一条（同值保留靠前的）。
//   - 每个指标最多 FeedIndexMax 条。
//   - 返回的 pool 只包含被某个指标用到的 extra 条目（已重新编号）。
func BuildFeedIndex(feed, extra []model.Vod) (map[string][]int, []model.Vod) {
	nf := len(feed)
	n := nf + len(extra)
	at := func(i int) *model.Vod {
		if i < nf {
			return &feed[i]
		}
		return &extra[i-nf]
	}
	out := map[string][]int{}
	usedExtra := map[int]bool{}
	for key, val := range feedMetrics {
		has := map[string]bool{}
		for i := 0; i < n; i++ {
			if v := at(i); val(v) > 0 {
				has[v.SiteKey] = true
			}
		}
		if len(has) == 0 {
			continue
		}
		cand := make([]int, 0, n)
		for i := 0; i < n; i++ {
			if has[at(i).SiteKey] {
				cand = append(cand, i)
			}
		}
		sort.SliceStable(cand, func(a, b int) bool { return val(at(cand[a])) > val(at(cand[b])) })
		seen, seenAny := map[string]bool{}, map[string]bool{}
		idx := make([]int, 0, FeedIndexMax)
		for _, i := range cand {
			v := at(i)
			k := v.Name + "|" + v.Year
			dup := seen[k] || (v.Year == "" && seenAny[v.Name]) || (v.Year != "" && seen[v.Name+"|"])
			if dup {
				continue
			}
			seen[k], seenAny[v.Name] = true, true
			idx = append(idx, i)
			if len(idx) >= FeedIndexMax {
				break
			}
		}
		out[key] = idx
		for _, i := range idx {
			if i >= nf {
				usedExtra[i-nf] = true
			}
		}
	}
	// 只保留被用到的候选，并把下标重新编号成紧凑的 pool 位置。
	keep := make([]int, 0, len(usedExtra))
	for j := range usedExtra {
		keep = append(keep, j)
	}
	sort.Ints(keep)
	remap := make(map[int]int, len(keep))
	pool := make([]model.Vod, 0, len(keep))
	for r, j := range keep {
		remap[nf+j] = nf + r
		pool = append(pool, extra[j])
	}
	for _, idx := range out {
		for p, i := range idx {
			if i >= nf {
				idx[p] = remap[i]
			}
		}
	}
	return out, pool
}

func feedIndexValid(idx map[string][]int, n int) bool {
	if idx == nil {
		return false
	}
	for _, l := range idx {
		for _, i := range l {
			if i < 0 || i >= n {
				return false
			}
		}
	}
	return true
}

// ---- 候选池（feedPool）：各指标榜单用到、但不在首页列表里的影片 ----
// 体积比 Feed 大不少，不放进 store.json（那份每次小改动都要整份重写），单独
// 一个 gzip 文件。丢了/损坏了大不了榜单暂时只有 Feed 里的条目，下一轮抓取补回。

const feedPoolFileName = "feedpool.json.gz"

var (
	feedPool     []model.Vod // 受 mu 保护
	feedPoolPath string
)

// InitFeedPool 在 store.Init 之后调用一次，加载磁盘上的榜单候选池。
func InitFeedPool(dataDir string) {
	p := filepath.Join(dataDir, feedPoolFileName)
	var list []model.Vod
	if f, err := os.Open(p); err == nil {
		defer f.Close()
		if gr, err := gzip.NewReader(f); err == nil {
			defer gr.Close()
			if b, err := io.ReadAll(gr); err == nil {
				if err := json.Unmarshal(b, &list); err != nil {
					log.Printf("[store] 榜单候选池解析失败，忽略: %v", err)
					list = nil
				}
			}
		}
	}
	mu.Lock()
	feedPoolPath = p
	feedPool = list
	mu.Unlock()
}

func writeFeedPool(path string, list []model.Vod) {
	if path == "" {
		return
	}
	b, err := json.Marshal(list)
	if err != nil {
		return
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(b); err != nil || gw.Close() != nil {
		return
	}
	if err := atomicWrite(path, buf.Bytes()); err != nil {
		log.Printf("[store] 榜单候选池落盘失败: %v", err)
	}
}

// FeedPool 返回榜单候选池（下标接在 Feed 后面，见 BuildFeedIndex）。
func FeedPool() []model.Vod { mu.RLock(); defer mu.RUnlock(); return feedPool }

// SaveFeedWithPool 同时更新首页列表和候选池，并重算各指标的排序下标。
func SaveFeedWithPool(list, extra []model.Vod) {
	idx, pool := BuildFeedIndex(list, extra)
	mu.Lock()
	data.Feed, data.FeedIdx, feedPool = list, idx, pool
	path := feedPoolPath
	mu.Unlock()
	writeFeedPool(path, pool)
	flush()
}

// SaveFeed 只换首页列表，候选池保持不变（重算下标）。
func SaveFeed(list []model.Vod) { SaveFeedWithPool(list, FeedPool()) }

// FilterFeedSites 只保留 keep 里站源的影片（首页列表和候选池都过滤）。
func FilterFeedSites(keep map[string]bool) {
	f := func(in []model.Vod) []model.Vod {
		var out []model.Vod
		for _, v := range in {
			if keep[v.SiteKey] {
				out = append(out, v)
			}
		}
		return out
	}
	SaveFeedWithPool(f(Feed()), f(FeedPool()))
}

// FeedIndex 返回各指标的排序下标；落盘里没有（旧版本数据）或跟列表对不上时
// 现算一次并缓存。
func FeedIndex() map[string][]int {
	mu.RLock()
	idx, n := data.FeedIdx, len(data.Feed)+len(feedPool)
	mu.RUnlock()
	if feedIndexValid(idx, n) {
		return idx
	}
	mu.Lock()
	defer mu.Unlock()
	idx, feedPool = BuildFeedIndex(data.Feed, feedPool)
	data.FeedIdx = idx
	return data.FeedIdx
}
