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

// SiteRankMax 每个站源、每个指标最多保留多少条。
const SiteRankMax = 150

// SiteRank 是某个站源在某个指标上自己排好序的榜单（降序，只含该指标值>0的影片）。
// 排序只在站源内部进行：各站源的计数口径、量级完全不同（有的 0~1000 随机数，
// 有的上万），跨站直接比大小没有意义。多站合并（轮流各取一条）由前端按需做。
type SiteRank struct {
	Site string `json:"s"` // SiteKey
	Idx  []int  `json:"i"` // 下标，指向「feed followed by pool」
}

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
// 分页、没进首页的候选）」上，对每个指标、每个站源各自算出降序排好的下标。
// 下标指向「feed followed by 返回的 pool」这一份拼接列表：列表只存一份。
//
//   - 站源在某指标上没有任何有效值（>0）就不提供该指标，不出现在该指标里；
//     提供了的站源也只收值>0 的影片（0 视为没有数据）。
//   - 同一站源内同名同年份的影片只留值最高的一条；跨站去重留给合并时做。
//   - 每个站源每个指标最多 SiteRankMax 条；站源顺序按其在 feed 中首次出现的先后。
//   - 返回的 pool 只包含被用到的 extra 条目（已重新编号）。
func BuildFeedIndex(feed, extra []model.Vod) (map[string][]SiteRank, []model.Vod) {
	nf := len(feed)
	n := nf + len(extra)
	at := func(i int) *model.Vod {
		if i < nf {
			return &feed[i]
		}
		return &extra[i-nf]
	}
	var siteOrder []string
	seenSite := map[string]bool{}
	for i := 0; i < n; i++ {
		if k := at(i).SiteKey; !seenSite[k] {
			seenSite[k] = true
			siteOrder = append(siteOrder, k)
		}
	}
	bySite := map[string][]int{}
	for i := 0; i < n; i++ {
		k := at(i).SiteKey
		bySite[k] = append(bySite[k], i)
	}
	out := map[string][]SiteRank{}
	usedExtra := map[int]bool{}
	for key, val := range feedMetrics {
		var ranks []SiteRank
		for _, site := range siteOrder {
			var cand []int
			for _, i := range bySite[site] {
				if val(at(i)) > 0 {
					cand = append(cand, i)
				}
			}
			if len(cand) == 0 {
				continue
			}
			sort.SliceStable(cand, func(a, b int) bool { return val(at(cand[a])) > val(at(cand[b])) })
			seen := map[string]bool{}
			idx := make([]int, 0, SiteRankMax)
			for _, i := range cand {
				v := at(i)
				k := v.Name + "|" + v.Year
				if seen[k] {
					continue
				}
				seen[k] = true
				idx = append(idx, i)
				if len(idx) >= SiteRankMax {
					break
				}
			}
			ranks = append(ranks, SiteRank{Site: site, Idx: idx})
			for _, i := range idx {
				if i >= nf {
					usedExtra[i-nf] = true
				}
			}
		}
		if len(ranks) > 0 {
			out[key] = ranks
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
	for _, ranks := range out {
		for _, sr := range ranks {
			for p, i := range sr.Idx {
				if i >= nf {
					sr.Idx[p] = remap[i]
				}
			}
		}
	}
	return out, pool
}

func feedIndexValid(idx map[string][]SiteRank, n int) bool {
	if idx == nil {
		return false
	}
	for _, l := range idx {
		for _, sr := range l {
			for _, i := range sr.Idx {
				if i < 0 || i >= n {
					return false
				}
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
func FeedIndex() map[string][]SiteRank {
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
