// Package live 实现直播源解析（M3U/TXT）+ 频道目录，与鸿蒙影视 Live.ets 一致。
package live

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

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
