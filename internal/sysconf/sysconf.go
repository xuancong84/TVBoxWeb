// Package sysconf 系统级参数（超时、连接、并发、缓存、SSH 跳板、解释器路径）。
//
// 所有参数登记在 items 表里：键、分组、单位、默认值、取值范围、是否需重启。
// 值持久化在项目根目录的 config.json（扁平 JSON：键 -> 数字/字符串），
// 设置页「系统」通过 /api/system 读写。缺省或文件不存在时使用默认值；
// 字符串类参数留空时回落到对应环境变量（向后兼容原先的 SSH_CMDS_GEO_BLOCK 等）。
package sysconf

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Item 一个可配置参数。
type Item struct {
	Key     string `json:"key"`
	Group   string `json:"group"`
	Label   string `json:"label"`
	Desc    string `json:"desc,omitempty"`
	Kind    string `json:"kind"`           // int | text | bool
	Unit    string `json:"unit,omitempty"` // 秒 / 分钟 / 个 ...
	Def     int    `json:"def"`
	Min     int    `json:"min,omitempty"`
	Max     int    `json:"max,omitempty"`
	Env     string `json:"env,omitempty"` // 字符串参数留空时回落的环境变量
	Restart bool   `json:"restart,omitempty"`
	Secret  bool   `json:"secret,omitempty"` // 口令类：对外只显示 Mask
}

// Mask 口令类参数对外显示的占位符；提交回来的值等于它表示「没改」。
const Mask = "******"

// GLaunch 启动参数分组（原先只能用命令行指定，现在也可在 config.json 里设置；命令行显式指定的优先）。
const GLaunch = "启动参数"

const (
	gSSH   = "SSH 跳板"
	gNet   = "网络连接"
	gTO    = "请求超时"
	gSrv   = "本机服务"
	gConc  = "并发"
	gCache = "缓存与间隔"
	gRT    = "解释器"
)

func sec(key, group, label, desc string, def int, restart bool) Item {
	return Item{Key: key, Group: group, Label: label, Desc: desc, Kind: "int", Unit: "秒", Def: def, Min: 1, Max: 3600, Restart: restart}
}
func min_(key, label, desc string, def int) Item {
	return Item{Key: key, Group: gCache, Label: label, Desc: desc, Kind: "int", Unit: "分钟", Def: def, Min: 1, Max: 60 * 24 * 365}
}
func cnt(key, label, desc string, def, max int, restart bool) Item {
	return Item{Key: key, Group: gConc, Label: label, Desc: desc, Kind: "int", Unit: "个", Def: def, Min: 1, Max: max, Restart: restart}
}

