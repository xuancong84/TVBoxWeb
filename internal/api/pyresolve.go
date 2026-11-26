package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/xxl6097/TVBoxWeb/internal/sysconf"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/cms"
	"github.com/xxl6097/TVBoxWeb/internal/httpc"
	"github.com/xxl6097/TVBoxWeb/internal/pyspider"
	"github.com/xxl6097/TVBoxWeb/internal/resolve"
	"github.com/xxl6097/TVBoxWeb/internal/store"
)

// resolvePyPlay 解析 Python 站源的播放令牌（pyplay:...）：调用站源的 playerContent。
// 站源可能直接给出真实地址（parse=0），也可能只给一个要再解析的网页地址（parse=1），
// 后者继续走普通的解析接口。
func resolvePyPlay(ctx context.Context, tok string) resolve.Resolved {
	key, flag, id, ok := pyspider.ParsePlayToken(tok)
	if !ok {
		return resolve.Resolved{URL: tok, Via: "无效的播放令牌"}
	}
	site := store.Site(key)
	if site == nil {
		return resolve.Resolved{URL: tok, Via: "站源不存在"}
	}
	cctx, cancel := context.WithTimeout(ctx, sysconf.Dur("to.py_play"))
	defer cancel()
	raw, err := cms.SpiderCall(cctx, *site, "play", map[string]any{"flag": flag, "id": id})
	if err != nil {
		return resolve.Resolved{URL: tok, Via: "站源解析失败：" + err.Error()}
	}
	var r struct {
		Parse  any            `json:"parse"`
		URL    any            `json:"url"`
		Header map[string]any `json:"header"`
		Host   string         `json:"host"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return resolve.Resolved{URL: tok, Via: "站源返回格式错误"}
	}
	u := ""
	switch x := r.URL.(type) {
	case string:
		u = strings.TrimSpace(x)
	case []any: // [名称, 地址, 名称, 地址 ...]
		for _, e := range x {
			if s, ok := e.(string); ok && strings.HasPrefix(s, "http") {
				u = s
				break
			}
		}
	}
	if u == "" {
		return resolve.Resolved{URL: tok, Via: "站源没有返回播放地址"}
	}
	hdr := map[string]string{}
	for k, v := range r.Header {
		if s, ok := v.(string); ok {
			hdr[k] = s
		}
	}
	if q, isLocal := pyspider.LocalProxyQuery(u); isLocal {
		return resolve.Resolved{URL: pyspider.ProxyToken(key, q), Header: hdr, Via: site.Name + "（本地代理）", OK: true}
	}
	if fmt.Sprint(r.Parse) == "1" {
		// 相对地址按规则的 host 补全
		if !strings.HasPrefix(u, "http") && strings.HasPrefix(r.Host, "http") {
			if b, err := url.Parse(r.Host); err == nil {
				if ref, err := url.Parse(u); err == nil {
					u = b.ResolveReference(ref).String()
				}
			}
		}
		if isMediaURL(u) {
			return resolve.Resolved{URL: u, Header: hdr, Via: site.Name, OK: true}
		}
		// 网页地址：先交给普通解析接口，不行再抓网页嗅探
		res := resolve.ResolveCtx(ctx, u, store.Parses())
		if !res.OK && strings.HasPrefix(u, "http") {
			if mu, ok := sniffPage(ctx, u, hdr); ok {
				return resolve.Resolved{URL: mu, Header: map[string]string{"Referer": u}, Via: site.Name + "（网页嗅探）", OK: true}
			}
		}
		if res.Header == nil && len(hdr) > 0 {
			res.Header = hdr
		}
		return res
	}
	return resolve.Resolved{URL: u, Header: hdr, Via: site.Name, OK: strings.HasPrefix(u, "http")}
}

var localProxyURL = regexp.MustCompile(`https?://(?:127\.0\.0\.1|localhost):\d+/proxy\?[^\s"'<>]*`)

// servePyProxy 处理 /api/proxy?url=pyproxy:...：调用站源的 localProxy，把它给出的内容回给播放器。
func servePyProxy(w http.ResponseWriter, r *http.Request, tok string) {
	key, query, ok := pyspider.ParseProxyToken(tok)
	if !ok {
		fail(w, http.StatusBadRequest, "无效的本地代理令牌")
		return
	}
	site := store.Site(key)
	if site == nil {
		fail(w, http.StatusNotFound, "站源不存在")
		return
	}
	vals, _ := url.ParseQuery(query)
	params := map[string]string{}
	for k := range vals {
		params[k] = vals.Get(k)
	}
	ctx, cancel := context.WithTimeout(r.Context(), sysconf.Dur("to.py_play"))
	defer cancel()
	raw, err := cms.SpiderCall(ctx, *site, "proxy", map[string]any{"params": params})
	if err != nil {
		fail(w, http.StatusBadGateway, "站源代理失败："+err.Error())
		return
	}
	var pr struct {
		Code int    `json:"code"`
		Mime string `json:"mime"`
		Body string `json:"body"`
	}
	if json.Unmarshal(raw, &pr) != nil {
		fail(w, http.StatusBadGateway, "站源代理返回格式错误")
		return
	}
	body, _ := base64.StdEncoding.DecodeString(pr.Body)
	if o := r.Header.Get("Origin"); OriginAllowed(o, r.Host) {
		w.Header().Set("Access-Control-Allow-Origin", o)
		w.Header().Set("Vary", "Origin")
	}
	if strings.HasPrefix(strings.TrimSpace(string(body[:min(len(body), 16)])), "#EXTM3U") {
		txt := localProxyURL.ReplaceAllStringFunc(string(body), func(m string) string {
			if q, ok := pyspider.LocalProxyQuery(m); ok {
				return pyspider.ProxyToken(key, q)
			}
			return m
		})
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.WriteHeader(max(pr.Code, 200))
		_, _ = w.Write([]byte(rewriteM3u8(txt, "", "", tokenFrom(r))))
		return
	}
	if pr.Mime != "" {
		w.Header().Set("Content-Type", pr.Mime)
	}
	if pr.Code == 0 {
		pr.Code = 200
	}
	w.WriteHeader(pr.Code)
	_, _ = w.Write(body)
}

// isMediaURL 判断是不是可直接播放的媒体地址（含音频）。
func isMediaURL(u string) bool {
	if cms.IsDirect(u) {
		return true
	}
	p := strings.ToLower(u)
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	for _, ext := range []string{".mp3", ".m4a", ".aac", ".flac", ".wav", ".ogg", ".webm"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}

var (
	sniffPlayerJSON = regexp.MustCompile(`player_[a-z0-9]+\s*=\s*(\{.*?\})\s*</script>`)
	sniffMedia      = regexp.MustCompile(`https?:(?:\\?/){2}[^"'\s<>\\]+?\.(?:m3u8|mp4|flv|mp3|m4a)(?:\?[^"'\s<>\\]*)?`)
)

// sniffPage 抓取播放网页，从中找出真实媒体地址（MacCMS 的 player_xxx 变量，或页面里直接出现的 m3u8/mp4）。
// 只做一次普通 HTTP 抓取（走 SSRF 防护），不执行页面脚本。
func sniffPage(ctx context.Context, page string, hdr map[string]string) (string, bool) {
	h := map[string]string{"Referer": page}
	for k, v := range hdr {
		h[k] = v
	}
	txt, err := httpc.TextCtx(ctx, page, h, 10*time.Second)
	if err != nil || txt == "" {
		return "", false
	}
	if m := sniffPlayerJSON.FindStringSubmatch(txt); m != nil {
		var pj struct {
			URL     string `json:"url"`
			Encrypt int    `json:"encrypt"`
		}
		if json.Unmarshal([]byte(m[1]), &pj) == nil && pj.URL != "" {
			u := pj.URL
			switch pj.Encrypt {
			case 1:
				u, _ = url.QueryUnescape(u)
			case 2:
				if b, err := base64.StdEncoding.DecodeString(u); err == nil {
					u, _ = url.QueryUnescape(string(b))
				}
			}
			if strings.HasPrefix(u, "http") && isMediaURL(u) {
				return u, true
			}
		}
	}
	if m := sniffMedia.FindString(txt); m != "" {
		return strings.ReplaceAll(m, `\/`, "/"), true
	}
	return "", false
}
