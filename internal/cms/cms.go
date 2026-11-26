// Package cms 实现苹果CMS（采集源）协议客户端，与鸿蒙影视 Cms.ets 一致。
// 只实现 TVBox 的「协议型」站源：type 0（XML）、type 1（JSON）、type 4（data 包一层）。
package cms

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"github.com/xxl6097/TVBoxWeb/internal/sysconf"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/httpc"
	"github.com/xxl6097/TVBoxWeb/internal/model"
)

// ---------- 响应缓存 ----------

// cmHit 字段导出（而非原来的小写私有字段）是为了能被 json.Marshal 落盘——
// 见下面「磁盘持久化」一节。
type cmHit struct {
	Key  string    `json:"key"`
	Body string    `json:"body"`
	At   time.Time `json:"at"`
}

const (
	// hitMax/hitChars：缓存条数/总字节上限。以前只按「最近几分钟」的量
	// 留了 30 条、3MB，够用；现在默认缓存一整天，同一天里会被真正摸到
	// 的站源/分类/分页/搜索关键词组合远不止 30 个，上限太小的话缓存刚
	// 存进去就被挤掉，等于形同虚设——按一天的使用量放宽到 5000 条、
	// 80MB（对树莓派这种设备也完全不算事）。
	hitMax   = 5000
	hitChars = 80 * 1000 * 1000
	// ttlDefault 点播源/单仓源的分类、片单、搜索这三类「拉全量列表」的
	// 请求默认缓存一天——这几类是最容易被频繁触发的（翻页、切分类、
	// 敲关键词搜索），之前只缓存几十秒到几分钟，相当于没怎么缓存，
	// 站源那边看到的是「同一个 IP 短时间内一直在高频扫全站」，正是最
	// 容易被封的访问模式。
	// ttlDetail 详情接口单独给一个短得多的缓存期：它是唯一一个响应里带
	// 的播放地址（PlayURL）真的会被拿去播放的接口（片单/搜索里附带的
	// playUrl 只用来在卡片上数集数，从不会被直接播放——点播放一定会重新
	// 走一次详情接口），不少站源的播放地址又是带签名/时效的，缓存太久
	// 会把已经失效的播放地址发给用户，表现成「点了播不出来」。详情接口
	// 本身也是「点开这一部才会拉一次」，不是翻页那种高频扫描，被封的风险
	// 小得多，所以这里没有跟着默认值走到一天，留一小时做个折中。
	// （ttlDetail 即系统设置 ca.cms_detail）
)

func ttlCat() time.Duration    { return sysconf.Dur("ca.cms") }
func ttlList() time.Duration   { return sysconf.Dur("ca.cms") }
func ttlDetail() time.Duration { return sysconf.Dur("ca.cms_detail") }

var (
	cacheMu    sync.Mutex
	hits       []cmHit
	cacheDirty bool
	cachePath  string
)

func hit(url string, ttl time.Duration) string {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	for i := range hits {
		if hits[i].Key == url {
			if time.Since(hits[i].At) <= ttl {
				return hits[i].Body
			}
			hits = append(hits[:i], hits[i+1:]...)
			cacheDirty = true
			return ""
		}
	}
	return ""
}

