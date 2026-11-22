// Package live 实现直播源解析（M3U/TXT）+ 频道目录，与鸿蒙影视 Live.ets 一致。
package live

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/crawler"
	"github.com/xxl6097/TVBoxWeb/internal/httpc"
	"github.com/xxl6097/TVBoxWeb/internal/model"
)

// Probe 单条流探活 + 测响应速度。
func Probe(url string, ms time.Duration) bool {
	return ProbeCtx(context.Background(), url, ms)
}

// ProbeCtx 与 Probe 相同，ctx 可取消。
func ProbeCtx(ctx context.Context, url string, ms time.Duration) bool {
	if url == "" {
		return false
	}
	head, err := httpc.PeekCtx(ctx, url, ms)
	if err != nil || head == "" {
		return false
	}
	if head == "#BIN" || strings.Contains(head, "#EXTM3U") || strings.Contains(head, "#EXTINF") {
		return true
	}
	t := strings.ToLower(strings.TrimSpace(head))
	if strings.HasPrefix(t, "<!doctype") || strings.HasPrefix(t, "<html") || strings.Contains(t, "<body") {
		return false
	}
	if strings.Contains(t, "error_account") || strings.Contains(t, "pirated") {
		return false
	}
	return true
}

// minStreamBytes 判定「真的连续播出过」所需的最少字节数：按 dur（见
// internal/api/livescan.go 的 liveScanStreamDur，目前 1.2 秒）配合这个
// 下限一起算，相当于要求平均吞吐至少 ~40KB/s（≈320kbps）——实况 TV 频道
// 哪怕压得很狠也很少低于这个码率，但「连得上、对面服务器先吐几 KB 缓冲
// 占位数据就不再吭声」这种假阳性（StreamCheckCtx 不设「凑够就提前返回」
// 的快捷路径，老实读满 dur 整段时间，这个假阳性只会卡在远低于这个值的
// 总量上）会被正确拦下来。
const minStreamBytes = 48 * 1024

// LineProbeResult ProbeAnyStreamCtx 里某一条线路「真的测过」的结果——调用方
// 拿它去记每条线路的可靠度分数（见 store.RecordLineOutcome），而不是只知道
// "频道整体能不能播"这一个粗粒度的布尔值。LatencyMs 只在找到真正的媒体地址
// （resolveMediaTarget 成功）时才有意义；完全连不上/解析失败时是「从开始
// 探测到确认失败」的耗时，仅供参考。
type LineProbeResult struct {
	URL       string
	OK        bool
	LatencyMs int64
}

// ProbeAnyStream 依次测一批线路（通常是同一个频道合并来的全部备用地址），
// 只要有一条线路通过 ProbeStreamCtx 就立刻返回 true，不等剩下的线路测
// 完——这跟真实播放器的行为一致：一条线播不出来会自动切下一条，只要有
// 一条真能播，频道整体就该算「能播」，不该因为排在前面的某条线挂了就把
// 整个频道打成「播不通」。
func ProbeAnyStream(urls []string, dur time.Duration, maxLines int) bool {
	ok, _ := ProbeAnyStreamCtx(context.Background(), urls, dur, maxLines)
	return ok
}

// ProbeAnyStreamCtx 与 ProbeAnyStream 相同，ctx 可取消，额外返回「这一轮
// 真的测过」的每条线路的结果（按尝试顺序，命中成功那条之后不再继续测，
// 后面没测到的线路不出现在返回值里——没有信息就不该写一个假结果回去）。
// maxLines<=0 时不限制测试的线路数；合并之后个别频道线路数能到几十条，
// 调用方可以传个上限防止极端情况下单个频道测试时间失控。
func ProbeAnyStreamCtx(ctx context.Context, urls []string, dur time.Duration, maxLines int) (bool, []LineProbeResult) {
	n := len(urls)
	if maxLines > 0 && n > maxLines {
		n = maxLines
	}
	var results []LineProbeResult
	for i := 0; i < n; i++ {
		if urls[i] == "" {
			continue
		}
		ok, latencyMs := ProbeStreamCtx(ctx, urls[i], dur)
		results = append(results, LineProbeResult{URL: urls[i], OK: ok, LatencyMs: latencyMs})
		if ok {
			return true, results
		}
	}
	return false, results
}