var items = []Item{
	{Key: "ssh.jump", Group: gSSH, Label: "SSH 跳板命令", Kind: "text", Env: "SSH_CMDS_GEO_BLOCK",
		Desc: "站源被地域封锁时，经海外机器中转抓取。每行一条 ssh 命令（也可用 ; 分隔），按顺序尝试，例如：ssh -p 22 user@host。留空则回落环境变量 SSH_CMDS_GEO_BLOCK。视频主数据永远不走跳板。"},

	sec("net.dial", gNet, "拨号超时", "建立 TCP 连接的最长等待", 10, true),
	sec("net.tls", gNet, "TLS 握手超时", "", 10, true),
	sec("net.header", gNet, "文本请求等响应头", "点播/订阅等文本类请求的响应头超时", 20, true),
	sec("net.stream_header", gNet, "视频流等响应头", "视频代理请求的响应头超时", 30, true),
	sec("net.idle", gNet, "空闲连接保留", "连接池里空闲连接的保留时间", 90, true),
	{Key: "net.stream_max_conns", Group: gNet, Label: "视频流单主机最大连接", Kind: "int", Unit: "个", Def: 32, Min: 1, Max: 512, Restart: true},

	{Key: "dns.cache", Group: gNet, Label: "DNS 缓存", Kind: "bool", Def: 1, Restart: true,
		Desc: "把所有域名（订阅源 → 站点 → 重定向 → 视频 CDN）解析出的 IP 存到 config.json 同目录的 dns-cache.json；再次访问先直连缓存 IP，连不上才走系统 DNS，再不行走 SSH 跳板解析。"},
	{Key: "dns.ssh", Group: gNet, Label: "DNS 失败时经 SSH 解析", Kind: "bool", Def: 1,
		Desc: "系统 DNS 解析失败时，用「SSH 跳板」在远端解析域名（只解析域名，视频数据仍本机直连）。需要先配置 SSH 跳板命令。"},

	sec("to.cms_cat", gTO, "点播·分类", "", 15, false),
	sec("to.cms_list", gTO, "点播·列表", "", 18, false),
	sec("to.cms_detail", gTO, "点播·详情", "", 18, false),
	sec("to.cms_search", gTO, "点播·搜索", "", 12, false),
	sec("to.py", gTO, "py/js 站源调用", "py/js 站源的 home/category/detail/search 调用超时", 75, false),
	sec("to.py_play", gTO, "py/js 站源取播放地址", "", 40, false),
	sec("to.spider_dl", gTO, "站源脚本下载", "下载 py/js 站源脚本", 25, false),
	sec("to.subscription", gTO, "订阅配置抓取", "", 20, false),
	sec("to.geo_file", gTO, "跳板小文件抓取", "经 SSH 跳板抓取的小文件", 25, false),
	sec("to.live", gTO, "直播源后台刷新", "", 20, false),
	sec("to.live_load", gTO, "直播源即时加载", "", 25, false),
	sec("to.epg", gTO, "EPG 节目单", "", 20, false),
	sec("to.live_probe", gTO, "直播频道探测", "单个频道可用性探测", 4, false),
	sec("to.webdav", gTO, "WebDAV", "", 15, false),
	sec("to.img", gTO, "图片缓存", "", 15, false),

	sec("srv.read_header", gSrv, "读请求头超时", "", 10, true),
	sec("srv.read", gSrv, "读请求体超时", "", 30, true),
	sec("srv.idle", gSrv, "Keep-Alive 空闲", "", 120, true),

	cnt("cc.search", "搜索并发", "全站搜索同时查询的站源数", 6, 64, false),
	cnt("cc.speed", "测速并发", "点播源测速同时进行的数量", 4, 64, false),
	cnt("cc.live", "直播源拉取并发", "", 6, 64, false),
	cnt("cc.live_probe", "直播探测并发", "直播频道探测默认并发", 8, 64, false),
	cnt("cc.cat_probe", "分类计数探测并发", "", 4, 32, false),
	cnt("cc.js_workers", "JS 站源 worker 数", "node 常驻进程数（保持默认 4 时，环境变量 JSSPIDER_WORKERS 仍可覆盖）", 4, 16, true),

	min_("ca.cms", "点播分类/列表/搜索缓存", "", 24*60),
	min_("ca.cms_detail", "点播详情缓存", "", 60),
	min_("ca.py_cat", "py/js 分类缓存", "", 6*60),
	min_("ca.py_list", "py/js 列表/搜索缓存", "", 60),
	min_("ca.py_detail", "py/js 详情缓存", "", 30),
	min_("ca.crawl_interval", "全站抓取间隔", "", 24*60),
	min_("ca.site_cache", "单站抓取缓存可信期", "", 24*60),
	min_("ca.live", "直播源缓存可信期", "", 24*60),
	min_("ca.epg", "EPG 缓存", "", 30),
	min_("ca.cat_probe", "分类计数缓存", "", 6*60),
	min_("ca.search_session", "搜索会话保留", "", 3),
	min_("ca.dns_refresh", "DNS 缓存刷新间隔", "缓存 IP 超过此时间后，下次使用时后台重新解析一次", 6*60),
	min_("ca.relay_cooldown", "跳板失败冷却", "同一主机经跳板也失败后，冷却多久再重试", 5),
	min_("ca.session", "登录会话有效期", "", 7*24*60),
	min_("ca.login_fail", "登录失败计数窗口", "", 15),
	min_("ca.channel_fail", "频道「播不通」结论可信期", "", 24*60),
	{Key: "cache.dir", Group: gCache, Label: "缓存目录", Kind: "text", Restart: true,
		Desc: "所有缓存（站源/榜单/直播列表、DNS、封面图）按类别放在此目录的子文件夹：sites、lists、dns、img。留空为 ./cache（相对运行目录）；修改后重启生效，旧位置的文件会自动搬迁。"},
	sec("ca.live_merge", gCache, "直播合并结果缓存", "", 20, false),
	sec("ca.scan_idle", gCache, "直播扫描空闲间隔", "一轮扫描无事可做后的等待", 30, false),
	sec("ca.scan_flush", gCache, "直播扫描落盘间隔", "", 30, false),

	{Key: "rt.python", Group: gRT, Label: "Python 解释器路径", Kind: "text", Env: "PYSPIDER_PYTHON", Restart: true,
		Desc: "py 站源用。留空依次找环境变量 PYSPIDER_PYTHON、~/miniconda3/bin/python、python3"},
	{Key: "rt.node", Group: gRT, Label: "Node 解释器路径", Kind: "text", Env: "JSSPIDER_NODE", Restart: true,
		Desc: "js 站源用。留空依次找环境变量 JSSPIDER_NODE、PATH 中的 node"},

	{Key: "srv.addr", Group: GLaunch, Label: "监听地址", Kind: "text", Restart: true,
		Desc: "host:port 列表，逗号 / 分号 / 换行分隔，不同 IP 可用不同端口，例如：127.0.0.1:8090, 192.168.1.10:9000, 0.0.0.0:8091。只写 :8090 或 8090 = 该端口监听全部网卡（含 IPv6）；IPv6 写作 [::1]:8090。留空 = :8090。命令行 -addr 指定时以命令行为准。"},
	{Key: "launch.data", Group: GLaunch, Label: "数据目录", Kind: "text", Restart: true, Desc: "留空为 data（相对运行目录）。"},
	{Key: "launch.user", Group: GLaunch, Label: "登录用户名", Kind: "text", Restart: true, Desc: "与密码同时设置才启用登录页；环境变量 YZ_USER 优先。"},
	{Key: "launch.pass", Group: GLaunch, Label: "登录密码", Kind: "text", Restart: true, Secret: true, Desc: "明文保存在 config.json（权限 0600）；环境变量 YZ_PASS 优先。"},
	{Key: "launch.token", Group: GLaunch, Label: "访问口令 token", Kind: "text", Restart: true, Secret: true, Desc: "公网部署建议设置；留空不用口令鉴权。"},
	{Key: "launch.secure_cookie", Group: GLaunch, Label: "会话 Cookie 加 Secure", Kind: "bool", Restart: true, Desc: "仅 HTTPS 部署开启；HTTP 访问下开启会导致登录失效。"},
	{Key: "launch.allow_private", Group: GLaunch, Label: "允许请求内网上游", Kind: "bool", Restart: true, Desc: "家用 NAS / 内网订阅源需要开启。"},
	{Key: "launch.origins", Group: GLaunch, Label: "额外允许的 CORS 来源", Kind: "text", Restart: true, Desc: "逗号分隔。"},
	{Key: "launch.hosts", Group: GLaunch, Label: "上游域名白名单", Kind: "text", Restart: true, Desc: "逗号分隔；留空不限制。"},
}