func keep(url, body string) {
	if body == "" {
		return
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	e := cmHit{Key: url, Body: body, At: time.Now()}
	cacheDirty = true
	for i := range hits {
		if hits[i].Key == url {
			hits[i] = e
			return
		}
	}
	hits = append(hits, e)
	for len(hits) > hitMax {
		hits = hits[1:]
	}
	total := 0
	for _, h := range hits {
		total += len(h.Body)
	}
	for total > hitChars && len(hits) > 1 {
		total -= len(hits[0].Body)
		hits = hits[1:]
	}
}

// ClearCache 用户换了配置/站源后主动清空。立即落盘（而不是等下一次定时
// flush），避免「清空后进程还没来得及 flush 就被杀掉」时，磁盘上那份
// 清空前的旧缓存在下次启动时被重新加载回来，等于白清。
func ClearCache() {
	cacheMu.Lock()
	hits = nil
	cacheDirty = true
	cacheMu.Unlock()
	FlushDiskCache()
}

// ---------- 磁盘持久化 ----------
//
// hits 原来只存在内存里：进程一重启（哪怕只是换个小版本重新部署），当天
// 已经攒下来的分类/片单/搜索缓存就全部清零，对每个站源，重启后第一次被
// 用户摸到都要重新联网拉一次——分类列表这种几乎不会变的数据尤其可惜。
// 这里把它落盘一份，跟站源抓取缓存（sitecrawl_cache）放在同一个 data
// 目录、同样用 gzip 压缩（JSON 高度重复，压缩比很高）。是否过期完全
// 复用 hit() 里已有的 ttl 比较，不需要在落盘结构里额外记一份 ttl。
const cacheFileName = "cms_cache.json.gz"

// InitDiskCache 在 store.Init 之后调用一次，加载磁盘上的响应缓存。
// 文件不存在/损坏都当成「空缓存」处理，不影响正常启动。
func InitDiskCache(dataDir string) {
	cacheMu.Lock()
	cachePath = sysconf.CacheFile("sites", cacheFileName, filepath.Join(dataDir, cacheFileName))
	cacheMu.Unlock()

	f, err := os.Open(cachePath)
	if err != nil {
		return
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		log.Printf("[cms] 响应缓存损坏，忽略（将重新抓取）: %v", err)
		return
	}
	defer gr.Close()
	b, err := io.ReadAll(gr)
	if err != nil {
		log.Printf("[cms] 读取响应缓存失败: %v", err)
		return
	}
	var loaded []cmHit
	if err := json.Unmarshal(b, &loaded); err != nil {
		log.Printf("[cms] 解析响应缓存失败: %v", err)
		return
	}
	cacheMu.Lock()
	hits = loaded
	cacheMu.Unlock()
}

// FlushDiskCache 把响应缓存压缩落盘；没有脏数据/尚未初始化路径时直接跳过。
// 由定时器周期调用（见 tvbox.go），不等到进程退出才存——树莓派这类设备
// 随时可能断电，不是每次都能走到优雅关闭。
func FlushDiskCache() {
	cacheMu.Lock()
	if !cacheDirty || cachePath == "" {
		cacheMu.Unlock()
		return
	}
	snapshot := make([]cmHit, len(hits))
	copy(snapshot, hits)
	cacheDirty = false
	path := cachePath
	cacheMu.Unlock()

	b, err := json.Marshal(snapshot)
	if err != nil {
		log.Printf("[cms] 响应缓存序列化失败: %v", err)
		return
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(b); err != nil {
		log.Printf("[cms] 响应缓存压缩失败: %v", err)
		return
	}
	if err := gw.Close(); err != nil {
		log.Printf("[cms] 响应缓存压缩失败: %v", err)
		return
	}
	if err := atomicWriteCache(path, buf.Bytes()); err != nil {
		log.Printf("[cms] 响应缓存落盘失败: %v", err)
	}
}

// atomicWriteCache 写临时文件后原子 rename；缓存本身可丢弃（丢了大不了
// 重新抓一遍），不需要 store 包那份 atomicWrite 额外维护的 .bak 备份。
func atomicWriteCache(dst string, b []byte) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cms-cache-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// ---------- CMS 客户端 ----------

type Cms struct {
	site  model.Site
	extra string // 筛选条件尾巴，如 &area=大陆&year=2025
}

func New(site model.Site) *Cms { return &Cms{site: site} }

func (c *Cms) SetExtra(q string) { c.extra = q }

func fetch(url string, ttl time.Duration, ms time.Duration) (string, error) {
	return fetchCtx(context.Background(), url, ttl, ms)
}

// fetchCtx 带 ctx 的抓取：缓存命中不发请求；未命中时 ctx 取消会立刻中止上游请求。
func fetchCtx(ctx context.Context, url string, ttl time.Duration, ms time.Duration) (string, error) {
	if h := hit(url, ttl); h != "" {
		return h, nil
	}
	txt, err := httpc.TextCtx(ctx, url, nil, ms)
	if err != nil {
		// 直连失败（典型：站源 API 对本机出口 IP 地区封锁）：尝试 SSH_CMDS_GEO_BLOCK
		// 跳板兜底。只用于 CMS 的 JSON/XML 接口响应（小体积），视频数据从不走这里。
		if rt, rerr := relayFetch(ctx, url, err); rerr == nil {
			keep(url, rt)
			return rt, nil
		}
		return "", err
	}
	keep(url, txt)
	return txt, nil
}

// RelayFetch / RelayWorthy 由 crawler 包在 init 里注入（crawler 依赖 cms，
// cms 不能反过来 import crawler），指向 SSH 跳板抓取。
func relayCooldown() time.Duration { return sysconf.Dur("ca.relay_cooldown") }

var (
	RelayFetch  func(ctx context.Context, url string, maxBytes int) ([]byte, error)
	RelayWorthy func(err error) bool
)

const (
	relayMaxBytes = 8 << 20
)

// relayFailed 记录各主机最近一次跳板也失败的时间：真正挂掉的站源在冷却期内
// 不再每次都多付一轮 SSH 握手 + 远端超时的等待。
var relayFailed sync.Map // host -> time.Time

func relayFetch(ctx context.Context, rawURL string, ferr error) (string, error) {
	if RelayFetch == nil || RelayWorthy == nil || !strings.HasPrefix(rawURL, "http") || ctx.Err() != nil || !RelayWorthy(ferr) {
		return "", ferr
	}
	host := rawURL
	if u, err := url.Parse(rawURL); err == nil {
		host = u.Host
	}
	if t, ok := relayFailed.Load(host); ok && time.Since(t.(time.Time)) < relayCooldown() {
		return "", ferr
	}
	b, err := RelayFetch(ctx, rawURL, relayMaxBytes)
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		if ctx.Err() == nil {
			relayFailed.Store(host, time.Now())
		}
		return "", ferr
	}
	relayFailed.Delete(host)
	return string(b), nil
}

func (c *Cms) u(params string) string {
	a := c.site.API
	if strings.HasSuffix(a, "?") {
		return a + params
	}
	if strings.Contains(a, "?") {
		return a + "&" + params
	}
	return a + "?" + params
}

// APIURL 导出：拼接站源 API 与参数（供测速等外部使用）。
func (c *Cms) APIURL(params string) string { return c.u(params) }

// ---------- 公共入口 ----------

func (c *Cms) Categories() ([]model.Category, error) {
	return c.CategoriesCtx(context.Background())
}

// CategoriesCtx 与 Categories 相同，ctx 可取消。
func (c *Cms) CategoriesCtx(ctx context.Context) ([]model.Category, error) {
	if c.isPy() {
		return c.pyCategories(ctx)
	}
	txt, err := fetchCtx(ctx, c.u("ac=list"), ttlCat(), sysconf.Dur("to.cms_cat"))
	if err != nil {
		return nil, err
	}
	if c.site.Type == 0 || looksXML(txt) {
		cats := catFromXML(txt)
		if len(cats) > 0 {
			return prependAll(cats), nil
		}
		derived := catsFromVideos(txt)
		if len(derived) > 0 {
			return prependAll(derived), nil
		}
		return derived, nil
	}
	root, err := parse(txt)
	if err != nil {
		return nil, err
	}
	root = Root(root)
	var out []model.Category
	for _, cl := range root.CatList {
		cat := model.Category{
			ID:   strconv.Itoa(int(cl.TypeID)),
			Name: unescapeEntities(s(cl.TypeName)),
		}
		if cl.TypePid > 0 {
			cat.PID = strconv.Itoa(int(cl.TypePid))
		}
		if cat.ID != "" && cat.Name != "" {
			out = append(out, cat)
		}
	}
	if len(out) == 0 && len(root.List) > 0 {
		seen := map[string]bool{}
		for _, it := range root.List {
			tid := strconv.Itoa(int(it.Tid))
			tn := unescapeEntities(s(it.TypeName))
			if tid != "" && tn != "" && !seen[tid] {
				seen[tid] = true
				out = append(out, model.Category{ID: tid, Name: tn})
			}
		}
	}
	if len(out) > 0 {
		out = prependAll(out)
	}
	return out, nil
}

func prependAll(cats []model.Category) []model.Category {
	return append([]model.Category{{ID: "", Name: "全部"}}, cats...)
}

func (c *Cms) List(tid string, page int) (model.VodPage, error) {
	return c.ListCtx(context.Background(), tid, page)
}

// ListCtx 与 List 相同，ctx 可取消。
func (c *Cms) ListCtx(ctx context.Context, tid string, page int) (model.VodPage, error) {
	if c.isPy() {
		return c.pyList(ctx, tid, page)
	}
	q := "ac=videolist&t=" + urlEncode(tid) + "&pg=" + strconv.Itoa(page) + c.extra
	txt, err := fetchCtx(ctx, c.u(q), ttlList(), sysconf.Dur("to.cms_list"))
	if err != nil {
		return model.VodPage{}, err
	}
	if c.site.Type == 0 || looksXML(txt) {
		return listFromXML(txt, page, c.site), nil
	}
	root, err := parse(txt)
	if err != nil {
		return model.VodPage{}, err
	}
	root = Root(root)
	pg := model.VodPage{Page: page, PageCount: 1}
	if root.Page > 0 {
		pg.Page = int(root.Page)
	}
	if root.PageCount > 0 {
		pg.PageCount = int(root.PageCount)
	}
	if root.Total > 0 {
		pg.Total = int(root.Total)
	}
	for _, it := range root.List {
		pg.Items = append(pg.Items, c.toVod(it))
	}
	return pg, nil
}

// ListMerged 把一个父分类和它的子分类合在一起取第一页（按 vod id 去重）。
func (c *Cms) ListMerged(ids []string, page int) (model.VodPage, error) {
	return c.ListMergedCtx(context.Background(), ids, page)
}

// ListMergedCtx 与 ListMerged 相同，ctx 可取消。
func (c *Cms) ListMergedCtx(ctx context.Context, ids []string, page int) (model.VodPage, error) {
	out := model.VodPage{Page: 1, PageCount: 1}
	seen := map[string]bool{}
	type res struct {
		pg  model.VodPage
		err error
	}
	ch := make(chan res, len(ids))
	for _, id := range ids {
		go func(id string) {
			pg, err := c.ListCtx(ctx, id, page)
			ch <- res{pg, err}
		}(id)
	}
	for range ids {
		r := <-ch
		if r.err != nil {
			continue
		}
		for _, it := range r.pg.Items {
			if !seen[it.ID] {
				seen[it.ID] = true
				out.Items = append(out.Items, it)
			}
		}
	}
	out.Total = len(out.Items)
	return out, nil
}

func (c *Cms) Detail(id string) (model.Vod, error) {
	return c.DetailCtx(context.Background(), id)
}

// DetailCtx 与 Detail 相同，ctx 可取消。
func (c *Cms) DetailCtx(ctx context.Context, id string) (model.Vod, error) {
	if c.isPy() {
		return c.pyDetail(ctx, id)
	}
	q := "ac=detail&ids=" + urlEncode(id)
	txt, err := fetchCtx(ctx, c.u(q), ttlDetail(), sysconf.Dur("to.cms_detail"))
	if err != nil {
		return model.Vod{}, err
	}
	if c.site.Type == 0 || looksXML(txt) {
		return detailFromXML(txt, c.site, id)
	}
	root, err := parse(txt)
	if err != nil {
		return model.Vod{}, err
	}
	root = Root(root)
	if len(root.List) > 0 {
		return c.toVod(root.List[0]), nil
	}
	return model.Vod{}, fmt.Errorf("接口没有返回详情数据")
}

// Search 搜索；不同站对 ac / wd 的接受度不同，做两级兜底。
func (c *Cms) Search(kw string) []model.Vod {
	return c.SearchCtx(context.Background(), kw)
}

// SearchCtx 与 Search 相同，ctx 可取消（第一级被取消后不再发起第二级请求）。
func (c *Cms) SearchCtx(ctx context.Context, kw string) []model.Vod {
	if c.isPy() {
		return c.pySearch(ctx, kw)
	}
	// 搜索本质上也是在拉 ac=videolist（只是多个 wd= 关键词过滤），跟片单
	// 翻页是同一类「全量列表」请求，一样要缓存，不能因为它走的是另一个
	// 方法就被漏掉——首页多站搜索一敲关键词就是几十个站并发打一轮，不缓存
	// 的话是这里面风险最大的一类。
	e := urlEncode(kw)
	var items []model.Vod
	if txt, err := fetchCtx(ctx, c.u("ac=videolist&wd="+e), ttlList(), sysconf.Dur("to.cms_search")); err == nil {
		items = c.pickList(txt)
	}
	if len(items) == 0 {
		if ctx.Err() != nil {
			return items
		}
		if txt2, err := fetchCtx(ctx, c.u("wd="+e), ttlList(), sysconf.Dur("to.cms_search")); err == nil {
			items = c.pickList(txt2)
		}
	}
	return items
}

func (c *Cms) pickList(txt string) []model.Vod {
	if c.site.Type == 0 || looksXML(txt) {
		return listFromXML(txt, 1, c.site).Items
	}
	root, err := parse(txt)
	if err != nil {
		return nil
	}
	root = Root(root)
	var out []model.Vod
	for _, it := range root.List {
		out = append(out, c.toVod(it))
	}
	return out
}

func (c *Cms) toVod(j model.JVod) model.Vod {
	// JSON 站源的这些文本字段很多直接是 CMS 后台里存的 HTML 转义文本（跟
	// XML 协议共用同一份库），不像标签那样需要清理，但 & 等实体要解码，
	// 不然标题里带 & 的片子会在界面上显示成「WELSH &amp; SHEDAR」这种
	// 没解码的样子。XML 协议那边 node()/text() 走的 plain() 已经做了这步，
	// 这里补齐到 JSON 协议上。
	v := model.Vod{
		ID:       strconv.Itoa(int(j.VodID)),
		Name:     unescapeEntities(s(j.VodName)),
		Pic:      unescapeEntities(s(j.VodPic)),
		Remark:   unescapeEntities(s(j.VodRemarks)),
		Year:     s(j.VodYear),
		Area:     unescapeEntities(s(j.VodArea)),
		Actor:    unescapeEntities(s(j.VodActor)),
		Director: unescapeEntities(s(j.VodDirector)),
		TypeName: unescapeEntities(s(j.TypeName)),
		From:     unescapeEntities(s(j.VodPlayFrom)),
		PlayURL:  unescapeEntities(s(j.VodPlayURL)),
		SiteKey:  c.site.Key,
		SiteName: c.site.Name,

		Time:      parseVodTime(j.VodTime),
		Score:     float64(j.VodScore),
		Douban:    float64(j.VodDoubanScore),
		Hits:      float64(j.VodHits),
		HitsWeek:  float64(j.VodHitsWeek),
		HitsMonth: float64(j.VodHitsMonth),
		Up:        float64(j.VodUp),
	}
	content := s(j.VodContent)
	if content == "" {
		content = s(j.VodBlurb)
	}
	v.Content = plain(content)
	return v
}

// ---------- 静态工具 ----------

// parseVodTime 把 vod_time（"2006-01-02 15:04:05" / 纯日期 / Unix 秒）解析成
// Unix 秒，按东八区解释（采集站基本都在国内）。解不出来返回 0。
func parseVodTime(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 1e9 {
		if n > 1e12 {
			n /= 1000
		}
		return n
	}
	loc := time.FixedZone("CST", 8*3600)
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02", "2006/01/02 15:04:05", "2006/01/02"} {
		if t, err := time.ParseInLocation(layout, raw, loc); err == nil {
			return t.Unix()
		}
	}
	return 0
}

