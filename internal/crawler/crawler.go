// Package crawler 实现定时抓取（远程配置刷新 + 逐站抓最新），与鸿蒙影视 Crawler.ets 一致。
package crawler

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/cms"
	"github.com/xxl6097/TVBoxWeb/internal/httpc"
	"github.com/xxl6097/TVBoxWeb/internal/model"
	"github.com/xxl6097/TVBoxWeb/internal/store"
)

var (
	errNoSites = errors.New("配置里没有 sites 列表")
	errInvalid = errors.New("订阅不是合法 JSON")
	errNoData  = errors.New("订阅里既没有站点也没有直播源")
)

func fetchText(source string) (string, error) {
	return fetchTextCtx(context.Background(), source)
}

func fetchTextCtx(ctx context.Context, source string) (string, error) {
	src := strings.TrimSpace(source)
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		return httpc.TextCtx(ctx, src, nil, 20*time.Second)
	}
	if src != "" {
		return src, nil
	}
	text := store.CachedText()
	if text == "" {
		return "", errors.New("还没有配置站源")
	}
	return text, nil
}

func parseConfig(text string) (model.RawConfig, error) {
	cleaned := cms.CleanJSON(text)
	var cfg model.RawConfig
	if err := json.Unmarshal([]byte(cleaned), &cfg); err != nil {
		return cfg, errors.New("配置不是合法 JSON，请确认地址返回的是 TVBox 配置")
	}
	return cfg, nil
}

func trim(s string) string { return strings.TrimSpace(s) }

