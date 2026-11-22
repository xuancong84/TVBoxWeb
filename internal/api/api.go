// Package api 实现 REST API 与视频流代理。
package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/cms"
	"github.com/xxl6097/TVBoxWeb/internal/crawler"
	"github.com/xxl6097/TVBoxWeb/internal/httpc"
	"github.com/xxl6097/TVBoxWeb/internal/imgcache"
	"github.com/xxl6097/TVBoxWeb/internal/live"
	"github.com/xxl6097/TVBoxWeb/internal/liveprobe"
	"github.com/xxl6097/TVBoxWeb/internal/metrics"
	"github.com/xxl6097/TVBoxWeb/internal/model"
	"github.com/xxl6097/TVBoxWeb/internal/netguard"
	"github.com/xxl6097/TVBoxWeb/internal/pinyin"
	"github.com/xxl6097/TVBoxWeb/internal/quality"
	"github.com/xxl6097/TVBoxWeb/internal/resolve"
	"github.com/xxl6097/TVBoxWeb/internal/store"
	"github.com/xxl6097/TVBoxWeb/internal/upgrade"
	"github.com/xxl6097/TVBoxWeb/internal/webdav"
)

// 版本信息（通过 ldflags -X 注入，默认值兜底）。
var (
	AppVersion = "v1.12.0"
	BuildTime  = "unknown"
)

// upgrader 升级器实例（如果启用）
var upgrader upgrade.Upgrader

// 升级相关类型别名
type (
	UpgradeProgress = upgrade.UpgradeProgress
	UpgradeStage    = upgrade.UpgradeStage
)

// 升级阶段常量
const (
	StageChecking    = upgrade.StageChecking
	StageDownloading = upgrade.StageDownloading
	StageVerifying   = upgrade.StageVerifying
	StageReplacing   = upgrade.StageReplacing
	StageDone        = upgrade.StageDone
)

// startTime 服务启动时间，用于健康检查返回 uptime。
var startTime time.Time

// wsHub 全局 WebSocket Hub 引用（由 main 设置）。
var wsHub interface{}

// SetStartTime 由 main 包设置服务启动时间。
func SetStartTime(t time.Time) {
	startTime = t
}

// SetWSHub 由 main 包设置全局 WebSocket Hub。
func SetWSHub(hub interface{}) {
	wsHub = hub
	// 初始化 store 包的 Hub 访问器
	store.InitWSHub(func() interface{} { return wsHub })
	// 注入 metrics 的 WebSocket 连接数获取器
	if h, ok := hub.(interface{ Size() int }); ok {
		metrics.SetWSConnectionGetter(func() int { return h.Size() })
	}
}

// SetUpgrader 设置升级器（由 main 包调用）
func SetUpgrader(u upgrade.Upgrader) {
	upgrader = u
}

// Handler 返回根路由。
func Handler() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", hHealth)
	// 登录（账号密码由上层库通过 tvbox.WithLogin 注入）
	mux.HandleFunc("/api/session", hSession)
	mux.HandleFunc("/api/login", hLogin)
	mux.HandleFunc("/api/logout", hLogout)
	mux.HandleFunc("/api/status", hStatus)
	mux.HandleFunc("/api/metrics", hMetrics)
	mux.HandleFunc("/api/version", hVersion)
	mux.HandleFunc("/api/upgrade/check", hUpgradeCheck)
	mux.HandleFunc("/api/upgrade/run", hUpgradeRun)
	mux.HandleFunc("/api/sites", hSites)
	mux.HandleFunc("/api/sites/enabled", hSitesEnabled)
	mux.HandleFunc("/api/sites/probe", hSitesProbe)
	mux.HandleFunc("/api/sites/speed", hSitesSpeed)
	mux.HandleFunc("/api/sites/health", hSitesHealth)
	mux.HandleFunc("/api/config", hConfig)
	mux.HandleFunc("/api/subscription", hSubscription)
	mux.HandleFunc("/api/subscriptions", hSubscriptionsList)
	mux.HandleFunc("/api/subscription/remove", hSubscriptionRemove)
	mux.HandleFunc("/api/subscription/toggle", hSubscriptionToggle)
	mux.HandleFunc("/api/subscriptions/bulk-enabled", hSubscriptionsBulkEnabled)
	mux.HandleFunc("/api/subscriptions/roots", hSubscriptionRoots)
	mux.HandleFunc("/api/subscriptions/sync", hSubscriptionsSync)
	mux.HandleFunc("/api/categories", hCategories)
	mux.HandleFunc("/api/catempty", hCategoriesEmpty)
	mux.HandleFunc("/api/vod", hVod)
	mux.HandleFunc("/api/vod/detail", hVodDetail)
	mux.HandleFunc("/api/vod/search", hVodSiteSearch)
	mux.HandleFunc("/api/search", hSearch)
	mux.HandleFunc("/api/search/start", hSearchStart)
	mux.HandleFunc("/api/search/poll", hSearchPoll)
	mux.HandleFunc("/api/suggest", hSuggest)
	mux.HandleFunc("/api/resolve", hResolve)
	mux.HandleFunc("/api/pinyin/abbr", hPinyinAbbr)

	mux.HandleFunc("/api/history", hHistory)
	mux.HandleFunc("/api/history/pos", hHistoryPos)
	mux.HandleFunc("/api/history/remove", hHistoryRemove)
	mux.HandleFunc("/api/history/clear", hHistoryClear)
	mux.HandleFunc("/api/favorites", hFavorites)
	mux.HandleFunc("/api/favorites/toggle", hFavToggle)
	mux.HandleFunc("/api/favorites/groups", hFavGroups)
	mux.HandleFunc("/api/favorites/group/create", hFavGroupCreate)
	mux.HandleFunc("/api/favorites/group/update", hFavGroupUpdate)
	mux.HandleFunc("/api/favorites/group/delete", hFavGroupDelete)
	mux.HandleFunc("/api/favorites/move", hFavMove)
	mux.HandleFunc("/api/favorites/batch/remove", hFavBatchRemove)
	mux.HandleFunc("/api/favorites/batch/move", hFavBatchMove)
	mux.HandleFunc("/api/favorites/import", hFavImport)
	mux.HandleFunc("/api/keywords", hKeywords)
	mux.HandleFunc("/api/keywords/remove", hKeywordRemove)
	mux.HandleFunc("/api/keywords/hot", hHotKeywords)

	mux.HandleFunc("/api/live/url", hLiveURL)
	mux.HandleFunc("/api/live/sources", hLiveSources)
	mux.HandleFunc("/api/live/sources/enabled", hLiveSourcesEnabled)
	mux.HandleFunc("/api/live/channel-status", hLiveChannelStatus)
	mux.HandleFunc("/api/live/channel-status/remove", hLiveChannelStatusRemove)
	mux.HandleFunc("/api/live/scan/reset", hLiveScanReset)
	mux.HandleFunc("/api/live/scan/status", hLiveScanStatus)
	mux.HandleFunc("/api/live/playable", hLivePlayable)
	mux.HandleFunc("/api/live/channels", hLiveChannels)
	mux.HandleFunc("/api/live/favs", hLiveFavs)
	mux.HandleFunc("/api/live/epg", hLiveEpg)
	mux.HandleFunc("/api/live/fav/toggle", hLiveFavToggle)
	mux.HandleFunc("/api/live/rank/one", hLiveRankOne)
	mux.HandleFunc("/api/live/rank", hLiveRank)
	mux.HandleFunc("/api/live/probe/channels", hLiveProbeChannels)
	mux.HandleFunc("/api/live/lines/probe", hLiveLinesProbe)
	mux.HandleFunc("/api/peek", hPeek)

	mux.HandleFunc("/api/crawl", hCrawl)
	mux.HandleFunc("/api/feed", hFeed)
	mux.HandleFunc("/api/quality/master", hQualityMaster)
	mux.HandleFunc("/api/prefs", hPrefs)
	mux.HandleFunc("/api/prefs/skip", hPrefsSkip)

	mux.HandleFunc("/api/webdav/sources", hWdSources)
	mux.HandleFunc("/api/webdav/probe", hWdProbe)
	mux.HandleFunc("/api/webdav/list", hWdList)
	mux.HandleFunc("/api/webdav/scan", hWdScan)
	mux.HandleFunc("/api/webdav/vods", hWdVods)
	mux.HandleFunc("/api/webdav/history", hWdHistory)
	mux.HandleFunc("/api/webdav/history/update", hWdHistoryUpdate)
	mux.HandleFunc("/api/webdav/history/remove", hWdHistoryRemove)
	mux.HandleFunc("/api/webdav/history/clear", hWdHistoryClear)

	mux.HandleFunc("/api/direct", hDirect)
	mux.HandleFunc("/api/privacy", hPrivacy)
	mux.HandleFunc("/api/backup", hBackup)
	mux.HandleFunc("/api/restore", hRestore)
	mux.HandleFunc("/api/reset", hReset)

	mux.HandleFunc("/api/proxy", hProxy)
	mux.HandleFunc("/api/img", hImg)
	// MKV 等浏览器不支持的容器：服务端实时转封装成 fMP4
	mux.HandleFunc("/api/remux", hRemux)
	// 用源 id 换播放所需的 Basic Auth（列表接口的密码是掩码，不能直接用）
	mux.HandleFunc("/api/webdav/auth", hWdAuth)
	return mux
}

// ---------- 工具 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(normalize(v))
}