func s(v string) string { return strings.TrimSpace(v) }

func looksXML(txt string) bool {
	return strings.HasPrefix(strings.TrimLeft(txt, " \t\r\n"), "<")
}

// escapeRawControlCharsInStrings 把 JSON 字符串字面量内部未转义的控制字符
// （最常见是裸换行/回车/制表符）改成合法的转义序列。按引号配对识别「当前
// 是不是在字符串里面」（遇到反斜杠时下一个字符原样透传，避免把 \" 的 "
// 误判成字符串结束），字符串外的结构性空白不受影响。
func escapeRawControlCharsInStrings(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inStr {
			b.WriteByte(c)
			if c == '"' {
				inStr = true
			}
			continue
		}
		if escaped {
			b.WriteByte(c)
			escaped = false
			continue
		}
		switch c {
		case '\\':
			b.WriteByte(c)
			escaped = true
		case '"':
			b.WriteByte(c)
			inStr = false
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}

// CleanJSON 配置 JSON 容错：去 BOM、转义字符串内裸控制字符、去 // 和 #
// 注释行、把保留字 class 改名。
// 注意："class" 仅当后跟 [ 时才替换，避免误匹配 "class_name"、"className" 等字段。
func CleanJSON(txt string) string {
	t := txt
	if strings.HasPrefix(t, "\uFEFF") {
		t = t[3:] // 去 UTF-8 BOM
	}
	// 有些采集源的 warningText/notice 之类字段手写多行提示文字时，直接在
	// JSON 字符串里嵌了裸换行（不满足 JSON 规范，字符串内的控制字符必须
	// 转义），Go 的 encoding/json 会报 "invalid character '\n' in string"
	// 直接拒绝整份配置。这里先转义掉字符串内部的裸控制字符，再做下面按行
	// 的注释剔除——顺序很重要：转义掉之后字符串里就不会再混进真正的换行，
	// 不会把本该连在一起的值误切成「新的一行」，影响后面的注释行判断。
	t = escapeRawControlCharsInStrings(t)
	lines := strings.Split(t, "\n")
	var keep []string
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		keep = append(keep, line)
	}
	// 只替换 "class": [ 这种确定是分类数组的写法，
	// 避免误把 "class_name"、"className" 等字段替换掉
	return regexp.MustCompile(`"class"(\s*:\s*\[)`).ReplaceAllString(strings.Join(keep, "\n"), `"catList"$1`)
}