func hasHTTP(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

const (
	// CrawlInterval 两次「全站抓取」之间的最短间隔：抓取是预先在后台按这个
	// 周期跑的（见 tvbox.go startScheduler），不是用户一点首页/点一下
	// 「更新」就现抓现等。用户在这个间隔内点「更新」只是看看而已，不会
	// 真的再去敲一遍全站——这里改成一个可调参数，按需调大调小即可。
	CrawlInterval = 24 * time.Hour
	// SiteCacheMaxAge 单个站源「抓列表」缓存结果的最长可信年龄：没超过这个
	// 岁数就直接用 store.SiteCrawlEntry 缓存，不重新联网抓。跟 CrawlInterval
	// 是两个不同的旋钮——CrawlInterval 决定「这一轮该不该跑」，这个决定
	// 「跑的时候这一个站源要不要真的联网」。这样无论是定时轮询还是服务
	// 重启触发的那一轮，绝大多数没过期的站源都是缓存秒出，只有真正过期
	// （或从没抓过）的那一小撮才会被放进这一轮要联网的子集。
	SiteCacheMaxAge = 24 * time.Hour
	// SitePool 每个站源实际抓取/缓存的候选条数——组装首页时按站源分组，
	// 组内保留这个候选池原始顺序（即该站自己返回的新旧/热度顺序），轮转
	// 组装时去重撞车了就换这个池子里的下一条候选继续凑。站源自己一页
	// 通常也就返回二三十条，这个数再大也没有更多候选可留，纯粹是上限。
	SitePool = 32
	// siteCacheVer 站源缓存条目的结构版本：2 = 带排行指标（时间/评分/点击/点赞）。
	siteCacheVer = 3
	// DeepPages 给「提供排行指标」的站源额外往后抓到第几页（含第 1 页）：各指标榜单
	// 的候选池。按「页号在外层、站源在内层」的顺序抓——先把所有站源的第 2 页
	// 抓完，再抓所有站源的第 3 页……这样同一个站源两次请求之间天然隔着一整轮。
	DeepPages = 10
	// DeepRate 深层分页抓取的全局限速（相邻两次请求的最小间隔）；DeepRoundMin 是
	// 每一页轮次的最短耗时（站源很少时防止对同一站源连续猛敲）。
	DeepRate     = 80 * time.Millisecond
	DeepRoundMin = 1500 * time.Millisecond
	// FeedMax 首页「最近更新」最终的总条数上限（即需求里的 totalN），
	// 默认 720 = 6!。组装方式见 Run() 里的「按站源热度排序后轮转取样」：
	// 不再是「站源 A 一次拿够 8 条再换站源 B」，而是把所有贡献了候选的
	// 站源按最近一次抓到的影片总数从多到少排一遍，一圈圈
	// 轮着从每个站源的候选池里各取一条最新/最热门且还没出现过的，直到
	// 凑满这个数或者所有站源的候选池都被轮空为止——这样热门大站不会一次
	// 性占满配额，中小站源也能在轮转中占到位置，首页观感更均匀。
	FeedMax = 720
	Conc    = 4
)

type CrawlResult struct {
	At         int64  `json:"at"`
	Sites      int    `json:"sites"`
	Alive      int    `json:"alive"`
	Dead       int    `json:"dead"`
	Items      int    `json:"items"`
	CfgUpdated bool   `json:"cfgUpdated"`
	OK         bool   `json:"ok"`
	Msg        string `json:"msg"`
}

var (
	mu                sync.Mutex
	running           bool
	progressProcessed int
	progressTotal     int
)

func IsRunning() bool {
	mu.Lock()
	defer mu.Unlock()
	return running
}

// Progress 返回抓取进度（已处理数/总数/是否运行中）。
//
// 命名返回值不能叫 running：它会在函数体内遮蔽同名的包级变量 running，
// 导致 `return ..., running` 返回的其实是这个命名返回值自己（零值
// false），而不是包级变量的真实值——于是这个函数会一直报告「没在跑」，
// 哪怕抓取正在进行中。GET /api/crawl、首页的抓取进度轮询都靠这个值
// 判断要不要继续等，一直是错的。
func Progress() (processed, total int, isRunning bool) {
	mu.Lock()
	defer mu.Unlock()
	return progressProcessed, progressTotal, running
}

// Run 立即跑一轮，不受 CrawlInterval 间隔限制（间隔限制在调用方：见
// MaybeRun 和 api.hCrawl）。force 目前未使用，保留是为了跟 MaybeRun()
// 调 Run(false) 的调用形式对应。
func Run(force bool) CrawlResult {
	mu.Lock()
	if running {
		mu.Unlock()
		return CrawlResult{OK: false, Msg: "正在更新中，请稍候"}
	}
	running = true
	mu.Unlock()
	defer func() {
		mu.Lock()
		running = false
		mu.Unlock()
	}()

	res := CrawlResult{}

	// 1) 远程配置刷新：老的单一「配置」导入（/api/config，站源专用、不走订阅列表）。
	// 只在「一条订阅都没有」时才生效——它内部直接 store.BuildSites() 整体覆盖
	// 站源列表，一旦订阅列表非空，这个覆盖会把订阅合并出来的结果冲掉（哪怕
	// 只是短暂地，下面 RefreshSubscriptions 跑完又会把它覆盖回来，但这段
	// 窗口期间 /api/status 等接口会看到错误的站源数）。
	if store.IsCustom() && len(store.Subscriptions()) == 0 {
		if u := store.GetConfigURL(); u != "" {
			if _, err := loadConfig(u); err == nil {
				res.CfgUpdated = true
			}
		}
	}
	if RefreshSubscriptions(context.Background()) > 0 {
		res.CfgUpdated = true
	}

	// 2) 逐站抓最新——先用没过期的缓存垫上，只有过期（或从没抓过）的站源
	// 才会放进 staleSites 真正联网，见 SiteCacheMaxAge 的注释。
	sites := store.ActiveSites()
	res.Sites = len(sites)
	if len(sites) == 0 {
		res.Msg = "还没有站源，请先在「设置 → 播放源 → 订阅」导入"
		return res
	}

	// 缓存里「现在已经不是任何现役站源」的条目清掉，避免缓存文件随着历史
	// 出现过的站源无限增长（保留全量 Sites() 而不是只保留 ActiveSites()，
	// 这样临时关掉某个站源再重新打开时，它的缓存不会被白白清掉）。
	keepKeys := map[string]bool{}
	for _, s := range store.Sites() {
		keepKeys[s.Key] = true
	}
	store.PruneSiteCrawlCache(keepKeys)

	// 初始化进度
	mu.Lock()
	progressTotal = len(sites)
	progressProcessed = 0
	mu.Unlock()
	defer func() {
		mu.Lock()
		progressProcessed = progressTotal
		mu.Unlock()
	}()

	var bag []model.Vod
	var dead []string
	var aliveCount int
	var bagMu sync.Mutex

	apply := func(s model.Site, items []model.Vod) {
		bagMu.Lock()
		if len(items) > 0 {
			bag = append(bag, items...)
			aliveCount++
		} else {
			dead = append(dead, s.Key)
		}
		bagMu.Unlock()
		mu.Lock()
		progressProcessed++
		mu.Unlock()
	}

	now := time.Now()
	var staleSites []model.Site
	for _, s := range sites {
		entry, ok := store.GetSiteCrawlEntry(s.Key)
		if ok && entry.V >= siteCacheVer && now.Sub(time.UnixMilli(entry.At)) < SiteCacheMaxAge {
			apply(s, entry.Items)
			continue
		}
		staleSites = append(staleSites, s)
	}

	var cursor int
	var cursorMu sync.Mutex
	var deepSites []deepJob
	var deepMu sync.Mutex
	worker := func() {
		for {
			cursorMu.Lock()
			if cursor >= len(staleSites) {
				cursorMu.Unlock()
				return
			}
			i := cursor
			cursor++
			cursorMu.Unlock()

			s := staleSites[i]
			var items []model.Vod
			total, pages := 0, 0
			ok := false
			c := cms.New(s)
			if pg, err := c.List("", 1); err == nil {
				ok = true
				total = pg.Total
				pages = pg.PageCount
				for k, v := range pg.Items {
					if k >= SitePool {
						break
					}
					if v.ID == "" || v.Name == "" {
						continue
					}
					items = append(items, slimVod(v, s))
				}
				if total <= 0 {
					total = len(pg.Items) // 有些站不报总数，退化成用本页条数
				}
			}
			// 供点播页下拉排序：ok=true 但 total=0 是「连上了确实空」，
			// ok=false 是这次压根没连上（超时/DNS失败/HTTP错误/解析失败
			// 都算），两者严重程度不同，具体排序规则见 api.siteSeverity。
			store.RecordSiteCrawl(s.Key, ok, total)
			// 落进本地缓存，带上这次抓取的时间戳：下一轮（哪怕是服务重启
			// 触发的那一轮）只要没超过 SiteCacheMaxAge 就不用再联网。
			store.SetSiteCrawlEntry(s.Key, store.SiteCrawlEntry{
				At: time.Now().UnixMilli(), OK: ok, Total: total, Items: items, V: siteCacheVer, Pages: pages,
			})
			if ok && pages > 1 && hasRankMetric(items) {
				deepMu.Lock()
				deepSites = append(deepSites, deepJob{site: s, pages: pages})
				deepMu.Unlock()
			}
			apply(s, items)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < Conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker()
		}()
	}
	wg.Wait()
	// 提供排行指标的站源再往后抓几页，给各指标榜单凑足候选（见 DeepPages）。
	fetchDeepPages(deepSites)
	// 这一轮真正联网抓到的站源结果统一落一次盘（gzip 压缩），而不是
	// 每抓完一个站源就落盘一次——几千个站源逐条落盘太费 IO。
	store.FlushSiteCrawlCache()

	// 3) 按片名去重 + 按站源热度轮转组装，凑够 FeedMax（totalN）条为止。
	// 同一部片子不同镜像站经常给出相同片名但年份一个有一个空（有的站根本没解析
	// 出年份字段），按「片名+年份」严格匹配会把它们误判成两部不同的片子，
	// 漏掉重复。把空年份当「通配」处理：某个片名只要已经出现过（不管那次
	// 有没有年份），这次不管有没有年份都算重复；片名相同但两次都各自给了
	// 不同的具体年份（比如翻拍），才当成两部不同的片子保留。
	seen := map[string]bool{}        // 片名|年份 精确命中
	seenAnyYear := map[string]bool{} // 片名（不论年份）已经出现过
	bySite := map[string][]model.Vod{}
	var siteOrder []string
	for _, v := range bag {
		if _, ok := bySite[v.SiteKey]; !ok {
			siteOrder = append(siteOrder, v.SiteKey)
		}
		bySite[v.SiteKey] = append(bySite[v.SiteKey], v)
	}
	// 按站源最近一次统计到的影片总数从多到少排——「已经按片量从多到少
	// 排好的站源列表」。siteOrder 原来是谁先抓完就排谁，跟热度无关；这里
	// 换成热度序，轮转组装时大站不会抢跑占满名额，小站也能在一圈圈轮转
	// 里占到位置。注意片量不是 model.Site.Count（那个字段在 siteList 原
	// 始副本上一直是零值，只有 hSites 这个 HTTP handler 会在响应里临时拿
	// SiteHealthMap 的 LastTotal 现填进去，store.Sites() 这里拿到的是没填
	// 过的原始值）——真正的片量在 store.SiteHealthMap()[key].LastTotal。
	health := store.SiteHealthMap()
	siteCount := map[string]int{}
	for _, s := range store.Sites() {
		siteCount[s.Key] = health[s.Key].LastTotal
	}
	sort.SliceStable(siteOrder, func(i, j int) bool {
		return siteCount[siteOrder[i]] > siteCount[siteOrder[j]]
	})
	// 轮转组装：一圈圈按热度序扫过所有站源，每个站源每一圈最多出一条——
	// 该站候选池里排在 poolIdx 之后、且去重没撞车的第一条（即该站自己给的
	// 顺序里最新/最热门的下一条）。一圈下来哪个站源一条都没能新增，说明
	// 所有站源的候选池都已经耗尽或剩下的全是重复，直接收工，不再空转。
	poolIdx := map[string]int{}
	var feed []model.Vod
	for len(feed) < FeedMax {
		progressed := false
		for _, key := range siteOrder {
			list := bySite[key]
			i := poolIdx[key]
			for i < len(list) {
				v := list[i]
				i++
				k := v.Name + "|" + v.Year
				dup := seen[k]
				if !dup && v.Year == "" && seenAnyYear[v.Name] {
					dup = true
				}
				if !dup && v.Year != "" && seen[v.Name+"|"] {
					dup = true
				}
				if dup {
					continue
				}
				seen[k] = true
				seenAnyYear[v.Name] = true
				feed = append(feed, v)
				progressed = true
				break
			}
			poolIdx[key] = i
			if len(feed) >= FeedMax {
				break
			}
		}
		if !progressed {
			break
		}
	}

	// sites 是这一轮开始时的快照：一整轮（几千个站源，内部还可能要跳板
	// 重试）可能跑好几分钟，这期间用户完全可能在「设置」里停用了某条
	// 单仓源/站源——这会立即触发 RebuildFromSubscriptions 把 Feed 里对应
	// 的条目过滤掉（见该函数注释），但如果这里无脑拿 sites 快照算出来的
	// 结果整体覆盖 Feed，等于把刚被过滤掉的内容又原样塞回去，表现成
	// 「明明停用了，首页却还在」。落盘前按「此刻最新」的启用集合再筛一遍。
	activeNow := map[string]bool{}
	for _, s := range store.ActiveSites() {
		activeNow[s.Key] = true
	}
	feed = filterVodBySite(feed, activeNow)
	dead = filterKeys(dead, activeNow)
	extra := collectPoolCandidates(feed, activeNow)

	res.Alive = aliveCount
	res.Dead = len(dead)
	res.Items = len(feed)
	res.At = time.Now().UnixMilli()
	res.OK = len(feed) > 0
	if !res.OK {
		res.Msg = "这一轮没有抓到数据，可能是网络不通或站源集体失效"
	}

	store.SaveFeedWithPool(feed, extra)
	store.SaveDeadKeys(dead)
	store.SetLastCrawlAt(res.At)
	store.SaveCrawlSummary(SummaryOf(res))
	return res
}

func filterVodBySite(list []model.Vod, keep map[string]bool) []model.Vod {
	var out []model.Vod
	for _, v := range list {
		if keep[v.SiteKey] {
			out = append(out, v)
		}
	}
	return out
}

func filterKeys(keys []string, keep map[string]bool) []string {
	var out []string
	for _, k := range keys {
		if keep[k] {
			out = append(out, k)
		}
	}
	return out
}

// DueForRefresh 距上次「全站抓取」是否已经超过 CrawlInterval（或者压根没
// 抓过 / 订阅内容变过触发了 ResetCrawlAt）。给 API 层用来决定「这次点击要不
// 要真的去后台起一轮抓取」，自己不会去跑抓取、不会加锁，调用方自己决定
// 要不要配合 IsRunning() 一起判断。
func DueForRefresh() bool {
	last := store.LastCrawlAt()
	return last == 0 || time.Now().UnixMilli()-last >= CrawlInterval.Milliseconds()
}

// MaybeRun 到期才跑；没到期返回 nil。
func MaybeRun() *CrawlResult {
	if IsRunning() {
		return nil
	}
	if !DueForRefresh() {
		return nil
	}
	r := Run(false)
	return &r
}

// loadConfig 拉取并载入远程配置。
func loadConfig(source string) (int, error) {
	return LoadConfig(source)
}

// LoadConfig 供 api 包复用：载入配置（URL 或 JSON 文本）。
func LoadConfig(source string) (int, error) {
	return LoadConfigCtx(context.Background(), source)
}

// LoadConfigCtx 与 LoadConfig 相同，ctx 可取消。
func LoadConfigCtx(ctx context.Context, source string) (int, error) {
	text, err := fetchConfigText(ctx, source)
	if err != nil {
		return 0, err
	}
	cfg, err := parseConfig(text)
	if err != nil {
		return 0, err
	}
	if len(cfg.Sites) == 0 {
		return 0, errNoSites
	}
	store.BuildSites(cfg)
	store.BuildParses(cfg)
	store.SetCfg(text, source)
	store.ResetCrawlAt()
	return store.TotalSites(), nil
}

// fetchConfigText / fetchSubscriptionText 都是「拉取 + 必要时按地区限制跳板重试」的
// 统一入口：先直接拉取，拉到的内容（或其中嵌入的 base64 块，参见 geoblock.go 里
// extractUsableConfigText 的注释）能解出站点/直播源就直接用；拉不到可用内容、且
// source 是一个 http(s) 地址时，再依次尝试 SSH_CMDS_GEO_BLOCK 里配置的跳板。
func fetchConfigText(ctx context.Context, source string) (string, error) {
	return fetchWithGeoFallback(ctx, source)
}

func fetchSubscriptionText(ctx context.Context, source string) (string, error) {
	return fetchWithGeoFallback(ctx, source)
}

func fetchWithGeoFallback(ctx context.Context, source string) (string, error) {
	src := strings.TrimSpace(source)
	text, ferr := fetchTextCtx(ctx, src)
	if ferr == nil {
		if usable, ok := extractUsableConfigText(text); ok {
			return usable, nil
		}
	}
	if hasHTTP(src) && worthGeoRelay(ferr) {
		if relayed, rerr := fetchViaGeoBlockRelays(ctx, src); rerr == nil {
			return relayed, nil
		}
	}
	if ferr != nil {
		return "", ferr
	}
	// 直接请求成功，但内容解不出站点/直播源，又没有可用跳板：原样交回，
	// 让上层 parseConfig 走正常的「不是合法配置」报错路径。
	return text, nil
}

// subIDFor 用来源算一个稳定 ID：同一个地址（或同一段粘贴文本）再次导入时
// 原地更新而不是重复添加。
func subIDFor(source string) string {
	h := sha1.Sum([]byte(strings.TrimSpace(source)))
	return hex.EncodeToString(h[:])[:16]
}

// dedupKey 决定一个新条目该用什么 key/name：key 是第一次出现、或与之前
// 「完全相同」（uniqueVal 也相同）时直接复用；key 冲突但 uniqueVal 不同
// （比如两份订阅都用了同一个 key 但指向不同的 api 地址），改名为
// key_2/key_3/… 而不是互相覆盖，这样同一部剧在多个订阅里的不同播放源
// 都会保留下来，一个失效还有别的能用。完全重复的条目（key 和 uniqueVal
// 都一样）才会被丢弃。
func dedupKey(seen map[string]string, want, uniqueVal string) (key string, keep bool) {
	if prev, ok := seen[want]; !ok {
		seen[want] = uniqueVal
		return want, true
	} else if prev == uniqueVal {
		return "", false
	}
	for n := 2; ; n++ {
		cand := want + "_" + itoa(n)
		prev, ok := seen[cand]
		if !ok {
			seen[cand] = uniqueVal
			return cand, true
		}
		if prev == uniqueVal {
			return "", false
		}
	}
}

// mergeConfigs 把多份订阅的 sites/parses/lives 合并成一份，规则见 dedupKey。
func mergeConfigs(cfgs []model.RawConfig) model.RawConfig {
	var out model.RawConfig
	seenSites := map[string]string{}
	for _, cfg := range cfgs {
		for i, s := range cfg.Sites {
			key := s.Key
			if key == "" {
				key = s.Name
			}
			if key == "" {
				key = "site_" + itoa(i)
			}
			newKey, keep := dedupKey(seenSites, key, s.API)
			if !keep {
				continue
			}
			s.Key = newKey
			out.Sites = append(out.Sites, s)
		}
	}
	seenLives := map[string]bool{}
	for _, cfg := range cfgs {
		for _, l := range cfg.Lives {
			u := trim(l.URL)
			if u == "" || seenLives[u] {
				continue
			}
			seenLives[u] = true
			out.Lives = append(out.Lives, l)
		}
	}
	seenParses := map[string]string{}
	for _, cfg := range cfgs {
		for i, p := range cfg.Parses {
			name := p.Name
			if name == "" {
				name = "parse_" + itoa(i)
			}
			newName, keep := dedupKey(seenParses, name, p.URL)
			if !keep {
				continue
			}
			p.Name = newName
			out.Parses = append(out.Parses, p)
		}
	}
	for _, cfg := range cfgs {
		if cfg.Spider != "" && out.Spider == "" {
			out.Spider = cfg.Spider
		}
	}
	return out
}

// RebuildFromSubscriptions 用当前所有「已启用」订阅缓存的配置文本重建合并后的
// 站源/解析规则/直播源列表。添加、删除、启停某条订阅之后都要调用一次。
func RebuildFromSubscriptions() store.SubResult {
	subs := store.Subscriptions()
	var cfgs []model.RawConfig
	var names []string
	for _, s := range subs {
		if !s.Enabled || s.Text == "" {
			continue
		}
		cfg, err := parseConfig(s.Text)
		if err != nil {
			continue
		}
		cfgs = append(cfgs, cfg)
		name := cfg.Name
		if name == "" {
			name = s.Name
		}
		if name != "" {
			names = append(names, name)
		}
	}
	merged := mergeConfigs(cfgs)
	store.BuildSites(merged)
	store.BuildParses(merged)
	store.ResetCrawlAt()
	// 只摘掉「站源已经不在了」的旧 Feed 条目（避免点进详情报「站源不存在」），
	// 其余原样保留——不能把整个首页「最近更新」一次性清空干等：新一轮逐站
	// 抓取要跑完（几百个站、分钟级）才会有新数据，这中间空着的这段时间，
	// 首页就是空的，用户在这段时间点进首页只会看到「暂无内容」，怎么等都
	// 不会自己恢复（除非凑巧那一轮抓完）。保留旧条目，新一轮抓完后
	// Run() 末尾的 store.SaveFeed(feed) 自然会把它们换成新结果。
	keepSite := map[string]bool{}
	for _, s := range store.Sites() {
		keepSite[s.Key] = true
	}
	store.FilterFeedSites(keepSite)

	mergedName := strings.Join(names, " + ")
	store.SetSubName(mergedName)

	var lives []model.RawLive
	for i, l := range merged.Lives {
		u := trim(l.URL)
		if hasHTTP(u) {
			name := l.Name
			if name == "" {
				name = "直播源" + itoa(i+1)
			}
			lives = append(lives, model.RawLive{Name: name, URL: u})
		}
	}
	store.SetSubLives(lives)
	if len(lives) > 0 {
		cur := store.LiveURL()
		inList := false
		for _, l := range lives {
			if l.URL == cur {
				inList = true
			}
		}
		if !inList {
			store.SetLiveURL(lives[0].URL)
		}
	}
	return store.SubResult{Name: mergedName, Sites: store.TotalSites(), Lives: len(lives)}
}

// SiteLiveProvenance 扫一遍所有「已启用」订阅缓存的配置文本，分别按站源
// API 地址、直播源 URL 建出「这个地址来自哪些单仓源（展开后的叶子订阅）」
// 的反查表——站源/直播源合并去重时会丢掉这层信息（mergeConfigs 只留最终
// 合并结果），要查「这个点播源/直播源是哪条单仓源带来的」只能现查。
// 现查而不是缓存：订阅数量通常只有几十条，一次全扫是毫秒级，没必要为了
// 这个纯展示用途多维护一份状态。
func SiteLiveProvenance() (byAPI map[string][]model.SubRef, byLiveURL map[string][]model.SubRef) {
	byAPI = map[string][]model.SubRef{}
	byLiveURL = map[string][]model.SubRef{}
	for _, s := range store.Subscriptions() {
		if !s.Enabled || s.Text == "" {
			continue
		}
		cfg, err := parseConfig(s.Text)
		if err != nil {
			continue
		}
		name := cfg.Name
		if name == "" {
			name = s.Name
		}
		ref := model.SubRef{Name: name, Source: s.Source}
		for _, site := range cfg.Sites {
			if site.API == "" {
				continue
			}
			byAPI[site.API] = append(byAPI[site.API], ref)
		}
		for _, l := range cfg.Lives {
			u := trim(l.URL)
			if u == "" {
				continue
			}
			byLiveURL[u] = append(byLiveURL[u], ref)
		}
	}
	return
}

// RefreshSubscriptions 周期性刷新：重新拉取每条「来源是 URL」的订阅
// （纯文本粘贴的订阅没有「刷新」的意义，跳过）；某条刷新失败只记录错误、
// 保留它上次缓存的数据不动，不影响其它订阅，返回内容真的变了的条数——
// 「成功抓到」不等于「变了」：绝大多数订阅每次刷新拉到的内容跟上次一模
// 一样，如果拿「成功抓到」计数，几乎每次都会被判定成「变了」，进而触发
// RebuildFromSubscriptions 重建一遍站源列表，没必要。
func RefreshSubscriptions(ctx context.Context) int {
	subs := store.Subscriptions()
	changed := 0
	for _, s := range subs {
		if !s.Enabled || !hasHTTP(strings.TrimSpace(s.Source)) {
			continue
		}
		text, err := fetchSubscriptionText(ctx, s.Source)
		if err != nil {
			s.Err = err.Error()
			store.UpsertSubscription(s)
			continue
		}
		cfg, err := parseConfig(text)
		if err != nil || (len(cfg.Sites) == 0 && len(cfg.Lives) == 0) {
			s.Err = "刷新后的内容不是合法配置"
			store.UpsertSubscription(s)
			continue
		}
		textChanged := text != s.Text
		s.Text = text
		if cfg.Name != "" {
			s.Name = cfg.Name
		}
		s.Sites = len(cfg.Sites)
		s.Lives = len(cfg.Lives)
		s.Parses = len(cfg.Parses)
		s.UpdatedAt = time.Now().UnixMilli()
		s.Err = ""
		store.UpsertSubscription(s)
		if textChanged {
			changed++
		}
	}
	if changed > 0 {
		RebuildFromSubscriptions()
	}
	return changed
}

// LoadSubscription 订阅：一份 JSON 同时带点播站源与直播源。
func LoadSubscription(source string) (store.SubResult, error) {
	return LoadSubscriptionCtx(context.Background(), source)
}

// aggregatorEntry/aggregatorConfig 对应「多订阅聚合清单」格式：
// {"urls":[{"url":"...","name":"..."}, ...]}。这种文件本身不是 TVBox
// 配置（没有 sites/lives），只是给人工挑选用的菜单（比如 tvboxmuti.json），
// 真正能用的站源在它列出的每个地址里。
type aggregatorEntry struct {
	URL  string `json:"url"`
	Name string `json:"name"`
}

type aggregatorConfig struct {
	Urls []aggregatorEntry `json:"urls"`
}

// parseAggregator 尝试把文本当「聚合清单」解析；不是这种格式（没有 urls
// 字段，或根本不是合法 JSON）就返回 ok=false。
func parseAggregator(text string) (aggregatorConfig, bool) {
	cleaned := cms.CleanJSON(text)
	var agg aggregatorConfig
	if err := json.Unmarshal([]byte(cleaned), &agg); err != nil {
		return agg, false
	}
	return agg, len(agg.Urls) > 0
}

// LoadSubscriptionCtx 与 LoadSubscription 相同，ctx 可取消。
// 不再是「导入一份就整体替换」：这里只是把这一条订阅加入（或原地更新）
// 订阅列表，然后用所有已启用订阅重新合并出完整的站源/直播源列表，
// 所以可以同时订阅多个来源（比如一个聚合站 + 一个单独的地区限制站）。
//
// 如果这个地址本身是「聚合清单」（只有 urls 菜单，没有 sites/lives，比如
// noimank/tvbox 的 tvboxmuti.json），就展开清单里的每个地址分别当一条
// 订阅导入——清单本身不保存，单个地址失败不影响其它地址，全部失败才报错。
func LoadSubscriptionCtx(ctx context.Context, source string) (store.SubResult, error) {
	source = strings.TrimSpace(source)
	text, err := fetchSubscriptionText(ctx, source)
	if err != nil {
		return store.SubResult{}, err
	}
	cfg, cerr := parseConfig(text)
	if cerr == nil && (len(cfg.Sites) > 0 || len(cfg.Lives) > 0) {
		store.UpsertSubscription(store.Subscription{
			ID: subIDFor(source), Name: cfg.Name, Source: source, Enabled: true,
			Text: text, Sites: len(cfg.Sites), Lives: len(cfg.Lives), Parses: len(cfg.Parses),
			UpdatedAt: time.Now().UnixMilli(),
		})
		return RebuildFromSubscriptions(), nil
	}
	if agg, ok := parseAggregator(text); ok {
		visited := map[string]bool{source: true}
		imported, failed := 0, 0
		for _, e := range agg.Urls {
			n, f := importAggregatorEntry(ctx, e.URL, e.Name, visited, 1)
			imported += n
			failed += f
		}
		if imported == 0 {
			return store.SubResult{}, fmt.Errorf("聚合清单里的 %d 个地址都导入失败，可能都存在地区限制或已失效", failed)
		}
		return RebuildFromSubscriptions(), nil
	}
	if cerr != nil {
		return store.SubResult{}, errInvalid
	}
	return store.SubResult{}, errNoData
}

// importAggregatorEntry 抓取聚合清单里的一个地址并当作一条订阅导入（best
// effort：失败只计数，不中断其它地址）；如果这个地址本身又是一份聚合清单
// （嵌套菜单），递归展开，depth 限制嵌套层数防止死循环。相同地址在同一次
// 展开里只处理一次（visited），避免多份清单互相引用导致重复抓取。
func importAggregatorEntry(ctx context.Context, url, hintName string, visited map[string]bool, depth int) (imported, failed int) {
	u := strings.TrimSpace(url)
	if u == "" || visited[u] {
		return 0, 0
	}
	visited[u] = true
	if depth > 3 {
		return 0, 1
	}
	text, err := fetchSubscriptionText(ctx, u)
	if err != nil {
		return 0, 1
	}
	cfg, cerr := parseConfig(text)
	if cerr == nil && (len(cfg.Sites) > 0 || len(cfg.Lives) > 0) {
		name := cfg.Name
		if name == "" {
			name = hintName // 没有自带名字就用聚合清单里给它标的名字
		}
		store.UpsertSubscription(store.Subscription{
			ID: subIDFor(u), Name: name, Source: u, Enabled: true,
			Text: text, Sites: len(cfg.Sites), Lives: len(cfg.Lives), Parses: len(cfg.Parses),
			UpdatedAt: time.Now().UnixMilli(),
		})
		return 1, 0
	}
	if agg, ok := parseAggregator(text); ok {
		for _, e := range agg.Urls {
			n, f := importAggregatorEntry(ctx, e.URL, e.Name, visited, depth+1)
			imported += n
			failed += f
		}
		return imported, failed
	}
	return 0, 1
}

// RootSyncResult 是 SyncSubscriptionRoots 的返回结果：展开出的叶子订阅
// 数量、相对上一次新增/删除了多少条、以及每个无法解析的地址的失败原因。
type RootSyncResult struct {
	store.SubResult
	Leaves  int               `json:"leaves"`
	Added   int               `json:"added"`
	Removed int               `json:"removed"`
	Errs    map[string]string `json:"errs,omitempty"`
}

type resolvedLeaf struct {
	url  string
	name string
	text string
	cfg  model.RawConfig
}

// leafCollector 并发递归展开时的共享状态，所有字段都要加锁访问。
type leafCollector struct {
	mu      sync.Mutex
	leaves  []resolvedLeaf
	errs    map[string]string
	visited map[string]bool
	sem     chan struct{} // 限制同时在飞的抓取数量，避免瞬间打出一堆连接
}

func (c *leafCollector) tryVisit(u string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.visited[u] {
		return false
	}
	c.visited[u] = true
	return true
}

func (c *leafCollector) addLeaf(l resolvedLeaf) {
	c.mu.Lock()
	c.leaves = append(c.leaves, l)
	c.mu.Unlock()
}

func (c *leafCollector) addErr(u, msg string) {
	c.mu.Lock()
	c.errs[u] = msg
	c.mu.Unlock()
}

// resolve 递归展开一个地址：是普通订阅配置就当叶子收下；是聚合清单（只有
// urls 菜单）就继续展开它列出的每个地址（并发，深度限制 3 层防死循环，
// 同一个地址在本次展开里只处理一次）；两者都不是就记一笔失败原因。
func (c *leafCollector) resolve(ctx context.Context, wg *sync.WaitGroup, url, hintName string, depth int) {
	defer wg.Done()
	u := strings.TrimSpace(url)
	if u == "" || !c.tryVisit(u) {
		return
	}
	if depth > 3 {
		c.addErr(u, "聚合清单嵌套层数过多")
		return
	}
	c.sem <- struct{}{}
	text, err := fetchSubscriptionText(ctx, u)
	<-c.sem
	if err != nil {
		c.addErr(u, err.Error())
		return
	}
	cfg, cerr := parseConfig(text)
	if cerr == nil && (len(cfg.Sites) > 0 || len(cfg.Lives) > 0) {
		name := cfg.Name
		if name == "" {
			name = hintName
		}
		c.addLeaf(resolvedLeaf{url: u, name: name, text: text, cfg: cfg})
		return
	}
	if agg, ok := parseAggregator(text); ok {
		for _, e := range agg.Urls {
			wg.Add(1)
			go c.resolve(ctx, wg, e.URL, e.Name, depth+1)
		}
		return
	}
	c.addErr(u, "既不是合法配置也不是聚合清单")
}

// SyncSubscriptionRoots 把订阅文本框里的「根地址」列表（一行一个，可以是
// 普通订阅地址，也可以是聚合清单地址）递归展开、按叶子（单链接）地址去重后
// 同步进订阅列表：
//   - 新出现的叶子新增订阅（默认启用）；
//   - 之前就有、这次还在的叶子只刷新缓存内容，保留用户手动设的启用/停用状态；
//   - 之前有、这次展开不出来的叶子删除（说明已经从某个聚合清单里移除，或
//     用户从文本框里删掉了对应的根地址）。
//
// 根地址本身（含中间层聚合清单）不会被当成订阅保存，只有原始根地址列表存进
// store.SubscriptionRoots，下次点「保存」会重新整个展开一遍——聚合清单上游
// 的内容是会变的，这正是「保存」要解决的问题。
func SyncSubscriptionRoots(ctx context.Context, roots []string) (RootSyncResult, error) {
	seen := map[string]bool{}
	var cleanRoots []string
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		cleanRoots = append(cleanRoots, r)
	}
	store.SetSubscriptionRoots(cleanRoots)
	if len(cleanRoots) == 0 {
		for _, s := range store.Subscriptions() {
			store.RemoveSubscription(s.ID)
		}
		res := RebuildFromSubscriptions()
		return RootSyncResult{SubResult: res}, nil
	}

	c := &leafCollector{errs: map[string]string{}, visited: map[string]bool{}, sem: make(chan struct{}, 16)}
	var wg sync.WaitGroup
	for _, r := range cleanRoots {
		wg.Add(1)
		go c.resolve(ctx, &wg, r, "", 0)
	}
	wg.Wait()

	byURL := map[string]resolvedLeaf{}
	var order []string
	for _, l := range c.leaves {
		if _, ok := byURL[l.url]; !ok {
			order = append(order, l.url)
		}
		byURL[l.url] = l
	}
	if len(order) == 0 {
		return RootSyncResult{}, fmt.Errorf("没有展开出任何可用的站源/直播源，检查地址是否正确")
	}

	existing := store.Subscriptions()
	existingByURL := map[string]store.Subscription{}
	for _, s := range existing {
		existingByURL[s.Source] = s
	}

	added := 0
	for _, u := range order {
		l := byURL[u]
		name := l.name
		enabled := true
		if old, ok := existingByURL[u]; ok {
			enabled = old.Enabled
			// 这次展开没给出名字（比如文本框里直接写的是叶子地址本身，不是
			// 带名字菜单的聚合清单），但这个叶子之前已经有名字——保留它，
			// 不要让「保存」把人认得出来的名字清空成「未命名订阅」。
			if name == "" {
				name = old.Name
			}
		} else {
			added++
		}
		store.UpsertSubscription(store.Subscription{
			ID: subIDFor(u), Name: name, Source: u, Enabled: enabled,
			Text: l.text, Sites: len(l.cfg.Sites), Lives: len(l.cfg.Lives), Parses: len(l.cfg.Parses),
			UpdatedAt: time.Now().UnixMilli(),
		})
	}
	removed := 0
	for _, s := range existing {
		if _, ok := byURL[s.Source]; !ok {
			store.RemoveSubscription(s.ID)
			removed++
		}
	}

	res := RebuildFromSubscriptions()
	return RootSyncResult{SubResult: res, Leaves: len(order), Added: added, Removed: removed, Errs: c.errs}, nil
}