// writeJSONWithCache 带缓存控制的 JSON 响应。
// maxAge 单位秒，etag 可选（空字符串则不设置）。
func writeJSONWithCache(w http.ResponseWriter, code int, v any, maxAge int, etag string) {
	if etag != "" {
		w.Header().Set("ETag", `"`+etag+`"`)
	}
	if maxAge > 0 {
		w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d, must-revalidate", maxAge))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(normalize(v))
}

// checkETag 检查请求的 If-None-Match，匹配则返回 304，返回 true 表示已响应。
func checkETag(w http.ResponseWriter, r *http.Request, etag string) bool {
	if etag == "" {
		return false
	}
	inm := r.Header.Get("If-None-Match")
	if inm == `"`+etag+`"` || inm == etag {
		w.WriteHeader(http.StatusNotModified)
		metrics.RecordCacheHit()
		return true
	}
	return false
}

// normalize 递归把 nil slice/map 转成空 slice/map，避免前端收到 null
// （前端模板普遍直接读 arr.length，null 会抛 TypeError 导致白屏）。
// depth 限制防止循环引用导致栈溢出。
func normalize(v any) any { return normalizeDepth(v, 0) }

func normalizeDepth(v any, depth int) any {
	if depth > 20 {
		return v // 超过深度限制，原样返回，防止循环引用
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return v
	}
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
		return normalizeDepth(rv.Elem().Interface(), depth+1)
	case reflect.Slice:
		if rv.IsNil() {
			return reflect.MakeSlice(rv.Type(), 0, 0).Interface()
		}
		out := reflect.MakeSlice(rv.Type(), rv.Len(), rv.Len())
		for i := 0; i < rv.Len(); i++ {
			n := normalizeDepth(rv.Index(i).Interface(), depth+1)
			if reflect.TypeOf(n).AssignableTo(rv.Type().Elem()) {
				out.Index(i).Set(reflect.ValueOf(n))
			}
		}
		return out.Interface()
	case reflect.Map:
		if rv.IsNil() {
			return reflect.MakeMap(rv.Type()).Interface()
		}
		out := reflect.MakeMap(rv.Type())
		iter := rv.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), reflect.ValueOf(normalizeDepth(iter.Value().Interface(), depth+1)))
		}
		return out.Interface()
	case reflect.Struct:
		out := reflect.New(rv.Type()).Elem()
		for i := 0; i < rv.NumField(); i++ {
			f := rv.Field(i)
			if f.CanInterface() {
				n := normalizeDepth(f.Interface(), depth+1)
				if reflect.TypeOf(n).AssignableTo(rv.Type().Field(i).Type) {
					out.Field(i).Set(reflect.ValueOf(n))
				} else {
					out.Field(i).Set(f)
				}
			}
		}
		return out.Interface()
	default:
		return v
	}
}

// ---------- 结构化错误码 ----------
// 前端 humanizeApiError() 据此给出可操作的中文建议，而不是只显示 HTTP 状态码。
const (
	ErrBadRequest = "ERR_BAD_REQUEST"
	ErrTimeout    = "ERR_TIMEOUT"
	ErrUpstream   = "ERR_UPSTREAM"
	ErrSite       = "ERR_SITE"
	ErrAuth       = "ERR_AUTH"
	ErrNotFound   = "ERR_NOT_FOUND"
	ErrNetwork    = "ERR_NETWORK"
	ErrInternal   = "ERR_INTERNAL"
)

// errCodeOf 根据 HTTP 状态与错误文案推断错误码，
// 这样所有现存 fail() 调用点无需改动即可获得统一错误码。
func errCodeOf(status int, msg string) string {
	s := strings.ToLower(msg)
	switch {
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline") ||
		strings.Contains(s, "timed out") || strings.Contains(s, "超时"):
		return ErrTimeout
	case strings.Contains(s, "站源") || strings.Contains(s, "site") ||
		strings.Contains(s, "播放源") || strings.Contains(s, "订阅"):
		return ErrSite
	case status == 400:
		return ErrBadRequest
	case status == 401 || status == 403:
		return ErrAuth
	case status == 404:
		return ErrNotFound
	case status == 502 || status == 503 || status == 504:
		return ErrUpstream
	default:
		return ErrInternal
	}
}

func ok(w http.ResponseWriter, v any) { writeJSON(w, 200, v) }

// fail 返回带错误码的 JSON 错误响应（全站统一）。
func fail(w http.ResponseWriter, status int, msg string) {
	failWith(w, status, errCodeOf(status, msg), msg)
}

