package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/resolve"
	"github.com/xxl6097/TVBoxWeb/internal/sysconf"
)

// 点播 m3u8 / 解析结果的磁盘缓存（gzip）。
//
// 点播的 m3u8 内容是固定的（带 #EXT-X-ENDLIST），续播/重看时没必要再向上游拉一次；
// 「播放页 → 解析真实地址」这一步同样要走一轮网络，也一并缓存。
// 缓存的是上游原文（未改写），改写依赖当次请求的 token，所以命中后仍现场改写。
//
// 存放：<缓存目录>/m3u8/，每条一个 .json.gz（m- 开头是 m3u8，r- 开头是解析结果）。
// 内存里只留元数据索引，正文命中时才从磁盘读；重启后索引从目录重建，缓存不丢。
//
// TTL：普通地址 7 天；URL 里带签名/令牌/过期参数的 10 分钟（这类地址很快失效）。
// 播放出错时前端会调 /api/m3u8cache/drop 丢弃对应缓存，用户也可在详情页手动删除。

const (
	m3u8TTL       = 7 * 24 * time.Hour
	m3u8TTLSigned = 10 * time.Minute
	m3u8CacheMax  = 3000
)

type m3u8Variant struct {
	URL    string
	Height int
}

// m3u8Entry 是落盘的内容（Data 为上游原文）；内存索引里 Data 为空。
type m3u8Entry struct {
	Target   string
	Auth     string
	Data     string `json:",omitempty"`
	Base     string // 解析相对地址用的最终 URL
	CT       string
	Exp      time.Time
	Height   int           // 变体播放列表的清晰度（来自主列表），未知为 0
	Variants []m3u8Variant // 主列表的子流
}

type resEntry struct {
	Raw string
	R   resolve.Resolved
	Exp time.Time
}

var m3c = struct {
	mu      sync.Mutex
	loaded  bool
	dir     string
	m       map[string]*m3u8Entry // key -> 元数据
	r       map[string]*resEntry  // 原始地址 -> 解析结果
	heights map[string]int        // 子流绝对地址 -> 清晰度（主列表出现时登记）
}{m: map[string]*m3u8Entry{}, r: map[string]*resEntry{}, heights: map[string]int{}}

var signedKeyRe = regexp.MustCompile(`(?i)sign|token|expire|auth|verify|secret|hdnts|wstime|^(t|e|ts|k|key)$`)

func cacheTTL(target string) time.Duration {
	if u, err := url.Parse(target); err == nil {
		for k := range u.Query() {
			if signedKeyRe.MatchString(k) {
				return m3u8TTLSigned
			}
		}
	}
	return m3u8TTL
}

func m3u8Key(target, auth string) string { return target + "\x00" + auth }

func hashName(prefix, s string) string {
	h := sha1.Sum([]byte(s))
	return prefix + hex.EncodeToString(h[:]) + ".json.gz"
}

func m3u8File(key string) string { return filepath.Join(m3c.dir, hashName("m-", key)) }
func resFile(raw string) string  { return filepath.Join(m3c.dir, hashName("r-", raw)) }