// Restore 启动时用缓存恢复站点列表：优先用订阅列表合并重建；如果订阅列表
// 是空的但老版本留下的单一配置缓存还在（升级前只支持一条订阅/配置），
// 就把它迁移成订阅列表里的第一条，这样升级后已导入的站源不会丢。
func Restore() bool {
	migrateLegacySubscription()
	if len(store.Subscriptions()) > 0 {
		RebuildFromSubscriptions()
		if store.TotalSites() > 0 || len(store.SubLives()) > 0 {
			return true
		}
	}
	// 兼容走「配置」(/api/config，站源专用、不含直播源) 导入的老数据。
	text := store.CachedText()
	if text == "" {
		return false
	}
	cfg, err := parseConfig(text)
	if err != nil || len(cfg.Sites) == 0 {
		return false
	}
	store.BuildSites(cfg)
	store.BuildParses(cfg)
	return true
}

// migrateLegacySubscription 把升级前「单一订阅」缓存的配置文本搬进新的订阅
// 列表，只在订阅列表还是空的时候做一次。
func migrateLegacySubscription() {
	if len(store.Subscriptions()) > 0 {
		return
	}
	text := store.CachedText()
	if text == "" {
		return
	}
	cfg, err := parseConfig(text)
	if err != nil || (len(cfg.Sites) == 0 && len(cfg.Lives) == 0) {
		return
	}
	source := store.GetConfigURL()
	if source == "" {
		source = text // 原本是直接粘贴的 JSON 文本，没有单独的地址
	}
	name := cfg.Name
	if name == "" {
		name = store.SubName()
	}
	store.UpsertSubscription(store.Subscription{
		ID: subIDFor(source), Name: name, Source: source, Enabled: true,
		Text: text, Sites: len(cfg.Sites), Lives: len(cfg.Lives), Parses: len(cfg.Parses),
		UpdatedAt: time.Now().UnixMilli(),
	})
}