// ProbeStream 跟轻量的 Probe 不一样：Probe 只看第一包的几 KB 像不像一个
// 正常的流响应（给「检测」按钮、测速排行这类要求「秒出结果」的交互式场景
// 用，详见 Probe 的注释），会把「manifest 能连上、但实际分片早就取不到、
// 播放器里一直转圈」误判成能播。这个函数给背景扫描「可播频道」这种愿意
// 多等一两秒换正确性的场景用：真的要求连续拉到至少一段时间（dur）的数据
// 才算过关。
//
// 对 HLS（m3u8）特殊处理：manifest 本身通常只有几百字节到几 KB，直接拿
// manifest 的 URL 去「连续读 dur」只会立刻 EOF、永远凑不够字节——必须先
// 顺着它找到真正的媒体分片（master 播放列表再多跟一层子播放列表），实际
// 验证的是分片 URL 的连续传输情况，而不是 manifest 本身传了多少字节。
func ProbeStream(u string, dur time.Duration) bool {
	ok, _ := ProbeStreamCtx(context.Background(), u, dur)
	return ok
}

// ProbeStreamCtx 与 ProbeStream 相同，ctx 可取消，额外返回这次探测的延迟
// （毫秒）：从开始到解析出真正的媒体地址（resolveMediaTarget，含 m3u8
// master/media 多跳）为止的耗时，不包含后面 dur 那段「老实读够这么久」的
// 固定时间——那段时间长短是调用方传进来的固定参数，不反映这条线路本身
// 的快慢，混进延迟里会让所有成功线路的分数挤在同一档，起不到排序作用。
// 解析失败（完全连不上/不是能识别的播放列表）时延迟是到确认失败为止的
// 耗时，仅供参考。
func ProbeStreamCtx(ctx context.Context, u string, dur time.Duration) (ok bool, latencyMs int64) {
	start := time.Now()
	target, resolved := resolveMediaTarget(ctx, u, 0)
	latencyMs = time.Since(start).Milliseconds()
	if !resolved {
		return false, latencyMs
	}
	ok = httpc.StreamCheckCtx(ctx, target, dur, minStreamBytes)
	return ok, latencyMs
}

// resolveMediaTarget 把一个直播地址解析成「真正要连续读的那个 URL」：不是
// 播放列表就是它自己；是播放列表就顺着找到第一个媒体分片（最多跟 3 层，
// master -> media -> 分片，防止格式异常的播放列表互相指来指去时无限递归）。
func resolveMediaTarget(ctx context.Context, u string, hop int) (string, bool) {
	if u == "" || hop > 3 {
		return "", false
	}
	head, err := httpc.PeekCtx(ctx, u, 6*time.Second)
	if err != nil || head == "" {
		return "", false
	}
	if head == "#BIN" {
		// 不是文本播放列表，直接当成媒体流本身。
		return u, true
	}
	t := strings.ToLower(strings.TrimSpace(head))
	if strings.HasPrefix(t, "<!doctype") || strings.HasPrefix(t, "<html") || strings.Contains(t, "<body") {
		return "", false
	}
	if !strings.Contains(head, "#EXTM3U") {
		// 不认识的文本响应，没法当播放列表解析；保守起见直接拿它自己当
		// 媒体流试一次连续读——少数非标准直播源确实是裸流，只是响应头
		// 报的 content-type 不规范，被 PeekCtx 判成了文本。
		return u, true
	}

	// 是 m3u8：拿完整内容（不只是 4KB 头）才能找到第一条分片/子播放列表
	// 地址——这类文件本来就很小（几百字节到几 KB 量级），整份拉下来代价
	// 很低，不会拖慢多少。
	full, err := httpc.TextCtx(ctx, u, nil, 8*time.Second)
	if err != nil || full == "" {
		return "", false
	}
	next := firstM3U8Ref(full)
	if next == "" {
		return "", false
	}
	abs := resolveURLRef(u, next)
	if abs == "" {
		return "", false
	}
	return resolveMediaTarget(ctx, abs, hop+1)
}

// firstM3U8Ref 找 m3u8 文本里第一条「要跟下去」的地址：master 播放列表
// （带 #EXT-X-STREAM-INF）取紧跟其后的第一个子播放列表地址；媒体播放列表
// （没有 #EXT-X-STREAM-INF）取第一条分片地址（第一行不以 # 开头的非空行）。
func firstM3U8Ref(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), "#EXT-X-STREAM-INF") {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			next := strings.TrimSpace(lines[j])
			if next == "" || strings.HasPrefix(next, "#") {
				continue
			}
			return next
		}
	}
	// 没有 STREAM-INF：当媒体播放列表处理，找第一条分片地址。
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	return ""
}

func resolveURLRef(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ""
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	return b.ResolveReference(r).String()
}

// LineQuality 单条播放线路的轻量测速结果：只取开头 16KB，不下载真正的视频
// 数据，几乎不占带宽，可以安全地在频道播放期间周期性跑。
type LineQuality struct {
	URL  string `json:"url"`
	OK   bool   `json:"ok"`
	MS   int    `json:"ms"`   // 建连到拿到数据的耗时
	KBps int    `json:"kbps"` // 粗略吞吐（下载探测包的速率，仅供参考，不代表真实码率）
}