var (
	mu    sync.RWMutex
	vals  = map[string]any{}
	path  = "config.json"
	index = func() map[string]*Item {
		m := make(map[string]*Item, len(items))
		for i := range items {
			m[items[i].Key] = &items[i]
		}
		return m
	}()
	info   []InfoRow
	loaded bool
)

// InfoRow 只读展示项（启动参数等）。
type InfoRow struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

var listening []string

// SetListening 记录实际监听的地址（启动后登记），设置页展示。
func SetListening(a []string) { mu.Lock(); listening = a; mu.Unlock() }

// Listening 返回实际监听的地址。
func Listening() []string { mu.RLock(); defer mu.RUnlock(); return append([]string(nil), listening...) }

// SetInfo 登记只读的启动参数，供设置页展示。
func SetInfo(rows []InfoRow) { mu.Lock(); info = rows; mu.Unlock() }

// Info 返回只读的启动参数。
func Info() []InfoRow { mu.RLock(); defer mu.RUnlock(); return append([]InfoRow(nil), info...) }

// Schema 返回全部参数定义。
func Schema() []Item { return append([]Item(nil), items...) }

// Path 返回 config.json 路径。
func Path() string { mu.RLock(); defer mu.RUnlock(); return path }

// Loaded 是否已调用过 Init。
func Loaded() bool { mu.RLock(); defer mu.RUnlock(); return loaded }