// failWith 允许调用方显式指定错误码。
func failWith(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

const maskedPassword = "******"

// hBackup 导出整站数据（收藏 / 历史 / 配置 / 偏好 / 网盘源）。
func hBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	b, err := store.Snapshot()
	if err != nil {
		fail(w, 500, "导出备份失败："+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="yezhen-backup.json"`)
	w.WriteHeader(200)
	_, _ = w.Write(b)
}

// hRestore 导入备份（整体替换）。
func hRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		fail(w, 400, "读取备份文件失败："+err.Error())
		return
	}
	if len(raw) > 32<<20-1 {
		fail(w, 400, "备份文件过大（上限 32MB）")
		return
	}
	if err := store.Restore(raw); err != nil {
		fail(w, 400, err.Error())
		return
	}
	ok(w, map[string]any{"ok": true})
}

// hReset 恢复出厂设置。
func hReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	store.Reset()
	ok(w, map[string]any{"ok": true})
}

// guardURL 严格模式：给「内容回显型」接口用（/api/proxy、m3u8 解析）。
// 这类接口会把上游响应原样返回，允许访问内网等于把内网暴露成开放代理。
// 校验失败时写入结构化错误并返回 false，调用方直接 return。
func guardURL(w http.ResponseWriter, raw string) bool {
	if raw == "" {
		return true
	}
	if err := netguard.Check(raw); err != nil {
		failWith(w, http.StatusBadRequest, ErrBadRequest,
			"目标地址不安全："+err.Error()+"（若是你自己的 NAS/内网服务，请把它保存为 WebDAV 或订阅源，"+
				"保存后该域名会自动加入可信列表；或用 -allow-private 启动）")
		return false
	}
	return true
}

// guardSourceURL 源配置模式：给 WebDAV / 订阅 / 直播源 / EPG 等
// 「用户指向自己服务器」的接口用，允许私有/内网地址。
// 这一层不做内网判定，是因为拦在这里会让「家里 NAS」这个主流用法直接不可用；
// 真正需要防的是内容回显型接口，那一层由 guardURL 严格把关。
func guardSourceURL(w http.ResponseWriter, raw string) bool {
	if raw == "" {
		return true
	}
	if err := netguard.CheckSource(raw); err != nil {
		failWith(w, http.StatusBadRequest, ErrBadRequest, "目标地址不合法："+err.Error())
		return false
	}
	return true
}

// RefreshTrustedHosts 把用户已保存的源域名同步给 netguard。
// 导入订阅、保存网盘源、切换直播源/EPG 之后都要调用，否则
// 「公网域名在内网解析成 ULA」的地址仍会被严格模式拒绝。
func RefreshTrustedHosts() {
	seen := make(map[string]bool)
	var out []string
	for _, raw := range store.SourceURLs() {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		h := strings.ToLower(u.Hostname())
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	netguard.SetTrustedHosts(out)
}

func body(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func q(r *http.Request, k string) string { return r.URL.Query().Get(k) }

// ---------- 状态 / 站点 ----------

func hStatus(w http.ResponseWriter, r *http.Request) {
	_, ffmpegReason := FFmpegAvailable()
	ok(w, map[string]any{
		"isCustom":      store.IsCustom(),
		"subName":       store.SubName(),
		"configUrl":     store.GetConfigURL(),
		"subscriptions": len(store.Subscriptions()),
		"totalSites":    store.TotalSites(),
		"activeSites":   len(store.ActiveSites()),
		"feedCount":     len(store.Feed()),
		"lastCrawl":     store.LastCrawlAt(),
		"crawlSummary":  store.CrawlSummary(),
		"liveUrl":       store.LiveURL(),
		"privacyAgreed": store.PrivacyAgreed(),
		"guideDone":     store.GuideDone(),
		// ffmpeg 可用时，前端才把 MKV 之类的容器交给 /api/remux 转封装
		"remux":       ffmpegReason == "",
		"remuxReason": ffmpegReason,
	})
}

// hHealth 健康检查端点，用于 K8s livenessProbe / readinessProbe。
func hHealth(w http.ResponseWriter, r *http.Request) {
	uptime := int64(time.Since(startTime).Seconds())
	ok(w, map[string]any{
		"status":       "ok",
		"uptime":       uptime,
		"sites":        store.TotalSites(),
		"activeSites":  len(store.ActiveSites()),
		"lastCrawl":    store.LastCrawlAt(),
		"crawlSummary": store.CrawlSummary(),
		"time":         time.Now().Unix(),
	})
}

// hMetrics 性能监控端点，返回运行时指标（请求统计/缓存/内存/连接）。
func hMetrics(w http.ResponseWriter, r *http.Request) {
	ok(w, metrics.Snapshot())
}

// hVersion GET: 返回当前版本信息（appVersion/buildTime/osType/arch）。
func hVersion(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{
		"appVersion": AppVersion,
		"buildTime":  BuildTime,
		"osType":     runtime.GOOS,
		"arch":       runtime.GOARCH,
	})
}

// hUpgradeCheck GET: 检查是否有新版本。
func hUpgradeCheck(w http.ResponseWriter, r *http.Request) {
	if upgrader == nil {
		ok(w, map[string]any{
			"current":    AppVersion,
			"enabled":    false,
			"updateable": false,
			"error":      "本服务未启用在线升级",
		})
		return
	}
	rel, err := upgrader.Check(r.Context())
	if err != nil {
		ok(w, map[string]any{
			"current":    AppVersion,
			"enabled":    true,
			"updateable": false,
			"error":      err.Error(),
		})
		return
	}
	reply := map[string]any{
		"current":    AppVersion,
		"enabled":    true,
		"updateable": rel != nil,
	}
	if rel != nil {
		reply["release"] = rel
		reply["latest"] = rel.Version
	}
	ok(w, reply)
}

// hUpgradeRun POST: 执行升级，通过 SSE 推送进度。
func hUpgradeRun(w http.ResponseWriter, r *http.Request) {
	if upgrader == nil {
		fail(w, http.StatusNotFound, "本服务未启用在线升级")
		return
	}

	// 执行前重新向更新源确认版本，不信任客户端传入的任何 URL。
	rel, err := upgrader.Check(r.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, "无法向更新源确认版本: "+err.Error())
		return
	}
	if rel == nil {
		fail(w, http.StatusConflict, "当前已是最新版本")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, "当前环境不支持 SSE")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// SSE 写入器：并发安全由调用方保证，这里用锁。
	var mu sync.Mutex
	sse := func(event string, data any) {
		mu.Lock()
		defer mu.Unlock()
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	sse("progress", UpgradeProgress{Stage: StageChecking, Message: "开始升级到 " + rel.Version})
	err = upgrader.Upgrade(r.Context(), rel, func(p UpgradeProgress) {
		sse("progress", p)
	})
	if err != nil {
		sse("error", map[string]any{"message": err.Error()})
		return
	}
	sse("done", map[string]any{
		"version": rel.Version,
		"message": "升级完成，重启服务后生效",
	})
}

func hSites(w http.ResponseWriter, r *http.Request) {
	if store.TotalSites() == 0 {
		crawler.Restore()
	}
	// 生成 ETag（基于站源版本号 + 启用列表 + 最近一次内容更新时间，
	// 最后一项是因为 dead 标记来自内容更新，站源列表本身不变也要能刷新）
	explicitKeys := store.EnabledKeys()
	etag := fmt.Sprintf("sites-%d-%d-%d", store.SitesVersion(), len(explicitKeys), store.LastCrawlAt())
	// 检查 If-None-Match，匹配则返回 304
	if checkETag(w, r, etag) {
		return
	}
	// 若用户从未手动配置过 enabled 列表，默认全部 usable 站源均视为开启，
	// 与 EnabledSites() 的 fallback 逻辑保持一致，让设置页 badge 显示正确。
	useAll := len(explicitKeys) == 0
	enabled := map[string]bool{}
	for _, k := range explicitKeys {
		enabled[k] = true
	}
	dead := map[string]bool{}
	for _, k := range store.DeadKeys() {
		dead[k] = true
	}
	healthMap := store.SiteHealthMap()
	sites := store.Sites()
	out := make([]model.Site, len(sites))
	copy(out, sites)
	byAPI, _ := crawler.SiteLiveProvenance()
	for i := range out {
		if useAll {
			out[i].Enabled = out[i].Usable
		} else {
			out[i].Enabled = enabled[out[i].Key]
		}
		out[i].Dead = dead[out[i].Key]
		stat := healthMap[out[i].Key]
		out[i].Count = stat.LastTotal
		out[i].Severity = siteSeverity(stat, out[i].Count)
		out[i].Subs = byAPI[out[i].API]
	}
	// 设置缓存 5 分钟
	writeJSONWithCache(w, 200, out, 300, etag)
}

// siteSeverity 给 Count=0 的站源分个「严重程度」，供点播页下拉在数量都是
// 0 的站之间也能排出先后：
//
//	0 有内容（count>0），不用判断
//	1 最近一次内容更新连上了、解析成功，只是报告 0 条——站没坏，是真的空
//	2 最近一次内容更新没连上，但 24 小时内成功过——大概率只是暂时不可用
//	3 24 小时内没成功过（或者从没成功过）——更像过期域名/配置损坏/彻底失联
func siteSeverity(stat store.SiteHealthStat, count int) int {
	if count > 0 {
		return 0
	}
	if stat.LastCrawlOK {
		return 1
	}
	if stat.LastSuccess > 0 && time.Now().Unix()-stat.LastSuccess <= 86400 {
		return 2
	}
	return 3
}

// hSitesProbe 在用户手动启用一个「不可用」站源前探测它此刻能不能连通：
// 能取到分类列表才算通。失败时前端把开关拨回去并弹提示。
func hSitesProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	site := store.Site(req.Key)
	if site == nil {
		fail(w, 400, "站源不存在")
		return
	}
	if !strings.HasPrefix(site.API, "http") {
		fail(w, 400, "接口地址不是 HTTP，无法使用")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	cats, err := cms.New(*site).CategoriesCtx(ctx)
	if err != nil {
		fail(w, 502, "站源暂时连不上："+err.Error())
		return
	}
	if len(cats) == 0 {
		fail(w, 502, "站源连上了但没有返回任何分类")
		return
	}
	ok(w, map[string]any{"ok": true, "categories": len(cats)})
}

func hSitesEnabled(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Keys []string `json:"keys"`
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	store.SetEnabledKeys(req.Keys)
	// 只摘掉刚被停用的站源那些条目，其余保留——不用整个清空首页「最近
	// 更新」干等下一轮抓取，参考 crawler.RebuildFromSubscriptions 里同样
	// 的考虑。
	active := map[string]bool{}
	for _, s := range store.ActiveSites() {
		active[s.Key] = true
	}
	var kept []model.Vod
	for _, v := range store.Feed() {
		if active[v.SiteKey] {
			kept = append(kept, v)
		}
	}
	store.SaveFeed(kept)
	cms.ClearCache()
	ok(w, map[string]any{"ok": true})
}

// hSitesSpeed 站源测速：对每个可用站源发一次 ?ac=list，只读首包计时。
func hSitesSpeed(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Key  string `json:"key"`
		Name string `json:"name"`
		MS   int    `json:"ms"`
		OK   bool   `json:"ok"`
	}
	sites := store.UsableSites()
	var mu sync.Mutex
	var out []item
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, s := range sites {
		wg.Add(1)
		sem <- struct{}{}
		go func(s model.Site) {
			defer wg.Done()
			defer func() { <-sem }()
			c := cms.New(s)
			u := c.APIURL("ac=list")
			t0 := time.Now()
			ok := false
			if _, err := httpc.TextCtx(r.Context(), u, nil, 8*time.Second); err == nil {
				ok = true
			}
			ms := int(time.Since(t0).Milliseconds())
			// 同步记录健康度
			store.RecordSiteRequest(s.Key, ok, ms)
			mu.Lock()
			out = append(out, item{Key: s.Key, Name: s.Name, MS: ms, OK: ok})
			mu.Unlock()
		}(s)
	}
	wg.Wait()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].OK != out[j].OK {
			return out[i].OK
		}
		return out[i].MS < out[j].MS
	})
	ok(w, out)
}

// hSitesHealth 站源健康度状态汇总。
func hSitesHealth(w http.ResponseWriter, r *http.Request) {
	statuses := store.SiteHealthStatuses()
	ok(w, map[string]any{
		"statuses": statuses,
		"time":     time.Now().Unix(),
	})
}

// ---------- 配置 / 订阅 ----------

func hConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"`
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	n, err := crawler.LoadConfigCtx(r.Context(), req.Source)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	cms.ClearCache()
	RefreshTrustedHosts()
	ok(w, map[string]any{"count": n})
}

// hSubscription 添加（或按地址/文本原地更新）一条订阅源。可以多次调用来
// 同时订阅多个来源——站源/直播源/解析规则会按 crawler.RebuildFromSubscriptions
// 的规则合并去重，不会互相替换。
func hSubscription(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"`
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	res, err := crawler.LoadSubscriptionCtx(r.Context(), req.Source)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	cms.ClearCache()
	RefreshTrustedHosts()
	ok(w, res)
}

// hSubscriptionsList 列出当前所有订阅源（不含缓存的配置文本）。
func hSubscriptionsList(w http.ResponseWriter, r *http.Request) {
	ok(w, store.SubscriptionSummaries())
}

// hSubscriptionRoots GET：返回「订阅」文本框上次保存的原始根地址列表
// （一行一个，可能含聚合清单地址），供前端回填文本框。
func hSubscriptionRoots(w http.ResponseWriter, r *http.Request) {
	ok(w, store.SubscriptionRoots())
}

// hSubscriptionsSync POST：把文本框里的根地址列表（一行一个）递归展开成
// 叶子（单链接）订阅并同步，见 crawler.SyncSubscriptionRoots。
func hSubscriptionsSync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Roots []string `json:"roots"`
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	res, err := crawler.SyncSubscriptionRoots(r.Context(), req.Roots)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	cms.ClearCache()
	RefreshTrustedHosts()
	ok(w, res)
}

// hSubscriptionRemove 删除一条订阅，并用剩下已启用的订阅重新合并站源列表。
func hSubscriptionRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := body(r, &req); err != nil || req.ID == "" {
		fail(w, 400, "参数错误")
		return
	}
	if !store.RemoveSubscription(req.ID) {
		fail(w, 404, "订阅不存在")
		return
	}
	res := crawler.RebuildFromSubscriptions()
	cms.ClearCache()
	RefreshTrustedHosts()
	ok(w, res)
}

// hSubscriptionToggle 启用/停用一条订阅（不删除，保留缓存的配置文本，
// 方便随时重新启用），然后重新合并站源列表。
func hSubscriptionToggle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := body(r, &req); err != nil || req.ID == "" {
		fail(w, 400, "参数错误")
		return
	}
	if !store.SetSubscriptionEnabled(req.ID, req.Enabled) {
		fail(w, 404, "订阅不存在")
		return
	}
	res := crawler.RebuildFromSubscriptions()
	cms.ClearCache()
	RefreshTrustedHosts()
	ok(w, res)
}