// ProbeLineCtx 轻量测一条线路。
func ProbeLineCtx(ctx context.Context, url string, timeout time.Duration) LineQuality {
	q := LineQuality{URL: url, MS: -1}
	if url == "" {
		return q
	}
	ms, n, err := httpc.RangeTimedCtx(ctx, url, timeout, 16384)
	if err != nil || n == 0 || ms <= 0 {
		return q
	}
	q.OK = true
	q.MS = ms
	q.KBps = n * 1000 / ms / 1024
	return q
}

// ProbeLinesCtx 并发测多条线路（通常就几条到十几条），有界并发避免一次性
// 打出一堆连接；用于「当前频道所有备用线路」的质量监控。
func ProbeLinesCtx(ctx context.Context, urls []string, timeout time.Duration) []LineQuality {
	out := make([]LineQuality, len(urls))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = ProbeLineCtx(ctx, u, timeout)
		}(i, u)
	}
	wg.Wait()
	return out
}

// Load 拉取并解析。失败抛异常。
func Load(url string, timeout time.Duration) ([]model.ChannelGroup, error) {
	return LoadCtx(context.Background(), url, timeout)
}

// LoadCtx 与 Load 相同，ctx 可取消。
func LoadCtx(ctx context.Context, url string, timeout time.Duration) ([]model.ChannelGroup, error) {
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	txt, err := httpc.TextCtx(ctx, url, nil, timeout)
	if err != nil || strings.TrimSpace(txt) == "" {
		// 直播源列表（m3u/txt，纯文本小文件）被地区封锁时，经 SSH_CMDS_GEO_BLOCK
		// 跳板重试一次；只搬频道列表文本，绝不搬直播流本身。
		if strings.HasPrefix(url, "http") && (err == nil || crawler.WorthGeoRelay(err)) {
			if b, rerr := crawler.FetchSmallViaGeoRelay(ctx, url, 8<<20); rerr == nil && strings.TrimSpace(string(b)) != "" {
				txt, err = string(b), nil
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(txt) == "" {
		return nil, fmt.Errorf("直播源返回空内容")
	}
	gs := Parse(txt)
	if len(gs) == 0 {
		return nil, fmt.Errorf("没解析出频道，检查源格式是否为 m3u/txt")
	}
	return gs, nil
}

// MergeGroups 把多份直播源各自解析出的频道列表合并成一份：先按「归一化频道名」
// 把所有源里的同一个频道聚到一起、合并它们的全部线路（URLs），分组名按各源里
// 出现次数最多的那个决定（不同源常把同一频道分到不同分类，用多数票避免拆成两份）。
func MergeGroups(all [][]model.ChannelGroup) []model.ChannelGroup {
	type agg struct {
		ch    model.Channel
		votes map[string]int
	}
	var order []string
	merged := map[string]*agg{}

	for _, gs := range all {
		for _, g := range gs {
			for _, c := range g.Channels {
				key := normalizeChannelName(c.Name)
				if key == "" {
					continue
				}
				a, ok := merged[key]
				if !ok {
					a = &agg{ch: model.Channel{Name: c.Name}, votes: map[string]int{}}
					merged[key] = a
					order = append(order, key)
				}
				a.votes[g.Name]++
				if a.ch.Logo == "" {
					a.ch.Logo = c.Logo
				}
				urls := c.URLs
				if len(urls) == 0 && c.URL != "" {
					urls = []string{c.URL}
				}
				for _, u := range urls {
					if u != "" && !contains(a.ch.URLs, u) {
						a.ch.URLs = append(a.ch.URLs, u)
					}
				}
			}
		}
	}

	groupIdx := map[string]int{}
	var out []model.ChannelGroup
	for _, key := range order {
		a := merged[key]
		if len(a.ch.URLs) == 0 {
			continue
		}
		a.ch.URL = a.ch.URLs[0]
		a.ch.Group = bestVote(a.votes)
		gi, ok := groupIdx[a.ch.Group]
		if !ok {
			out = append(out, model.ChannelGroup{Name: a.ch.Group})
			gi = len(out) - 1
			groupIdx[a.ch.Group] = gi
		}
		out[gi].Channels = append(out[gi].Channels, a.ch)
	}
	return out
}

// normalizeChannelName 去掉空格/连字符差异 + 大小写差异，让「CCTV-1」「cctv1」
// 「CCTV 1」判定为同一个频道；只做这种保守归一化，避免把名字不同的频道误合并。
func normalizeChannelName(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	r := strings.NewReplacer("-", "", "_", "", " ", "", "　", "")
	return r.Replace(s)
}

func bestVote(votes map[string]int) string {
	best, bestN := "未分组", -1
	for g, n := range votes {
		if n > bestN || (n == bestN && g < best) {
			best, bestN = g, n
		}
	}
	return best
}

func Parse(text string) []model.ChannelGroup {
	t := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if strings.Contains(t, "#EXTINF") {
		return parseM3u(t)
	}
	return parseTxt(t)
}

// ---------- 目录 ----------

func BuildTree(groups []model.ChannelGroup) model.LiveDir {
	root := model.LiveDir{Name: "全部"}
	for _, g := range groups {
		if len(g.Channels) == 0 {
			continue
		}
		parts := splitName(g.Name)
		if len(parts) >= 2 {
			p := child(&root, parts[0])
			sub := child(p, tail(parts, 1))
			sub.Channels = append(sub.Channels, g.Channels...)
		} else {
			d := child(&root, g.Name)
			d.Channels = append(d.Channels, g.Channels...)
		}
	}
	countOf(&root)
	return root
}

// Collect 取节点下的全部频道（含所有子目录）。
func Collect(n model.LiveDir, out *[]model.Channel) {
	*out = append(*out, n.Channels...)
	for i := range n.Children {
		Collect(n.Children[i], out)
	}
}

func countOf(n *model.LiveDir) int {
	c := len(n.Channels)
	for i := range n.Children {
		c += countOf(&n.Children[i])
	}
	n.Count = c
	return c
}

func child(parent *model.LiveDir, name string) *model.LiveDir {
	for i := range parent.Children {
		if parent.Children[i].Name == name {
			return &parent.Children[i]
		}
	}
	parent.Children = append(parent.Children, model.LiveDir{Name: name})
	return &parent.Children[len(parent.Children)-1]
}

func splitName(n string) []string {
	var out []string
	var cur strings.Builder
	for _, c := range n {
		switch c {
		case '/', '\\', '|', '｜', '>', '＞':
			if strings.TrimSpace(cur.String()) != "" {
				out = append(out, strings.TrimSpace(cur.String()))
			}
			cur.Reset()
		default:
			cur.WriteRune(c)
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, strings.TrimSpace(cur.String()))
	}
	if len(out) == 0 {
		return []string{n}
	}
	return out
}

func tail(parts []string, from int) string {
	return strings.Join(parts[from:], "/")
}

// ---------- M3U ----------

func parseM3u(t string) []model.ChannelGroup {
	lines := strings.Split(t, "\n")
	var groups []model.ChannelGroup
	curName, curLogo, curGroup := "", "", ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTINF") {
			curGroup = attr(line, "group-title")
			if curGroup == "" {
				curGroup = "未分组"
			}
			curLogo = attr(line, "tvg-logo")
			if c := strings.LastIndex(line, ","); c >= 0 {
				curName = strings.TrimSpace(line[c+1:])
			} else {
				curName = ""
			}
			if curName == "" {
				curName = attr(line, "tvg-name")
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			if strings.HasPrefix(line, "#EXTGRP:") {
				if g := strings.TrimSpace(line[8:]); g != "" {
					curGroup = g
				}
			}
			continue
		}
		if curName == "" {
			continue
		}
		push(&groups, curGroup, curName, line, curLogo)
		curName, curLogo = "", ""
	}
	return groups
}

func attr(line, key string) string {
	i := strings.Index(line, key+`="`)
	if i < 0 {
		return ""
	}
	s := i + len(key) + 2
	e := strings.Index(line[s:], `"`)
	if e < 0 {
		return ""
	}
	return line[s : s+e]
}

// ---------- TXT ----------

func parseTxt(t string) []model.ChannelGroup {
	lines := strings.Split(t, "\n")
	var groups []model.ChannelGroup
	curGroup := "未分组"
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, ",#genre#") {
			if g := strings.TrimSpace(line[:strings.Index(line, ",#genre#")]); g != "" {
				curGroup = g
			}
			continue
		}
		c := strings.Index(line, ",")
		if c <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:c])
		u := strings.TrimSpace(line[c+1:])
		if name == "" || !isURL(u) {
			continue
		}
		push(&groups, curGroup, name, u, "")
	}
	return groups
}

func isURL(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

func push(groups *[]model.ChannelGroup, groupName, name, url, logo string) {
	var g *model.ChannelGroup
	for i := range *groups {
		if (*groups)[i].Name == groupName {
			g = &(*groups)[i]
			break
		}
	}
	if g == nil {
		*groups = append(*groups, model.ChannelGroup{Name: groupName})
		g = &(*groups)[len(*groups)-1]
	}
	for i := range g.Channels {
		if g.Channels[i].Name == name {
			if !contains(g.Channels[i].URLs, url) {
				g.Channels[i].URLs = append(g.Channels[i].URLs, url)
			}
			return
		}
	}
	ch := model.Channel{Name: name, URL: url, Logo: logo, Group: groupName}
	ch.URLs = append(ch.URLs, url)
	g.Channels = append(g.Channels, ch)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