// Init 从 p 读取配置；文件不存在则以当前（默认）值写出一份。p 为空用 ./config.json。
func Init(p string) error {
	if p == "" {
		p = "config.json"
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	mu.Lock()
	path = p
	loaded = true
	mu.Unlock()
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return save()
	}
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("解析 %s 失败：%w", p, err)
	}
	// 旧版本把端口和地址分开存（srv.port + srv.bind）：迁移成 srv.addr
	if _, has := raw["srv.addr"]; !has {
		port := 8090
		if f, ok := raw["srv.port"].(float64); ok && f >= 1 && f <= 65535 {
			port = int(f)
		}
		bind, _ := raw["srv.bind"].(string)
		var parts []string
		if ips := SplitList(bind); len(ips) == 0 {
			if port != 8090 {
				parts = []string{":" + strconv.Itoa(port)}
			}
		} else {
			for _, ip := range ips {
				if strings.Count(ip, ":") >= 2 && !strings.HasPrefix(ip, "[") && net.ParseIP(ip) != nil {
					ip = "[" + ip + "]"
				}
				parts = append(parts, ip+":"+strconv.Itoa(port))
			}
		}
		raw["srv.addr"] = strings.Join(parts, ",")
	}
	mu.Lock()
	vals = map[string]any{}
	for k, v := range raw {
		if nv, err := normalize(k, v); err == nil {
			vals[k] = nv
		}
	}
	mu.Unlock()
	return nil
}

func normalize(key string, v any) (any, error) {
	it := index[key]
	if it == nil {
		return nil, fmt.Errorf("未知参数 %s", key)
	}
	if it.Kind == "bool" {
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%s 需要布尔值", key)
		}
		return b, nil
	}
	if it.Kind == "text" {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s 需要字符串", key)
		}
		s = strings.TrimSpace(s)
		switch key {
		case "ssh.jump", "srv.addr", "launch.origins", "launch.hosts":
			// 列表型：换行当分隔符，内部统一用 ; 存储（origins/hosts 用 , 更自然，但读取处都会同时按两种切分）
			s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", ""), "\n", ";")
			for strings.Contains(s, ";;") {
				s = strings.ReplaceAll(s, ";;", ";")
			}
			s = strings.Trim(s, "; ")
		}
		if key == "srv.addr" && s != "" {
			if _, err := ParseAddrs(s); err != nil {
				return nil, err
			}
		}
		return s, nil
	}
	f, ok := v.(float64)
	if !ok {
		if n, ok2 := v.(int); ok2 {
			f, ok = float64(n), true
		}
	}
	if !ok {
		return nil, fmt.Errorf("%s 需要数字", key)
	}
	n := int(f)
	if n < it.Min || (it.Max > 0 && n > it.Max) {
		return nil, fmt.Errorf("%s（%s）取值范围 %d–%d", key, it.Label, it.Min, it.Max)
	}
	return n, nil
}

// Update 校验并合并 m（键 -> 值），写入 config.json。任一项非法则整体不生效。
func Update(m map[string]any) error {
	next := map[string]any{}
	for k, v := range m {
		if it := index[k]; it != nil && it.Secret {
			if sv, _ := v.(string); sv == Mask {
				continue // 没改
			}
		}
		nv, err := normalize(k, v)
		if err != nil {
			return err
		}
		next[k] = nv
	}
	mu.Lock()
	old := vals
	merged := make(map[string]any, len(old)+len(next))
	for k, v := range old {
		merged[k] = v
	}
	for k, v := range next {
		merged[k] = v
	}
	vals = merged
	mu.Unlock()
	if err := save(); err != nil {
		mu.Lock()
		vals = old
		mu.Unlock()
		return err
	}
	return nil
}

// Reset 恢复全部默认值并写盘。
func Reset() error {
	mu.Lock()
	vals = map[string]any{}
	mu.Unlock()
	return save()
}

// Values 返回全部参数当前生效值（数字参数为生效数值；文本参数为配置文件里的原值，
// 留空表示走环境变量/默认）。
func Values() map[string]any {
	out := make(map[string]any, len(items))
	for i := range items {
		it := &items[i]
		switch it.Kind {
		case "text":
			out[it.Key] = rawStr(it.Key)
			if it.Secret && out[it.Key] != "" {
				out[it.Key] = Mask
			}
		case "bool":
			out[it.Key] = Bool(it.Key)
		default:
			out[it.Key] = Int(it.Key)
		}
	}
	return out
}