func parse(txt string) (model.JResp, error) {
	t := CleanJSON(txt)
	if strings.TrimSpace(t) == "" {
		return model.JResp{}, fmt.Errorf("接口返回空内容")
	}
	var j model.JResp
	if err := json.Unmarshal([]byte(t), &j); err != nil {
		return model.JResp{}, fmt.Errorf("接口返回的不是合法 JSON（站源可能已失效）")
	}
	return j, nil
}

// Root type 4 站点返回体多包一层 data。
func Root(j model.JResp) model.JResp {
	if j.Data != nil {
		return *j.Data
	}
	return j
}

var (
	reBr      = regexp.MustCompile(`(?i)<br\s*/?>`)
	reP       = regexp.MustCompile(`(?i)</p>`)
	reTag     = regexp.MustCompile(`<[^>]+>`)
	reEntity  = regexp.MustCompile(`&(lt|gt|quot|nbsp|amp|#39);`)
	reNewline = regexp.MustCompile(`\n{3,}`)
)

// unescapeEntities 只做 HTML/XML 实体解码，不碰标签——用于不需要去标签的
// 字段（片名、演员、分类名等）。plain() 在这基础上再加标签清理，给简介用。
func unescapeEntities(s string) string {
	if !strings.ContainsRune(s, '&') {
		return s
	}
	return reEntity.ReplaceAllStringFunc(s, func(m string) string {
		switch m {
		case "&lt;":
			return "<"
		case "&gt;":
			return ">"
		case "&quot;":
			return `"`
		case "&#39;":
			return "'"
		case "&nbsp;":
			return " "
		case "&amp;":
			return "&"
		}
		return m
	})
}