// hSubscriptionsBulkEnabled 一次性批量启停多条订阅（设置页「单仓源」列表的
// 「全选 / 全部反选」）。不能直接在前端挨个调 hSubscriptionToggle——那样
// 几十条订阅就是几十次并发请求，每次还各自触发一遍 RebuildFromSubscriptions
// （重新解析全部订阅缓存文本、重建整份合并站源列表），在树莓派这种算力下
// 并发几十次这个量级的重建，互相抢占导致响应变慢甚至超时，表现为「点了
// 全部反选，结果只有一部分真的变了」。这里只在循环里做便宜的
// SetSubscriptionEnabled（纯内存字段赋值），重建只在最后做一次。
func hSubscriptionsBulkEnabled(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Updates map[string]bool `json:"updates"`
	}
	if err := body(r, &req); err != nil || len(req.Updates) == 0 {
		fail(w, 400, "参数错误")
		return
	}
	for id, enabled := range req.Updates {
		store.SetSubscriptionEnabled(id, enabled)
	}
	res := crawler.RebuildFromSubscriptions()
	cms.ClearCache()
	RefreshTrustedHosts()
	ok(w, res)
}

// ---------- 点播 ----------

func hCategories(w http.ResponseWriter, r *http.Request) {
	siteKey := q(r, "site")
	site := store.Site(siteKey)
	if site == nil {
		fail(w, 400, "站源不存在")
		return
	}
	// 生成 ETag（基于站源版本 + site key）
	etag := fmt.Sprintf("cat-%s-%d", siteKey, store.SitesVersion())
	if checkETag(w, r, etag) {
		return
	}
	c := cms.New(*site)
	t0 := time.Now()
	cats, err := c.CategoriesCtx(r.Context())
	ms := int(time.Since(t0).Milliseconds())
	store.RecordSiteRequest(siteKey, err == nil, ms)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	// 缓存 5 分钟
	writeJSONWithCache(w, 200, cats, 300, etag)
}

func hVod(w http.ResponseWriter, r *http.Request) {
	siteKey := q(r, "site")
	site := store.Site(siteKey)
	if site == nil {
		fail(w, 400, "站源不存在")
		return
	}
	tid := q(r, "tid")
	page, _ := strconv.Atoi(q(r, "page"))
	if page <= 0 {
		page = 1
	}
	c := cms.New(*site)
	if ext := q(r, "ext"); ext != "" {
		c.SetExtra(ext)
	}

	// 父分类聚合：前端把该父分类的子分类 id 用 children 参数传入，
	// 命中时合并父 + 子分类第一页（按 vod id 去重）。
	var pg model.VodPage
	var err error
	t0 := time.Now()
	if children := q(r, "children"); children != "" {
		ids := []string{tid}
		for _, id := range strings.Split(children, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
		pg, err = c.ListMergedCtx(r.Context(), ids, page)
	} else {
		pg, err = c.ListCtx(r.Context(), tid, page)
	}
	ms := int(time.Since(t0).Milliseconds())
	store.RecordSiteRequest(siteKey, err == nil, ms)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, pg)
}

func hVodDetail(w http.ResponseWriter, r *http.Request) {
	siteKey := q(r, "site")
	site := store.Site(siteKey)
	if site == nil {
		fail(w, 400, "站源不存在")
		return
	}
	id := q(r, "id")
	c := cms.New(*site)
	t0 := time.Now()
	v, err := c.DetailCtx(r.Context(), id)
	ms := int(time.Since(t0).Milliseconds())
	store.RecordSiteRequest(siteKey, err == nil, ms)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, v)
}

// hVodSiteSearch 点播 tab 的「筛选片名」：只在当前选中的这一个站源内搜，
// 走站源自己的搜索接口（跟多站搜索共用同一个 SearchCtx），不是只在已经
// 加载到本地的这一页里做字符串包含匹配——站源的片单通常有成千上万条，
// 只在当前这一页（二三十条）里过滤基本搜不到东西。
func hVodSiteSearch(w http.ResponseWriter, r *http.Request) {
	siteKey := q(r, "site")
	site := store.Site(siteKey)
	if site == nil {
		fail(w, 400, "站源不存在")
		return
	}
	kw := strings.TrimSpace(q(r, "kw"))
	if kw == "" {
		ok(w, []model.Vod{})
		return
	}
	c := cms.New(*site)
	t0 := time.Now()
	list := c.SearchCtx(r.Context(), kw)
	ms := int(time.Since(t0).Milliseconds())
	store.RecordSiteRequest(siteKey, true, ms)
	ok(w, list)
}

// hSearch 多站并发搜索（每批 6，最多 18 站，单站失败不影响整体）。
func hSearch(w http.ResponseWriter, r *http.Request) {
	kw := q(r, "kw")
	if kw == "" {
		ok(w, map[string]any{"list": []model.Vod{}, "sites": 0})
		return
	}
	sites := store.ActiveSites()
	var searchable []model.Site
	for _, s := range sites {
		if s.Searchable {
			searchable = append(searchable, s)
		}
	}
	if len(searchable) > 18 {
		searchable = searchable[:18]
	}
	type result struct {
		site model.Site
		vods []model.Vod
	}
	ch := make(chan result, len(searchable))
	sem := make(chan struct{}, 6)
	for _, s := range searchable {
		sem <- struct{}{}
		go func(s model.Site) {
			defer func() { <-sem }()
			c := cms.New(s)
			ch <- result{s, c.SearchCtx(r.Context(), kw)}
		}(s)
	}
	var list []model.Vod
	alive := 0
	// 搜索结果去重：用 Name+SiteKey 作为唯一键
	seen := map[string]bool{}
	for range searchable {
		res := <-ch
		if len(res.vods) > 0 {
			alive++
			for _, v := range res.vods {
				key := v.Name + "|" + v.SiteKey
				if !seen[key] {
					seen[key] = true
					list = append(list, v)
				}
			}
		}
	}

	// 拼音首字母搜索：关键词是纯 ASCII（如 sgyy）时，站源按中文搜往往返回空，
	// 这里用拼音首字母缩写从「最近更新 feed」里匹配片名，作为本地联想补充。
	if isAsciiLetters(kw) {
		for _, v := range store.Feed() {
			if !seen[v.Name+"|"+v.SiteKey] && pinyin.MatchAbbr(v.Name, kw) {
				seen[v.Name+"|"+v.SiteKey] = true
				list = append(list, v)
			}
		}
	}

	ok(w, map[string]any{"list": list, "sites": alive, "total": len(searchable)})
}

// isAsciiLetters 判断是否为纯 ASCII 字母（可能是拼音首字母缩写）。
func isAsciiLetters(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}

// hSuggest 搜索联想：从「最近更新 feed」+「搜索历史」里做片名联想，
// 支持中文前缀/包含、拼音首字母缩写（sgyy→三国演义）、T9 数字串。
func hSuggest(w http.ResponseWriter, r *http.Request) {
	kw := strings.TrimSpace(q(r, "kw"))
	if kw == "" {
		ok(w, []string{})
		return
	}
	seen := map[string]bool{}
	var pool []string
	for _, v := range store.Feed() {
		if v.Name != "" && !seen[v.Name] {
			seen[v.Name] = true
			pool = append(pool, v.Name)
		}
	}
	for _, k := range store.Keywords() {
		if k != "" && !seen[k] {
			seen[k] = true
			pool = append(pool, k)
		}
	}
	var out []string
	for _, name := range pool {
		if pinyin.MatchAbbr(name, kw) {
			out = append(out, name)
		}
	}
	// 排序：前缀/包含优先，拼音命中的靠后
	sort.SliceStable(out, func(i, j int) bool {
		ci := strings.Contains(out[i], kw)
		cj := strings.Contains(out[j], kw)
		if ci != cj {
			return ci
		}
		pi := strings.HasPrefix(out[i], kw)
		pj := strings.HasPrefix(out[j], kw)
		return pi && !pj
	})
	if len(out) > 12 {
		out = out[:12]
	}
	ok(w, out)
}

func hResolve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	ok(w, resolve.ResolveCtx(r.Context(), req.URL, store.Parses()))
}

func hPinyinAbbr(w http.ResponseWriter, r *http.Request) {
	word := q(r, "word")
	ok(w, map[string]any{"abbrs": pinyin.AbbrsOf(word)})
}

// ---------- 历史 / 收藏 / 关键词 ----------

func hHistory(w http.ResponseWriter, r *http.Request) { ok(w, store.History()) }

func hHistoryPos(w http.ResponseWriter, r *http.Request) {
	var h store.HistItem
	if err := body(r, &h); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	store.UpdateHistoryPos(h)
	ok(w, map[string]any{"ok": true})
}

func hHistoryRemove(w http.ResponseWriter, r *http.Request) {
	var h store.HistItem
	if err := body(r, &h); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	store.RemoveHistory(h)
	ok(w, map[string]any{"ok": true})
}

func hHistoryClear(w http.ResponseWriter, r *http.Request) {
	store.ClearHistory()
	ok(w, map[string]any{"ok": true})
}

func hFavorites(w http.ResponseWriter, r *http.Request) { ok(w, store.Favorites()) }

func hFavToggle(w http.ResponseWriter, r *http.Request) {
	var f store.FavItem
	if err := body(r, &f); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	added := store.ToggleFavorite(f)
	ok(w, map[string]any{"added": added})
}

// ---------- 收藏分组 API ----------

// hFavGroups GET: 获取所有分组列表。
func hFavGroups(w http.ResponseWriter, r *http.Request) {
	groups := store.FavGroupList()
	// 为每个分组统计收藏数量
	favs := store.Favorites()
	countMap := map[string]int{"": 0} // "" = 默认分组
	for _, f := range favs {
		countMap[f.GroupID]++
	}
	type groupWithCount struct {
		store.FavGroup
		Count int `json:"count"`
	}
	out := []groupWithCount{}
	for _, g := range groups {
		out = append(out, groupWithCount{FavGroup: g, Count: countMap[g.ID]})
	}
	ok(w, map[string]any{
		"groups":       out,
		"defaultCount": countMap[""],
		"total":        len(favs),
	})
}