func writeGz(path string, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	tmp := path + ".tmp"
	if os.WriteFile(tmp, buf.Bytes(), 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

func readGz(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, 16<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// cacheInit 首次使用时扫描缓存目录重建内存索引（调用方持有 m3c.mu）。
func cacheInit() {
	if m3c.loaded {
		return
	}
	m3c.loaded = true
	m3c.dir = sysconf.CacheDir("m3u8")
	now := time.Now()
	ents, _ := os.ReadDir(m3c.dir)
	for _, de := range ents {
		name := de.Name()
		path := filepath.Join(m3c.dir, name)
		switch {
		case strings.HasPrefix(name, "m-") && strings.HasSuffix(name, ".json.gz"):
			var e m3u8Entry
			if readGz(path, &e) != nil || now.After(e.Exp) {
				_ = os.Remove(path)
				continue
			}
			e.Data = ""
			m3c.m[m3u8Key(e.Target, e.Auth)] = &e
			for _, v := range e.Variants {
				if v.Height > 0 {
					m3c.heights[v.URL] = v.Height
				}
			}
		case strings.HasPrefix(name, "r-") && strings.HasSuffix(name, ".json.gz"):
			var e resEntry
			if readGz(path, &e) != nil || now.After(e.Exp) {
				_ = os.Remove(path)
				continue
			}
			m3c.r[e.Raw] = &e
		case strings.HasSuffix(name, ".tmp"):
			_ = os.Remove(path)
		}
	}
}

// m3u8Has 只查元数据（不读正文）。
func m3u8Has(target, auth string) *m3u8Entry {
	m3c.mu.Lock()
	defer m3c.mu.Unlock()
	cacheInit()
	k := m3u8Key(target, auth)
	e := m3c.m[k]
	if e == nil {
		return nil
	}
	if time.Now().After(e.Exp) {
		delete(m3c.m, k)
		_ = os.Remove(m3u8File(k))
		return nil
	}
	return e
}

// m3u8Get 命中时从磁盘读出正文。
func m3u8Get(target, auth string) *m3u8Entry {
	if m3u8Has(target, auth) == nil {
		return nil
	}
	var e m3u8Entry
	if readGz(m3u8File(m3u8Key(target, auth)), &e) != nil || e.Data == "" {
		m3c.mu.Lock()
		delete(m3c.m, m3u8Key(target, auth))
		m3c.mu.Unlock()
		return nil
	}
	return &e
}

var resRe = regexp.MustCompile(`(?i)RESOLUTION=\d+x(\d+)`)

// m3u8Store 判断并缓存一份上游 m3u8：点播列表（ENDLIST）一律缓存；
// 主列表（STREAM-INF）仅在调用方声明是点播（vod=1）时缓存。
func m3u8Store(target, base, auth, ct, data string, vod bool) {
	if len(data) >= 4<<20 {
		return // 被截断过
	}
	isMaster := strings.Contains(data, "#EXT-X-STREAM-INF")
	if !(strings.Contains(data, "#EXT-X-ENDLIST") || (vod && isMaster)) {
		return
	}
	e := &m3u8Entry{Target: target, Auth: auth, Base: base, CT: ct, Exp: time.Now().Add(cacheTTL(target))}
	if !strings.Contains(strings.ToLower(ct), "mpegurl") {
		e.CT = "application/vnd.apple.mpegurl"
	}
	m3c.mu.Lock()
	defer m3c.mu.Unlock()
	cacheInit()
	if isMaster {
		lines := strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n")
		for i := 0; i < len(lines); i++ {
			l := strings.TrimSpace(lines[i])
			if !strings.HasPrefix(l, "#EXT-X-STREAM-INF") {
				continue
			}
			h := 0
			if mm := resRe.FindStringSubmatch(l); mm != nil {
				h, _ = strconv.Atoi(mm[1])
			}
			for i++; i < len(lines); i++ {
				u := strings.TrimSpace(lines[i])
				if u == "" || strings.HasPrefix(u, "#") {
					continue
				}
				if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
					u = absURL(base, u)
				}
				e.Variants = append(e.Variants, m3u8Variant{URL: u, Height: h})
				if h > 0 {
					if len(m3c.heights) > 20000 {
						m3c.heights = map[string]int{}
					}
					m3c.heights[u] = h
				}
				break
			}
		}
	} else {
		e.Height = m3c.heights[target]
	}
	if len(m3c.m) >= m3u8CacheMax {
		now := time.Now()
		var oldK string
		var oldT time.Time
		for k, v := range m3c.m {
			if now.After(v.Exp) {
				delete(m3c.m, k)
				_ = os.Remove(m3u8File(k))
				continue
			}
			if oldK == "" || v.Exp.Before(oldT) {
				oldK, oldT = k, v.Exp
			}
		}
		if len(m3c.m) >= m3u8CacheMax && oldK != "" {
			delete(m3c.m, oldK)
			_ = os.Remove(m3u8File(oldK))
		}
	}
	full := *e
	full.Data = data
	writeGz(m3u8File(m3u8Key(target, auth)), &full)
	m3c.m[m3u8Key(target, auth)] = e
}

// ---------- 解析结果缓存 ----------

func resolveGet(raw string) (resolve.Resolved, bool) {
	m3c.mu.Lock()
	defer m3c.mu.Unlock()
	cacheInit()
	e := m3c.r[raw]
	if e == nil {
		return resolve.Resolved{}, false
	}
	if time.Now().After(e.Exp) {
		delete(m3c.r, raw)
		_ = os.Remove(resFile(raw))
		return resolve.Resolved{}, false
	}
	return e.R, true
}

func resolvePut(raw string, r resolve.Resolved) {
	if !r.OK || r.URL == "" || r.URL == raw {
		return
	}
	m3c.mu.Lock()
	defer m3c.mu.Unlock()
	cacheInit()
	if len(m3c.r) >= m3u8CacheMax {
		now := time.Now()
		for k, v := range m3c.r {
			if now.After(v.Exp) {
				delete(m3c.r, k)
				_ = os.Remove(resFile(k))
			}
		}
		if len(m3c.r) >= m3u8CacheMax {
			return
		}
	}
	e := &resEntry{Raw: raw, R: r, Exp: time.Now().Add(cacheTTL(r.URL))}
	m3c.r[raw] = e
	writeGz(resFile(raw), e)
}

// ---------- 接口 ----------

type m3u8Status struct {
	Cached bool `json:"cached"`
	Height int  `json:"height,omitempty"` // 已缓存的最高清晰度，未知为 0
}

// m3u8StatusOf 查一个原始播放地址（解析前）是否已有缓存的 m3u8。
func m3u8StatusOf(raw string) m3u8Status {
	// 候选键与播放页一致：解析结果、原地址，以及两者经「补全 /index.m3u8」后的地址
	cands := []string{raw}
	if r, ok := resolveGet(raw); ok {
		cands = append([]string{r.URL}, cands...)
	}
	for _, c := range append([]string(nil), cands...) {
		if f := fixPlayableURL(c); f != c {
			cands = append(cands, f)
		}
	}
	for _, c := range cands {
		e := m3u8Has(c, "")
		if e == nil {
			continue
		}
		st := m3u8Status{Cached: true, Height: e.Height}
		for _, v := range e.Variants {
			if ve := m3u8Has(v.URL, ""); ve != nil && v.Height > st.Height {
				st.Height = v.Height
			}
		}
		return st
	}
	return m3u8Status{}
}

// hM3u8Status POST {urls:[原始播放地址]} -> {status:{url:{cached,height}}}，只返回命中的。
func hM3u8Status(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URLs []string `json:"urls"`
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	if len(req.URLs) > 3000 {
		req.URLs = req.URLs[:3000]
	}
	out := map[string]m3u8Status{}
	for _, u := range req.URLs {
		if st := m3u8StatusOf(u); st.Cached {
			out[u] = st
		}
	}
	ok(w, map[string]any{"status": out})
}

// hM3u8Drop POST {url} 或 {urls:[...]}：丢弃这些地址（原始或解析后）相关的缓存。
// 手动删除（详情页线路上的 ×）和播放失败时都走这里。
func hM3u8Drop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL  string   `json:"url"`
		URLs []string `json:"urls"`
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	if req.URL != "" {
		req.URLs = append(req.URLs, req.URL)
	}
	if len(req.URLs) > 3000 {
		req.URLs = req.URLs[:3000]
	}
	n := 0
	m3c.mu.Lock()
	defer m3c.mu.Unlock()
	cacheInit()
	targets := map[string]bool{}
	for _, u := range req.URLs {
		targets[u] = true
		if e := m3c.r[u]; e != nil {
			targets[e.R.URL] = true
			delete(m3c.r, u)
			_ = os.Remove(resFile(u))
		}
	}
	// 传进来的是解析后的地址时，也要清掉指向它的解析结果
	for k, e := range m3c.r {
		if targets[e.R.URL] {
			delete(m3c.r, k)
			_ = os.Remove(resFile(k))
		}
	}
	for t := range targets {
		for k, e := range m3c.m {
			if e.Target != t {
				continue
			}
			// 主列表：连带丢弃它的子流（过期的分片签名多半出在子流上）
			for _, v := range e.Variants {
				for k2, e2 := range m3c.m {
					if e2.Target == v.URL {
						delete(m3c.m, k2)
						_ = os.Remove(m3u8File(k2))
					}
				}
			}
			delete(m3c.m, k)
			_ = os.Remove(m3u8File(k))
			n++
		}
	}
	ok(w, map[string]any{"ok": true, "dropped": n})
}

// fixPlayableURL 对应前端 PlayerView.fixPlayable：没有扩展名的地址补 /index.m3u8。
func fixPlayableURL(u string) string {
	lu := strings.ToLower(u)
	if u == "" || !strings.Contains(u, "://") {
		return u
	}
	for _, e := range []string{".m3u8", ".mp4", ".flv", ".mkv", ".ts", ".m4v"} {
		if strings.Contains(lu, e) {
			return u
		}
	}
	noQuery := strings.TrimRight(strings.SplitN(u, "?", 2)[0], "/")
	last := noQuery[strings.LastIndex(noQuery, "/")+1:]
	if last != "" && !strings.Contains(last, ".") {
		return noQuery + "/index.m3u8"
	}
	return u
}