// plain HTML 实体 + 标签清理，用于简介。
func plain(s0 string) string {
	r := reBr.ReplaceAllString(s0, "\n")
	r = reP.ReplaceAllString(r, "\n")
	r = reTag.ReplaceAllString(r, "")
	r = unescapeEntities(r)
	r = reNewline.ReplaceAllString(r, "\n\n")
	return strings.TrimSpace(r)
}

// node XML 节点取值：先取 CDATA 内容，再清标签与实体。
func node(block, tag string) string {
	re := regexp.MustCompile(`(?is)<` + tag + `>(.*?)</` + tag + `>`)
	m := re.FindStringSubmatch(block)
	if m == nil {
		return ""
	}
	return text(m[1])
}

func text(s0 string) string {
	r := s0
	if i := strings.Index(r, "<![CDATA["); i >= 0 {
		if e := strings.Index(r[i:], "]]>"); e > 0 {
			r = r[i+9 : i+e]
		}
	}
	return plain(r)
}

// catsFromVideos 从每条影片的 tid/type 反推分类。
func catsFromVideos(xml string) []model.Category {
	var out []model.Category
	seen := map[string]bool{}
	re := regexp.MustCompile(`(?is)<video>.*?</video>`)
	for _, b := range re.FindAllString(xml, -1) {
		tid := node(b, "tid")
		tn := node(b, "type")
		if tid != "" && tn != "" && !seen[tid] {
			seen[tid] = true
			out = append(out, model.Category{ID: tid, Name: tn})
		}
	}
	return out
}