func SummaryOf(r CrawlResult) string {
	t := fmtTime(r.At)
	if !r.OK {
		return t + " 更新失败：" + r.Msg
	}
	s := t + " 更新完成 · " + itoa(r.Alive) + "/" + itoa(r.Sites) + " 个站源有响应 · 收到 " + itoa(r.Items) + " 条"
	if r.Dead > 0 {
		s += " · " + itoa(r.Dead) + " 个无响应"
	}
	if r.CfgUpdated {
		s += " · 配置已刷新"
	}
	return s
}

func fmtTime(ms int64) string {
	if ms <= 0 {
		return "还没更新过"
	}
	t := time.UnixMilli(ms)
	return t.Format("01-02 15:04")
}

// slimVod 把抓到的完整影片压成「缓存/首页用」的小结构：From/PlayURL 整份不留
// （太占地方），总集数这个小整数先从完整数据里算出来存着。
func slimVod(v model.Vod, s model.Site) model.Vod {
	return model.Vod{
		ID: v.ID, Name: v.Name, Pic: v.Pic, Remark: v.Remark,
		Year: v.Year, Area: v.Area, TypeName: v.TypeName,
		SiteKey: s.Key, SiteName: s.Name,
		EpCount: cms.EpCount(v.From, v.PlayURL),
		Time:    v.Time, Score: v.Score, Douban: v.Douban,
		Hits: v.Hits, HitsWeek: v.HitsWeek, HitsMonth: v.HitsMonth, Up: v.Up,
	}
}