// hFavGroupCreate POST: 创建新分组。
func hFavGroupCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := body(r, &req); err != nil || req.Name == "" {
		fail(w, 400, "缺少分组名称")
		return
	}
	if len(req.Name) > 20 {
		fail(w, 400, "分组名称不能超过 20 个字符")
		return
	}
	g := store.CreateFavGroup(req.Name)
	ok(w, g)
}

// hFavGroupUpdate POST: 修改分组名称。
func hFavGroupUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := body(r, &req); err != nil || req.ID == "" || req.Name == "" {
		fail(w, 400, "缺少参数 id 或 name")
		return
	}
	if !store.UpdateFavGroup(req.ID, req.Name) {
		fail(w, 404, "分组不存在")
		return
	}
	ok(w, map[string]any{"ok": true})
}

// hFavGroupDelete POST: 删除分组（该组收藏自动归入默认分组）。
func hFavGroupDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := body(r, &req); err != nil || req.ID == "" {
		fail(w, 400, "缺少参数 id")
		return
	}
	if !store.DeleteFavGroup(req.ID) {
		fail(w, 404, "分组不存在")
		return
	}
	ok(w, map[string]any{"ok": true})
}

// hFavMove POST: 将收藏移入指定分组。
func hFavMove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SiteKey string `json:"siteKey"`
		VodID   string `json:"vodId"`
		GroupID string `json:"groupId"` // 空字符串 = 默认分组
	}
	if err := body(r, &req); err != nil || req.SiteKey == "" || req.VodID == "" {
		fail(w, 400, "缺少参数 siteKey 或 vodId")
		return
	}
	if !store.MoveFav(req.SiteKey, req.VodID, req.GroupID) {
		fail(w, 404, "收藏不存在")
		return
	}
	ok(w, map[string]any{"ok": true})
}

// hFavBatchRemove POST: 批量删除收藏。
func hFavBatchRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Items []store.FavItem `json:"items"`
	}
	if err := body(r, &req); err != nil || len(req.Items) == 0 {
		fail(w, 400, "缺少 items 参数")
		return
	}
	removed := store.BatchRemoveFavorites(req.Items)
	ok(w, map[string]any{"removed": removed})
}

// hFavBatchMove POST: 批量移动收藏到分组。
func hFavBatchMove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Items   []store.FavItem `json:"items"`
		GroupID string          `json:"groupId"`
	}
	if err := body(r, &req); err != nil || len(req.Items) == 0 {
		fail(w, 400, "缺少 items 参数")
		return
	}
	moved := store.BatchMoveFavorites(req.Items, req.GroupID)
	ok(w, map[string]any{"moved": moved})
}

// hFavImport POST: 批量导入收藏（去重合并）。
func hFavImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Items []store.FavItem `json:"items"`
	}
	if err := body(r, &req); err != nil || len(req.Items) == 0 {
		fail(w, 400, "缺少 items 参数")
		return
	}
	added, dup := store.ImportFavorites(req.Items)
	ok(w, map[string]any{"added": added, "duplicate": dup})
}

func hKeywords(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ok(w, store.Keywords())
	case http.MethodPost:
		var req struct {
			Kw string `json:"kw"`
		}
		_ = body(r, &req)
		ok(w, store.PushKeyword(req.Kw))
	case http.MethodDelete:
		store.ClearKeywords()
		ok(w, map[string]any{"ok": true})
	default:
		fail(w, 405, "method not allowed")
	}
}

// hKeywordRemove POST: 删除单个搜索历史词。
func hKeywordRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kw string `json:"kw"`
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	ok(w, store.RemoveKeyword(req.Kw))
}

// hHotKeywords GET: 热门搜索词（从 feed 提取）。
func hHotKeywords(w http.ResponseWriter, r *http.Request) {
	ok(w, store.HotKeywords())
}

// ---------- 直播 ----------

func hLiveURL(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ok(w, map[string]string{"url": store.LiveURL()})
	case http.MethodPost:
		var req struct {
			URL string `json:"url"`
		}
		_ = body(r, &req)
		store.SetLiveURL(req.URL)
		RefreshTrustedHosts()
		ok(w, map[string]any{"ok": true})
	}
}

func hLiveSources(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list := store.SubLives()
		explicit := store.EnabledLiveURLs()
		useAll := len(explicit) == 0
		enabled := map[string]bool{}
		for _, u := range explicit {
			enabled[u] = true
		}
		_, byLiveURL := crawler.SiteLiveProvenance()
		out := make([]model.RawLive, len(list))
		copy(out, list)
		for i := range out {
			out[i].Enabled = useAll || enabled[out[i].URL]
			out[i].Subs = byLiveURL[out[i].URL]
		}
		ok(w, out)
	case http.MethodPost:
		var req struct {
			Lives []model.RawLive `json:"lives"`
		}
		_ = body(r, &req)
		store.SetSubLives(req.Lives)
		if len(req.Lives) > 0 && store.LiveURL() == "" {
			store.SetLiveURL(req.Lives[0].URL)
		}
		ok(w, map[string]any{"ok": true})
	}
}

// hLiveSourcesEnabled POST {urls}：设置哪些「直播源」参与直播频道合并，
// 跟 hSitesEnabled 对站源的语义一致（urls 为空 = 全部启用）。
func hLiveSourcesEnabled(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URLs []string `json:"urls"`
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	store.SetEnabledLiveURLs(req.URLs)
	ok(w, map[string]any{"ok": true})
}

// hLiveFavs GET: 返回收藏的直播频道列表。
// hLiveEpg GET /api/live/epg?name=频道名&url=EPG地址（url 省略时用设置里保存的地址）
func hLiveEpg(w http.ResponseWriter, r *http.Request) {
	u := q(r, "url")
	if u == "" {
		u = store.EpgURL()
	}
	if !guardSourceURL(w, u) {
		return
	}
	res, err := live.FetchEPGCtx(r.Context(), u, q(r, "name"))
	if err != nil {
		status, code := 502, ErrUpstream
		if strings.Contains(err.Error(), "尚未配置") || strings.Contains(err.Error(), "缺少频道") {
			status, code = 400, ErrBadRequest
		}
		failWith(w, status, code, err.Error())
		return
	}
	ok(w, res)
}

func hLiveFavs(w http.ResponseWriter, r *http.Request) {
	ok(w, store.LiveFavs())
}

// hLiveFavToggle POST: 切换直播频道收藏状态。
func hLiveFavToggle(w http.ResponseWriter, r *http.Request) {
	var ch model.Channel
	if err := body(r, &ch); err != nil || ch.Name == "" || ch.URL == "" {
		fail(w, 400, "缺少频道信息")
		return
	}
	added := store.ToggleLiveFav(ch)
	ok(w, map[string]any{"added": added})
}

// liveAllSources 汇总「订阅合并出的直播源」+「设置里手动填的直播源」，按 URL
// 去重，并按 store.EnabledLiveURLs() 过滤掉用户手动停用的直播源（空 = 全部
// 启用）——跟「站源」的启停语义一致，拉黑某个直播源之后它就不会再出现在
// 合并后的频道目录里。
func liveAllSources() []model.RawLive {
	explicit := store.EnabledLiveURLs()
	useAll := len(explicit) == 0
	enabled := map[string]bool{}
	for _, u := range explicit {
		enabled[u] = true
	}
	seen := map[string]bool{}
	var out []model.RawLive
	for _, s := range store.SubLives() {
		if s.URL == "" || seen[s.URL] || (!useAll && !enabled[s.URL]) {
			continue
		}
		seen[s.URL] = true
		out = append(out, s)
	}
	if manual := store.LiveURL(); manual != "" && !seen[manual] && (useAll || enabled[manual]) {
		out = append(out, model.RawLive{Name: "手动直播源", URL: manual})
	}
	return out
}

// hLiveChannels GET：返回「所有已启用直播源合并」后的统一频道目录——同一个
// 频道（比如 CCTV1）的所有线路都会在 Channel.urls 里，前端播放时可以展示全部
// 线路供手动切换，也用于播放器里的自动测速切换。
//
// 每个直播源的抓取结果都落了磁盘缓存（见 live.Channels/store.LiveCacheEntry），
// 超过 live.CacheMaxAge（1 天）才需要重新联网；哪怕过期/缺失也永远不在这次
// 请求里等网络——直接用缓存里现有的（哪怕是旧的）拼一份结果立即返回，缺失/
// 过期的源在后台异步刷新，下次请求自然拿到新结果。跟 /api/feed 对点播列表
// 的处理是同一套思路。
//
// 带 ?url= 时走旧的单源模式（兼容用途，正常不会用到）。
// 带 ?refresh=1 时额外让后台把没过期的缓存也重新抓一遍（用户手动点刷新）。
func hLiveChannels(w http.ResponseWriter, r *http.Request) {
	if u := q(r, "url"); u != "" {
		if !guardSourceURL(w, u) {
			return
		}
		groups, err := live.LoadCtx(r.Context(), u, 25*time.Second)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		groups = postProcessLive(groups)
		root := live.BuildTree(groups)
		ok(w, map[string]any{"root": root, "groups": groups, "url": u})
		return
	}

	sources := liveAllSources()
	if len(sources) == 0 {
		fail(w, 400, "暂无直播源，先去「设置 → 订阅」导入")
		return
	}
	force := q(r, "refresh") == "1"
	groups, errs := live.Channels(sources, force)
	groups = postProcessLive(groups)
	root := live.BuildTree(groups)
	resp := map[string]any{"root": root, "sources": len(sources), "errs": errs}
	// 不再在响应里重复带一份「groups」：root 树里已经包含了全部频道
	// （分组目录的叶子节点），前端也从来没用过这份多出来的拍平列表——
	// 这个接口本来就要把十几万条频道序列化下发，不必再平白无故翻倍。
	//
	// 短 max-age：这份结果本来就已经是 live.Channels 内部 mergeCacheTTL
	// 缓存过的「不保证是这一秒最新」的快照，浏览器在这几秒内重复请求
	// （比如切回直播页触发的静默刷新）直接吃 HTTP 缓存，不用再传一遍
	// 几十 MB。force=refresh 时不设缓存头，保证「刷新」按钮永远拿到新结果。
	if force {
		ok(w, resp)
		return
	}
	writeJSONWithCache(w, 200, resp, 15, "")
}