func catFromXML(xml string) []model.Category {
	var out []model.Category
	re := regexp.MustCompile(`(?is)<ty\b[^>]*\bid="(\d+)"[^>]*>(.*?)</ty>`)
	for _, m := range re.FindAllStringSubmatch(xml, -1) {
		name := plain(m[2])
		if name != "" {
			out = append(out, model.Category{ID: m[1], Name: name})
		}
	}
	return out
}

// parseDD 从一个 <video> 块里取出所有 <dd flag="...">线路内容</dd>，拼成
// SplitLines 认的 From/PlayURL 格式。detail 和 list 两个接口的 <video>
// 块结构是一样的，很多站的 list 接口其实也会把播放地址带出来（不是只有
// detail 才有），之前 listFromXML 没取，片单卡片上就一直算不出总集数。
func parseDD(b string) (from, playURL string) {
	var flags, urls []string
	ddRe := regexp.MustCompile(`(?is)<dd\b[^>]*\bflag="([^"]*)"[^>]*>(.*?)</dd>`)
	for _, m := range ddRe.FindAllStringSubmatch(b, -1) {
		flags = append(flags, m[1])
		urls = append(urls, text(m[2]))
	}
	if len(flags) == 0 {
		ddRe2 := regexp.MustCompile(`(?is)<dd\b[^>]*>(.*?)</dd>`)
		n := 1
		for _, m := range ddRe2.FindAllStringSubmatch(b, -1) {
			flags = append(flags, "线路"+strconv.Itoa(n))
			urls = append(urls, text(m[1]))
			n++
		}
	}
	return strings.Join(flags, "$$$"), strings.Join(urls, "$$$")
}