// hasRankMetric 站源这一页里有没有任何一条带有效（>0）的排行指标——没有就说明
// 这个站源不提供这些数据，往后翻页也是白抓。（更新时间不算：它每个站源都有，
// 而且往后翻页只会更旧。）
func hasRankMetric(items []model.Vod) bool {
	for _, v := range items {
		if v.Hits > 0 || v.HitsWeek > 0 || v.HitsMonth > 0 || v.Up > 0 || v.Score > 0 || v.Douban > 0 {
			return true
		}
	}
	return false
}

type deepJob struct {
	site  model.Site
	pages int
}

// fetchDeepPages 对 jobs 里的站源抓第 2..DeepPages 页。外层循环是页号、内层
// 是站源（并发 + 全局限速）：同一个站源的两次请求之间隔着其余所有站源的同
// 一页，不会对单个站源连续猛敲。某站源出错 / 翻到头 / 这页没有新条目（有些站
// 忽略 pg 参数一直返回同一页）就不再往后翻它。结果写回站源缓存的 Deep 字段。
func fetchDeepPages(jobs []deepJob) {
	if len(jobs) == 0 {
		return
	}
	type state struct {
		deepJob
		items []model.Vod
		seen  map[string]bool
		done  bool
	}
	states := make([]*state, len(jobs))
	for i, j := range jobs {
		states[i] = &state{deepJob: j, seen: map[string]bool{}}
		if e, ok := store.GetSiteCrawlEntry(j.site.Key); ok {
			for _, v := range e.Items {
				states[i].seen[v.ID] = true
			}
		}
	}
	tick := time.NewTicker(DeepRate)
	defer tick.Stop()
	for p := 2; p <= DeepPages; p++ {
		var active []*state
		for _, st := range states {
			if !st.done && p <= st.pages {
				active = append(active, st)
			}
		}
		if len(active) == 0 {
			break
		}
		start := time.Now()
		var cur int
		var curMu sync.Mutex
		var wg sync.WaitGroup
		for w := 0; w < Conc*2; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					curMu.Lock()
					if cur >= len(active) {
						curMu.Unlock()
						return
					}
					st := active[cur]
					cur++
					<-tick.C // 全局限速：相邻两次请求至少隔 DeepRate
					curMu.Unlock()
					pg, err := cms.New(st.site).List("", p)
					fresh := 0
					if err == nil {
						for _, v := range pg.Items {
							if v.ID == "" || v.Name == "" || st.seen[v.ID] {
								continue
							}
							st.seen[v.ID] = true
							st.items = append(st.items, slimVod(v, st.site))
							fresh++
						}
					}
					if err != nil || fresh == 0 {
						st.done = true
					}
				}
			}()
		}
		wg.Wait()
		if d := time.Since(start); d < DeepRoundMin {
			time.Sleep(DeepRoundMin - d)
		}
	}
	for _, st := range states {
		if e, ok := store.GetSiteCrawlEntry(st.site.Key); ok {
			e.Deep = st.items
			store.SetSiteCrawlEntry(st.site.Key, e)
		}
	}
}

// collectPoolCandidates 把所有现役站源缓存里（第 1 页 + 深层分页）不在首页列表
// 里的影片收集成各指标榜单的候选池（最终由 store.BuildFeedIndex 筛出真正
// 入榜的那部分再落盘）。
func collectPoolCandidates(feed []model.Vod, active map[string]bool) []model.Vod {
	inFeed := map[string]bool{}
	for _, v := range feed {
		inFeed[v.SiteKey+"\x00"+v.ID] = true
	}
	var out []model.Vod
	for key := range active {
		e, ok := store.GetSiteCrawlEntry(key)
		if !ok {
			continue
		}
		for _, list := range [][]model.Vod{e.Items, e.Deep} {
			for _, v := range list {
				k := v.SiteKey + "\x00" + v.ID
				if inFeed[k] {
					continue
				}
				inFeed[k] = true
				out = append(out, v)
			}
		}
	}
	return out
}