// postProcessLive 统一排序 + （按用户设置）过滤脏数据，merge 后的原始缓存
// 内容本身不受过滤设置影响——这样用户切换「过滤垃圾频道」开关不需要清缓存
// 重新抓一遍，下次请求当场就能看到效果。
func postProcessLive(groups []model.ChannelGroup) []model.ChannelGroup {
	if store.LiveJunkFilterOn() {
		groups = live.FilterJunk(groups)
	}
	live.SortGroups(groups)
	annotateChannelStatus(groups)
	annotateLineScores(groups)
	return groups
}

// annotateChannelStatus 给每个频道填上「真实播放过」的结果（见
// store.ChannelStatus），供前端按「播过且能播/播过但不行/没播过」三态上色。
// 现查现填，不参与缓存/去重——播放状态变了，下次打开直播页立刻能看到新结果。
func annotateChannelStatus(groups []model.ChannelGroup) {
	for gi := range groups {
		chs := groups[gi].Channels
		for ci := range chs {
			if ok, known := store.GetChannelStatus(chs[ci].Name); known {
				if ok {
					chs[ci].Status = "ok"
				} else {
					chs[ci].Status = "fail"
				}
			}
		}
	}
}

// annotateLineScores 把每个频道的备用线路（URLs）按可靠度分数从高到低
// 排序，并让主地址（URL）跟着指向排完序后的第一条——播放器总是先试
// URL、播不通再依次试 URLs 里的下一条，这里把「历史上最靠谱」的线路挪到
// 最前面，用户不用等它一条条试过那些已知会失败的线路才轮到真正能播的
// 那条。一次性取整份分数快照（SnapshotLineScores），而不是给每个频道的
// 每条线路都单独查一次——全量频道目录常有十几万条，省下反复加锁的开销。
func annotateLineScores(groups []model.ChannelGroup) {
	scores := store.SnapshotLineScores()
	for gi := range groups {
		annotateLineScoresFlat(groups[gi].Channels, scores)
	}
}

// annotateLineScoresFlat 跟 annotateLineScores 是同一件事，只是作用在一份
// 拍平的频道列表上——给 hLivePlayable 这种不经过 postProcessLive/
// ChannelGroup 结构、直接拍平成 []model.Channel 的接口复用，避免两套
// 排序逻辑分叉。
func annotateLineScoresFlat(chs []model.Channel, scores map[string]int) {
	for ci := range chs {
		urls := chs[ci].URLs
		if len(urls) == 0 {
			continue
		}
		sorted := store.SortURLsByScore(urls, scores)
		chs[ci].URLs = sorted
		chs[ci].URL = sorted[0]
	}
}

// hLiveChannelStatus POST {name, ok}：记录一次直播频道「真实播放」的结果——
// 播放页在 <video> 真正起播（onplaying）或者判定这个频道所有线路都播不通
// 时各调一次，不是靠 /api/peek 那种轻量连通性探测（那只能判断「连得上」，
// 不代表「播得出来」，两者在这个功能上的语义正好相反——一条线连不上但
// 换了别的线播成了，频道整体还是「能播」）。
func hLiveChannelStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
	}
	if err := body(r, &req); err != nil || req.Name == "" {
		fail(w, http.StatusBadRequest, "参数错误")
		return
	}
	store.SetChannelStatus(req.Name, req.OK)
	ok(w, map[string]any{"ok": true})
}

// hLiveLinesProbe POST：轻量批量测一批播放线路的延迟/粗略吞吐（只取开头
// 16KB，不下载真正的视频数据），用于「当前频道所有线路」的质量监控，
// 判断要不要自动切换到更好的线路。
func hLiveLinesProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URLs []string `json:"urls"`
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	if len(req.URLs) > 16 {
		req.URLs = req.URLs[:16]
	}
	for _, u := range req.URLs {
		if !guardSourceURL(w, u) {
			return
		}
	}
	results := live.ProbeLinesCtx(r.Context(), req.URLs, 4*time.Second)
	// 播放页每隔几秒就会轮询一次这个接口，顺手把每条线路的结果记进可靠度
	// 分数——这是比后台扫描更频繁、更贴近"用户这会儿实际在看"的信号，
	// 下次这个频道下发线路列表时排序会更准。
	for _, lq := range results {
		store.RecordLineOutcomeBulk(lq.URL, lq.OK, int64(lq.MS))
	}
	store.FlushLiveCache()
	ok(w, results)
}

func hLiveRankOne(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	ok(w, liveprobe.RankOne(req.Name, req.URL))
}

func hLiveRank(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Items   []liveprobe.LiveScore `json:"items"`
		Workers int                   `json:"workers"` // 并发数，0 自动为 8
		Timeout int                   `json:"timeout"` // 超时秒数，0 无限制
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	workers := req.Workers
	if workers <= 0 {
		workers = 8
	}
	timeout := time.Duration(req.Timeout) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second // 默认 30s 超时
	}
	ranked := liveprobe.RankAllConcurrent(req.Items, workers, timeout, nil)
	ok(w, liveprobe.Sort(ranked))
}

func hLiveProbeChannels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channels []model.Channel `json:"channels"`
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	if len(req.Channels) > 40 {
		req.Channels = req.Channels[:40]
	}
	dead := liveprobe.Channels(req.Channels, func(ch model.Channel, alive bool) {}, 4)
	ok(w, map[string]any{"dead": dead, "total": len(req.Channels)})
}

// hPeek 单条流探活（直播换台前用）。
func hPeek(w http.ResponseWriter, r *http.Request) {
	u := q(r, "url")
	if u == "" {
		fail(w, 400, "缺少 url")
		return
	}
	if !guardSourceURL(w, u) {
		return
	}
	alive := live.ProbeCtx(r.Context(), u, 2500*time.Millisecond)
	ok(w, map[string]any{"alive": alive})
}

// ---------- 抓取 / 首页 ----------

func hCrawl(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		processed, total, running := crawler.Progress()
		ok(w, map[string]any{
			"summary":   store.CrawlSummary(),
			"last":      store.LastCrawlAt(),
			"running":   running,
			"processed": processed,
			"total":     total,
		})
	case http.MethodPost:
		// 全站抓取是后台按 crawler.CrawlInterval 预先跑的（见 startScheduler），
		// 不是「这次调用」现抓现等——这里点一下（首页自动触发也好，用户手动点
		// 「更新」也好）只决定「要不要在后台补跑一轮」，从不在这次请求里等
		// 它跑完，也从不因为点了一下就把当前列表清空：GET /api/feed 永远
		// 立刻原样返回当前缓存的列表，新结果等后台这轮抓完了自然替换上去。
		switch {
		case crawler.IsRunning():
			ok(w, map[string]any{"started": false, "running": true})
		case !crawler.DueForRefresh():
			ok(w, map[string]any{"started": false, "running": false})
		default:
			go crawler.Run(true)
			ok(w, map[string]any{"started": true})
		}
	}
}

func hFeed(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"feed": store.Feed(), "deadKeys": store.DeadKeys(), "summary": store.CrawlSummary()})
}

func hQualityMaster(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	if req.URL == "" || !strings.HasPrefix(req.URL, "http") {
		ok(w, []quality.QLevel{})
		return
	}
	if !guardURL(w, req.URL) {
		return
	}
	txt, err := httpc.TextCtx(r.Context(), req.URL, nil, 6*time.Second)
	if err != nil {
		ok(w, []quality.QLevel{})
		return
	}
	ok(w, quality.ParseMaster(txt, req.URL))
}