func listFromXML(xml string, page int, site model.Site) model.VodPage {
	pg := model.VodPage{Page: page}
	if head := regexp.MustCompile(`(?i)<list\b[^>]*>`).FindString(xml); head != "" {
		if p := regexp.MustCompile(`(?i)\bpage="(\d+)"`).FindStringSubmatch(head); p != nil {
			pg.Page, _ = strconv.Atoi(p[1])
		}
		if pc := regexp.MustCompile(`(?i)\bpagecount="(\d+)"`).FindStringSubmatch(head); pc != nil {
			pg.PageCount, _ = strconv.Atoi(pc[1])
		}
		if rc := regexp.MustCompile(`(?i)\brecordcount="(\d+)"`).FindStringSubmatch(head); rc != nil {
			pg.Total, _ = strconv.Atoi(rc[1])
		}
	}
	re := regexp.MustCompile(`(?is)<video>.*?</video>`)
	for _, b := range re.FindAllString(xml, -1) {
		v := model.Vod{
			ID:       node(b, "id"),
			Name:     node(b, "name"),
			Pic:      node(b, "pic"),
			TypeName: node(b, "type"),
			Year:     node(b, "year"),
			SiteKey:  site.Key,
			SiteName: site.Name,
		}
		if note := node(b, "note"); note != "" {
			v.Remark = note
		} else {
			v.Remark = node(b, "dt")
		}
		v.From, v.PlayURL = parseDD(b)
		if v.Name != "" {
			pg.Items = append(pg.Items, v)
		}
	}
	return pg
}

func detailFromXML(xml string, site model.Site, id string) (model.Vod, error) {
	re := regexp.MustCompile(`(?is)<video>.*?</video>`)
	blocks := re.FindAllString(xml, -1)
	if len(blocks) == 0 {
		return model.Vod{}, fmt.Errorf("接口没有返回详情数据")
	}
	b := blocks[0]
	v := model.Vod{
		ID:       node(b, "id"),
		Name:     node(b, "name"),
		Pic:      node(b, "pic"),
		Remark:   node(b, "note"),
		TypeName: node(b, "type"),
		Year:     node(b, "year"),
		Area:     node(b, "area"),
		Actor:    node(b, "actor"),
		Director: node(b, "director"),
		Content:  node(b, "des"),
		SiteKey:  site.Key,
		SiteName: site.Name,
	}
	if v.ID == "" {
		v.ID = id
	}
	v.From, v.PlayURL = parseDD(b)
	return v, nil
}