func save() error {
	out := map[string]any{}
	for i := range items {
		it := &items[i]
		switch it.Kind {
		case "text":
			out[it.Key] = rawStr(it.Key)
		case "bool":
			out[it.Key] = Bool(it.Key)
		default:
			out[it.Key] = Int(it.Key)
		}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	mu.RLock()
	p := path
	mu.RUnlock()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func rawStr(key string) string {
	mu.RLock()
	defer mu.RUnlock()
	if s, ok := vals[key].(string); ok {
		return s
	}
	return ""
}

// Int 取整数参数（未设置用默认值）。
func Int(key string) int {
	it := index[key]
	if it == nil {
		return 0
	}
	mu.RLock()
	defer mu.RUnlock()
	if n, ok := vals[key].(int); ok {
		return n
	}
	return it.Def
}

// Bool 取布尔参数。
func Bool(key string) bool {
	it := index[key]
	if it == nil {
		return false
	}
	mu.RLock()
	defer mu.RUnlock()
	if b, ok := vals[key].(bool); ok {
		return b
	}
	return it.Def != 0
}

// Dur 取时长参数，按登记的单位换算。
func Dur(key string) time.Duration {
	n := Int(key)
	if it := index[key]; it != nil && it.Unit == "分钟" {
		return time.Duration(n) * time.Minute
	}
	return time.Duration(n) * time.Second
}

// Str 取文本参数：配置值非空用配置值，否则回落环境变量。
func Str(key string) string {
	it := index[key]
	if it == nil {
		return ""
	}
	if s := rawStr(key); s != "" {
		return s
	}
	if it.Env != "" {
		return strings.TrimSpace(os.Getenv(it.Env))
	}
	return ""
}

// SplitList 把 ,;换行 分隔的文本切成去空白、非空的条目。
func SplitList(s string) []string {
	var out []string
	for _, x := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == '\r' }) {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// ParseAddrs 解析监听地址列表：逗号 / 分号 / 换行分隔的 host:port，如
// "127.0.0.1:8090,192.168.1.10:9000"、"0.0.0.0:8090,0.0.0.0:8091"、":8090"、"[::1]:8090"。
// 只写端口（"8090"）视为 ":8090"。同一个端口上若有「全部网卡」（空 / 0.0.0.0 / ::）
// 又列了别的地址，会冲突，直接报错。
func ParseAddrs(s string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	wild := map[string]bool{}
	count := map[string]int{}
	for _, a := range SplitList(s) {
		if _, err := strconv.Atoi(a); err == nil {
			a = ":" + a
		}
		host, port, err := net.SplitHostPort(a)
		if err != nil {
			return nil, fmt.Errorf("监听地址 %q 无效（应为 host:port，IPv6 写作 [::1]:8090）", a)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("监听地址 %q 的端口无效（1–65535）", a)
		}
		if host != "" {
			ip := net.ParseIP(host)
			if ip == nil {
				return nil, fmt.Errorf("监听地址 %q 的主机部分必须是 IP（或留空）", a)
			}
			host = ip.String()
			a = net.JoinHostPort(host, port)
		}
		if seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
		count[port]++
		if host == "" || host == "0.0.0.0" || host == "::" {
			wild[port] = true
		}
	}
	for port, w := range wild {
		if w && count[port] > 1 {
			return nil, fmt.Errorf("端口 %s 上已有「全部网卡」监听（空 / 0.0.0.0 / ::），不能再同时列出同端口的其它地址", port)
		}
	}
	return out, nil
}

// ListenAddrs 由 srv.addr 得到要监听的 host:port 列表；留空 / 非法时为 [":8090"]。
func ListenAddrs() []string {
	if v, err := ParseAddrs(Str("srv.addr")); err == nil && len(v) > 0 {
		return v
	}
	return []string{":8090"}
}

// FallbackAddrs 按配置监听失败时的退路：每个用到的端口各监听全部网卡。
func FallbackAddrs() []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range ListenAddrs() {
		if _, port, err := net.SplitHostPort(a); err == nil && !seen[port] {
			seen[port] = true
			out = append(out, ":"+port)
		}
	}
	return out
}

var overrides []string

// SetOverrides 登记被命令行显式指定、因而覆盖了配置文件的参数键。
func SetOverrides(keys []string) { mu.Lock(); overrides = keys; mu.Unlock() }

// Overrides 返回被命令行覆盖的参数键。
func Overrides() []string { mu.RLock(); defer mu.RUnlock(); return append([]string(nil), overrides...) }