func hPrefs(w http.ResponseWriter, r *http.Request) {
	// 片头片尾设置按剧生效。带 skipA/skipB（点播是 site+vodId，网盘是 srcId+vodName）
	// 时读写的都是「这部剧」的设置，不带则退回全局默认值。
	skipA, skipB := q(r, "skipA"), q(r, "skipB")
	hasSkipScope := skipA != "" && skipB != ""

	switch r.Method {
	case http.MethodGet:
		curSkip, ownSkip := store.FindSkipCfg(skipA, skipB)
		ok(w, map[string]any{
			"qPref":      store.QPref(),
			"speedPref":  store.SpeedPref(),
			"volumePref": store.VolumePref(),
			"epgUrl":     store.EpgURL(),
			// 全局默认值（没有单独设置过的剧用它）
			"introSkip": store.IntroSkipSec(),
			"outroSkip": store.OutroSkipSec(),
			// 当前这部剧实际生效的值，以及它是否有单独设置
			"curIntroSkip":    curSkip.Intro,
			"curOutroSkip":    curSkip.Outro,
			"skipOwn":         ownSkip,
			"skipScoped":      hasSkipScope,
			"tapSound":        store.TapSound(),
			"tapVibrate":      store.TapVibrate(),
			"skipCfgCount":    store.SkipCfgCount(),
			"liveJunkFilter":  store.LiveJunkFilterOn(),
			"liveScanEnabled": store.LiveScanEnabled(),
		})
	case http.MethodPost:
		var req map[string]any
		if err := body(r, &req); err != nil {
			fail(w, 400, "参数错误")
			return
		}
		if v, ok := req["qPref"]; ok {
			store.SetQPref(int(toFloat(v)))
		}
		if v, ok := req["speedPref"]; ok {
			store.SetSpeedPref(toFloat(v))
		}
		if v, ok := req["volumePref"]; ok {
			store.SetVolumePref(toFloat(v))
		}
		if v, ok := req["epgUrl"]; ok {
			if s, ok := v.(string); ok {
				store.SetEpgURL(strings.TrimSpace(s))
			}
		}
		// 只有不带 skipA/skipB（即「设置页里改全局默认」）时才写全局值。
		// 否则播放页按剧保存片头片尾时会把全局默认一起改掉，
		// 等于所有剧又被同一套值覆盖 —— 那正是这次要修的问题。
		if !hasSkipScope {
			if v, ok := req["introSkip"]; ok {
				store.SetIntroSkipSec(int(toFloat(v)))
			}
			if v, ok := req["outroSkip"]; ok {
				store.SetOutroSkipSec(int(toFloat(v)))
			}
		}
		// 按剧保存片头片尾：带上 skipA/skipB 时只影响「这部剧」，
		// 两个值都置 0 会删掉本剧记录、回落到全局默认。
		// skipGlobal=1 表示额外把这对值设为所有剧的默认值。
		if hasSkipScope {
			prev, _ := store.FindSkipCfg(skipA, skipB)
			intro, outro := prev.Intro, prev.Outro
			if v, ok := req["introSkip"]; ok {
				intro = int(toFloat(v))
			}
			if v, ok := req["outroSkip"]; ok {
				outro = int(toFloat(v))
			}
			store.SetSkipCfg(skipA, skipB, intro, outro)
			if g, ok := req["skipGlobal"]; ok && toFloat(g) != 0 {
				store.SetIntroSkipSec(intro)
				store.SetOutroSkipSec(outro)
			}
		}
		if v, ok := req["tapSound"]; ok {
			store.SetTapSound(int(toFloat(v)))
		}
		if v, ok := req["tapVibrate"]; ok {
			store.SetTapVibrate(int(toFloat(v)))
		}
		if v, ok := req["liveJunkFilter"]; ok {
			if b, ok2 := v.(bool); ok2 {
				store.SetLiveJunkFilterOn(b)
			}
		}
		if v, ok := req["liveScanEnabled"]; ok {
			if b, ok2 := v.(bool); ok2 {
				store.SetLiveScanEnabled(b)
			}
		}
		// EPG 地址可能指向内网 NAS，变了就刷新可信域名
		RefreshTrustedHosts()
		ok(w, map[string]any{"ok": true})
	}
}

// hPrefsSkip 管理「某一部剧」的片头片尾跳过设置。
//
//	DELETE /api/prefs/skip?skipA=<site>&skipB=<vodId>   清掉本剧单独设置，回落全局默认
//	GET    /api/prefs/skip?skipA=&skipB=                查本剧当前生效值与来源
func hPrefsSkip(w http.ResponseWriter, r *http.Request) {
	a, b := q(r, "skipA"), q(r, "skipB")
	if a == "" || b == "" {
		fail(w, 400, "缺少 skipA / skipB")
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg, own := store.FindSkipCfg(a, b)
		ok(w, map[string]any{"intro": cfg.Intro, "outro": cfg.Outro, "own": own})
	case http.MethodDelete:
		store.DeleteSkipCfg(a, b)
		ok(w, map[string]any{"ok": true})
	default:
		fail(w, 405, "不支持的方法")
	}
}

// toFloat 把 JSON 反序列化出的数值统一转成 float64。
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}

// ---------- WebDAV ----------

// hWdAuth 用源 id 换出「播放所需的 Basic Auth」。
//
// 为什么需要这个接口：GET /api/webdav/sources 出于安全不回显密码（返回 ******），
// 而播放时前端要拼 `Authorization: Basic base64(user:pass)` 给 /api/proxy。
// 早先前端直接拿列表里的密码拼，掩码上线后拼出 webdav:****** → 上游 401。
// 期间之所以「看起来能用」，是因为 /webdav/sources 被缓存进 IndexedDB，
// 缓存里还是掩码上线前的旧数据；加登录后每次登录都会清缓存，问题才暴露。
//
// 这里只返回 Auth 头（不含明文密码本身以外的东西），且必须通过鉴权。
func hWdAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	id := q(r, "id")
	if id == "" {
		fail(w, http.StatusBadRequest, "缺少 id")
		return
	}
	for _, s := range store.WebdavSources() {
		if s.ID != id {
			continue
		}
		// Basic Auth 里用 UTF-8 用户名/密码
		raw := s.User + ":" + s.Pass
		ok(w, map[string]string{
			"auth": base64.StdEncoding.EncodeToString([]byte(raw)),
		})
		return
	}
	fail(w, http.StatusNotFound, "找不到该网盘源")
}

func hWdSources(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// 密码永不回显给前端（即使开了 CORS 白名单）
		srcs := store.WebdavSources()
		out := make([]model.WebdavSource, len(srcs))
		copy(out, srcs)
		for i := range out {
			if out[i].Pass != "" {
				out[i].Pass = maskedPassword
			}
		}
		ok(w, out)
	case http.MethodPost:
		var src model.WebdavSource
		if err := body(r, &src); err != nil {
			fail(w, 400, "参数错误")
			return
		}
		if !guardSourceURL(w, src.URL) {
			return
		}
		// 掩码或留空都表示「沿用已存密码」，避免「查看即清空密码」或存成星号
		resolveWdPass(&src)
		store.SaveWebdavSource(src)
		RefreshTrustedHosts()
		ok(w, map[string]any{"ok": true})
	case http.MethodDelete:
		store.RemoveWebdavSource(q(r, "id"))
		RefreshTrustedHosts()
		ok(w, map[string]any{"ok": true})
	}
}

// resolveWdPass 把「掩码」和「空」都解释成「沿用已保存的密码」。
//
// 背景：GET /api/webdav/sources 出于安全不回显密码，返回的是 maskedPassword。
// 前端把这个值回填进表单后再点「测试连接」，就会拿 "******" 当密码去认证 → 401。
// 保存时同理会把真实密码覆盖成星号。这里统一在服务端兜底：
//   - 传了掩码或留空 → 按 id（其次按 url+user）找回已存的密码；
//   - 找不到对应的已存源（属于新增）→ 保持原样，空密码即匿名访问。
func resolveWdPass(src *model.WebdavSource) {
	if src.Pass != "" && src.Pass != maskedPassword {
		return
	}
	stored := store.WebdavSources()
	norm := func(s string) string { return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "/")) }

	for _, old := range stored {
		if src.ID != "" && old.ID == src.ID {
			src.Pass = old.Pass
			return
		}
	}
	// 前端新增/编辑时可能没带 id，按地址 + 用户名兜底
	for _, old := range stored {
		if norm(old.URL) == norm(src.URL) && old.User == src.User {
			src.Pass = old.Pass
			if src.ID == "" {
				src.ID = old.ID
			}
			return
		}
	}
}

func hWdProbe(w http.ResponseWriter, r *http.Request) {
	var src model.WebdavSource
	if err := body(r, &src); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	if !guardSourceURL(w, src.URL) {
		return
	}
	// 前端可能传来掩码密码，先还原成真实密码再认证
	resolveWdPass(&src)
	n, err := webdav.ProbeCtx(r.Context(), src)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	ok(w, map[string]any{"count": n})
}

func hWdList(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Src  model.WebdavSource `json:"src"`
		Path string             `json:"path"`
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	// 前端传的是列表接口返回的掩码对象，先用服务端存的真实密码还原
	resolveWdPass(&req.Src)
	if req.Path == "" {
		req.Path = webdav.MountHref(req.Src)
	}
	entries, err := webdav.ListCtx(r.Context(), req.Src, req.Path)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	ok(w, entries)
}

func hWdScan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Src      model.WebdavSource `json:"src"`
		MaxDepth int                `json:"maxDepth"`
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	if req.MaxDepth <= 0 {
		req.MaxDepth = 4
	}
	// 限制扫描深度上限，防止过深扫描导致 OOM 或超时
	if req.MaxDepth > 8 {
		req.MaxDepth = 8
	}
	resolveWdPass(&req.Src)
	vods := webdav.ScanCtx(r.Context(), req.Src, req.MaxDepth, nil)
	if req.Src.ID != "" {
		store.SaveWebdavScan(req.Src.ID, vods)
	}
	ok(w, vods)
}

func hWdVods(w http.ResponseWriter, r *http.Request) {
	ok(w, store.WebdavScanCache(q(r, "srcId")))
}

func hWdHistory(w http.ResponseWriter, r *http.Request) { ok(w, store.WdHistory()) }

func hWdHistoryUpdate(w http.ResponseWriter, r *http.Request) {
	var h model.WdHistory
	if err := body(r, &h); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	store.UpdateWdHistory(h)
	ok(w, map[string]any{"ok": true})
}

func hWdHistoryRemove(w http.ResponseWriter, r *http.Request) {
	var h model.WdHistory
	if err := body(r, &h); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	store.RemoveWdHistory(h)
	ok(w, map[string]any{"ok": true})
}

func hWdHistoryClear(w http.ResponseWriter, r *http.Request) {
	store.ClearWdHistory()
	ok(w, map[string]any{"ok": true})
}

// ---------- 直连 / 隐私 ----------

func hDirect(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ok(w, store.DirectList())
	case http.MethodPost:
		var req struct {
			URL string `json:"url"`
		}
		_ = body(r, &req)
		store.PushDirect(req.URL)
		ok(w, map[string]any{"ok": true})
	}
}