// SplitLines vod_play_url / vod_play_from → 线路与剧集。
func SplitLines(from, playURL string) []model.Line {
	var out []model.Line
	if playURL == "" {
		return out
	}
	var fArr []string
	if from != "" {
		fArr = strings.Split(from, "$$$")
	}
	uArr := strings.Split(playURL, "$$$")
	for i, u := range uArr {
		ln := model.Line{Name: fmt.Sprintf("线路 %d", i+1)}
		if i < len(fArr) && fArr[i] != "" {
			ln.Name = fArr[i]
		}
		for k, seg := range strings.Split(u, "#") {
			seg = strings.TrimSpace(seg)
			if seg == "" {
				continue
			}
			ep := model.Episode{Name: fmt.Sprintf("第 %d 集", k+1), URL: seg}
			if p := strings.Index(seg, "$"); p > 0 {
				ep.Name = strings.TrimSpace(seg[:p])
				ep.URL = strings.TrimSpace(seg[p+1:])
			}
			if ep.URL != "" && strings.Contains(ep.URL, "://") {
				ln.Eps = append(ln.Eps, ep)
			}
		}
		directCount := 0
		for _, e := range ln.Eps {
			if IsDirect(e.URL) {
				directCount++
			}
		}
		ln.Direct = len(ln.Eps) > 0 && directCount*2 >= len(ln.Eps)
		if len(ln.Eps) > 0 {
			out = append(out, ln)
		}
	}
	return out
}

// EpCount 总集数：取各线路里集数最多的那条（线路之间偶尔有抓取失败、
// 集数对不齐的情况，取最全的那条更接近真实总集数）。给首页卡片这类
// 只想存一个数、不想存整份 From/PlayURL 的场景用。
func EpCount(from, playURL string) int {
	max := 0
	for _, ln := range SplitLines(from, playURL) {
		if n := len(ln.Eps); n > max {
			max = n
		}
	}
	return max
}

func IsDirect(u string) bool {
	lu := strings.ToLower(u)
	for _, ext := range []string{".m3u8", ".mp4", ".flv", ".mkv", ".ts", ".m4v"} {
		if strings.Contains(lu, ext) {
			return true
		}
	}
	return false
}

// ---------- 筛选（ext 里的 filters） ----------

type jExt struct {
	Filters map[string][]struct {
		Key   string `json:"key"`
		Name  string `json:"name"`
		Value []struct {
			N string `json:"n"`
			V string `json:"v"`
		} `json:"value"`
	} `json:"filters"`
}

// FiltersOf 从站点 ext 里取出该分类的筛选组。
func FiltersOf(ext, tid string) []model.FilterGroup {
	var out []model.FilterGroup
	txt := strings.TrimSpace(ext)
	if txt == "" || txt[0] != '{' {
		return out
	}
	var j jExt
	if err := json.Unmarshal([]byte(txt), &j); err != nil {
		return out
	}
	groups, ok := j.Filters[tid]
	if !ok {
		return out
	}
	for _, g := range groups {
		gg := model.FilterGroup{Key: g.Key, Name: g.Name}
		if gg.Name == "" {
			gg.Name = g.Key
		}
		for _, v := range g.Value {
			if v.N != "" {
				gg.Values = append(gg.Values, model.FilterValue{N: v.N, V: v.V})
			}
		}
		if gg.Key != "" && len(gg.Values) > 0 {
			out = append(out, gg)
		}
	}
	return out
}

// SortCategories 按 type_pid 层级稳定排序（父类在前）。
func SortCategories(cats []model.Category) []model.Category {
	sort.SliceStable(cats, func(i, j int) bool {
		return cats[i].PID < cats[j].PID
	})
	return cats
}

// urlEncode 用 Go 标准库转义（与 JS encodeURIComponent 语义接近）。
func urlEncode(s0 string) string {
	return url.QueryEscape(s0)
}