func hPrivacy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ok(w, map[string]any{"agreed": store.PrivacyAgreed(), "guideDone": store.GuideDone()})
	case http.MethodPost:
		var req struct {
			Agree bool `json:"agree"`
			Guide bool `json:"guide"`
		}
		_ = body(r, &req)
		if req.Agree {
			store.SetPrivacyAgreed()
		}
		if req.Guide {
			store.SetGuideDone()
		}
		ok(w, map[string]any{"ok": true})
	}
}

// ---------- 视频代理 ----------

// hProxy 视频流代理：透传 Range，重写 m3u8 内的相对/绝对地址，绕开跨域。
func hProxy(w http.ResponseWriter, r *http.Request) {
	target := q(r, "url")
	if target == "" {
		fail(w, http.StatusBadRequest, "缺少 url 参数")
		return
	}
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		fail(w, http.StatusBadRequest, "url 必须以 http:// 或 https:// 开头")
		return
	}
	if !guardURL(w, target) {
		return
	}
	auth := q(r, "auth")
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Header.Set("User-Agent", httpc.WebUA)
	req.Header.Set("Accept", "*/*")
	if auth != "" {
		req.Header.Set("Authorization", "Basic "+auth)
	}
	rng := r.Header.Get("Range")
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	res, err := httpc.StreamClient.Do(req)
	if err != nil || res.StatusCode >= 400 {
		// 直连被地区封锁（如 play.xluuss.com 对境外 IP 回 444/断连）：仅 m3u8 播放列表
		// 和小体积密钥文件走 SSH 跳板重试；视频分片等主体数据绝不走 SSH。
		if rng == "" && relayEligible(target) {
			if data, rerr := crawler.FetchSmallViaGeoRelay(r.Context(), target, relayMaxBytes); rerr == nil {
				if err == nil {
					res.Body.Close()
				}
				serveRelayed(w, r, target, auth, data)
				return
			}
		}
		if err != nil {
			fail(w, http.StatusBadGateway, "无法连接上游资源："+err.Error())
			return
		}
	}
	defer res.Body.Close()

	ct := res.Header.Get("Content-Type")
	isM3u8 := strings.Contains(ct, "mpegurl") || strings.Contains(ct, "m3u8") ||
		strings.Contains(ct, "x-mpegurl") || strings.Contains(ct, "vnd.apple")

	// 有些山寨 IPTV 镜像把 m3u8 返回成 text/html 之类的乱 Content-Type，
	// 只信 header 会漏判——漏判的后果不是"播不了"这么简单：m3u8 不会被
	// rewriteM3u8() 重写，里面的分片/密钥地址原样透传给浏览器，hls.js 直接
	// 跨域请求那些分片，大概率被 CORS/鉴权拦下，而且 hls.js 对"分片一直加载
	// 失败"不会很快判定为 fatal，表现成播放页永远卡在"缓冲中"、前端的换线路
	// 兜底压根没有机会触发。这里嗅探正文兜底：HLS playlist 规范要求必须以
	// #EXTM3U 开头。只在没带 Range 的请求上做（Range 命中的是分片/视频的某个
	// 中间偏移，不可能恰好读到文件开头，嗅探没有意义，也不该为了嗅探多读
	// 任何字节）。
	var body io.Reader = res.Body
	if !isM3u8 && res.StatusCode == http.StatusOK && r.Header.Get("Range") == "" {
		peek := make([]byte, 7)
		n, _ := io.ReadFull(res.Body, peek)
		body = io.MultiReader(bytes.NewReader(peek[:n]), res.Body)
		if string(peek[:n]) == "#EXTM3U" {
			isM3u8 = true
		}
	}

	if o := r.Header.Get("Origin"); OriginAllowed(o, r.Host) {
		w.Header().Set("Access-Control-Allow-Origin", o)
		w.Header().Set("Vary", "Origin")
	}
	w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges")

	if isM3u8 {
		// 重写 m3u8，body 长度会变：不透传 Content-Length，让 Go 自动计算。
		// 限制读取大小：正常 m3u8 文件很小（通常 < 1MB），超过 4MB 说明有问题。
		data, _ := io.ReadAll(io.LimitReader(body, 4<<20)) // max 4MB
		// base 用跟随重定向后的最终 URL，而非原始 target：
		// 302 跳转到真实流地址时，m3u8 里的相对分片/子流地址应相对最终 URL 解析，
		// 否则会拼错域名（如 iFan 聚合 → miguvideo 的场景）。
		base := target
		if res.Request != nil && res.Request.URL != nil {
			base = res.Request.URL.String()
		}
		rewritten := rewriteM3u8(string(data), base, auth, tokenFrom(r))
		for _, h := range []string{"Content-Type", "Cache-Control", "ETag", "Last-Modified"} {
			if v := res.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(res.StatusCode)
		_, _ = w.Write([]byte(rewritten))
		return
	}

	// 非 m3u8（分片/key 等二进制）：透传长度，逐字节转发
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified", "Cache-Control"} {
		if v := res.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(res.StatusCode)
	_, _ = io.Copy(w, body)
}

const relayMaxBytes = 1 << 20 // 跳板只搬 ≤1MB 的小文件

// relayEligible 只放行 m3u8 播放列表和 .key 密钥这类小文件走跳板。
func relayEligible(target string) bool {
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	if strings.Contains(strings.ToLower(u.RawQuery), ".m3u8") {
		return true // 如 /live.php?url=xxx.m3u8
	}
	p := strings.ToLower(u.Path)
	return strings.HasSuffix(p, ".m3u8") || strings.HasSuffix(p, ".m3u") || strings.HasSuffix(p, ".key")
}

// serveRelayed 输出经跳板取回的小文件；m3u8 同样重写里面的地址走代理。
func serveRelayed(w http.ResponseWriter, r *http.Request, target, auth string, data []byte) {
	if o := r.Header.Get("Origin"); OriginAllowed(o, r.Host) {
		w.Header().Set("Access-Control-Allow-Origin", o)
		w.Header().Set("Vary", "Origin")
	}
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("#EXTM3U")) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte(rewriteM3u8(string(data), target, auth, tokenFrom(r))))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

// hImg GET /api/img?url=<封面图原始地址>：优先命中本地缓存（见 imgcache，
// 落在 /dev/shm 即内存里），没有才联网拉一次并顺手缓存——同一张封面图
// 第二次访问不用再找原始站源重新下载。
func hImg(w http.ResponseWriter, r *http.Request) {
	target := q(r, "url")
	if target == "" {
		fail(w, http.StatusBadRequest, "缺少 url 参数")
		return
	}
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		fail(w, http.StatusBadRequest, "url 必须以 http:// 或 https:// 开头")
		return
	}
	if !guardURL(w, target) {
		return
	}
	data, ct, pending, err := imgcache.Get(r.Context(), target)
	if pending {
		// 这张图上次没拉成功，这次不再同步阻塞重试——后台已经在悄悄重试，
		// 这次直接回一张空图：前端 <img> 会触发 onerror 走现成的占位图逻辑，
		// 不会卡在这等。不能给缓存头，否则浏览器会把这次「空」当真缓存住，
		// 下次用户请求就没机会再触发重试了。
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "image/gif")
		w.WriteHeader(http.StatusOK)
		return
	}
	if err != nil {
		fail(w, http.StatusBadGateway, "无法获取图片："+err.Error())
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// rewriteM3u8 把 m3u8 里所有 URI（分片行 + #EXT-X-KEY/#EXT-X-MAP/#EXT-X-MEDIA 的 URI 属性）
// 都改成走代理，保证分片、AES 密钥、fMP4 init 段都能正确拉取。
func tokenFrom(r *http.Request) string {
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.Header.Get("X-Auth-Token")
}

func rewriteM3u8(body, base, auth, token string) string {
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n"), "\n")
	proxyOf := func(uri string) string {
		u := strings.TrimSpace(uri)
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			u = absURL(base, u)
		}
		out := "/api/proxy?url=" + url.QueryEscape(u) + proxyAuth(auth)
		// m3u8 内的分片/KEY 地址由浏览器直接发起，无法带自定义头，
		// 因此把访问口令透传到每个子请求 URL 上（仅在开启口令时生效）。
		if token != "" {
			out += "&token=" + url.QueryEscape(token)
		}
		return out
	}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			lines[i] = rewriteUriAttrs(line, proxyOf)
			continue
		}
		// 分片行：整行就是 URI（可能带 KEY 属性尾巴，取第一段）
		uri := trimmed
		if idx := strings.IndexAny(uri, " \t"); idx >= 0 {
			uri = uri[:idx]
		}
		lines[i] = proxyOf(uri)
	}
	return strings.Join(lines, "\n")
}

// uriAttrRe 大小写不敏感，支持双引号和单引号两种写法：
// URI="http://..." 或 uri='http://...'
var uriAttrRe = regexp.MustCompile(`(?i)(URI\s*=\s*)(["'])([^"']*)(["'])`)

// rewriteUriAttrs 重写标签行里 URI="..." 或 URI='...' 的值
// （#EXT-X-KEY / #EXT-X-MAP / #EXT-X-MEDIA 等），大小写不敏感。
func rewriteUriAttrs(line string, proxyOf func(string) string) string {
	return uriAttrRe.ReplaceAllStringFunc(line, func(m string) string {
		parts := uriAttrRe.FindStringSubmatch(m)
		if len(parts) < 5 {
			return m
		}
		// parts[1]=URI= parts[2]=开引号 parts[3]=URI值 parts[4]=闭引号
		return parts[1] + parts[2] + proxyOf(parts[3]) + parts[4]
	})
}

func proxyAuth(auth string) string {
	if auth == "" {
		return ""
	}
	return "&auth=" + url.QueryEscape(auth)
}

func absURL(base, ref string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return u.ResolveReference(r).String()
}

var _ = fmt.Sprintf
