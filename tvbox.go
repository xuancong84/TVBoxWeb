// Package tvbox 提供「夜帧影视」的 Web 服务库，内嵌前端静态资源。
//
// 第三方开发者可直接在自己的 Go 应用中引用本库，一行代码启动一个
// 完整可用的影视聚合点播/直播 Web 服务（Go 后端 + 内嵌 Vue3 前端）。
//
// 用法：
//
//	import "github.com/xxl6097/TVBoxWeb"
//
//	// 最简单：直接启动
//	if err := tvbox.ListenAndServe(":8090"); err != nil {
//		log.Fatal(err)
//	}
//
//	// 或接入自定义 mux / 中间件
//	srv, _ := tvbox.New(tvbox.WithDataDir("./mydata"))
//	http.Handle("/", srv.Handler())
package tvbox

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"github.com/xxl6097/TVBoxWeb/internal/httpc"
	"github.com/xxl6097/TVBoxWeb/internal/sysconf"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/api"
	"github.com/xxl6097/TVBoxWeb/internal/cms"
	"github.com/xxl6097/TVBoxWeb/internal/crawler"
	"github.com/xxl6097/TVBoxWeb/internal/jsspider"
	"github.com/xxl6097/TVBoxWeb/internal/pyspider"
	"github.com/xxl6097/TVBoxWeb/internal/store"
	"github.com/xxl6097/TVBoxWeb/internal/upgrade"
	"github.com/xxl6097/TVBoxWeb/internal/ws"
)

//go:embed all:dist
var distFS embed.FS

// 全局初始化保护：ws.Init / SetStartTime / SetWSHub 只需执行一次。
var (
	initOnce sync.Once
	initErr  error
)

// Server 代表一个「夜帧影视」Web 服务实例。
type Server struct {
	opts      Options
	mux       *http.ServeMux
	stop      chan struct{}
	closeOnce sync.Once
}

// Options 是库的配置项，通过 Option 函数式选项设置。
type Options struct {
	// DataDir 数据目录，配置/历史/收藏等存为 DataDir/store.json。
	// 默认 "data"。
	DataDir string

	// Logger 日志输出，默认 log.Default()。
	Logger *log.Logger

	// Upgrader 在线升级实现。未设置时升级接口返回「未启用」。
	Upgrader upgrade.Upgrader

	// Version 当前版本号，用于升级检测对比。默认 AppVersion。
	Version string

	// AuthToken 访问口令。非空时 /api/* 与 /ws 必须携带
	// Authorization: Bearer <token> 或 X-Auth-Token: <token>。
	// 默认空（兼容原有单机部署）。
	AuthToken string

	// LoginUser / LoginPass 账号密码登录。两者都非空时启用登录页：
	// 前端路由会先跳到 /login，登录成功后拿到 HttpOnly 会话 Cookie。
	// 与 AuthToken 可以同时使用（任一通过即放行），都为空则不鉴权。
	LoginUser string
	LoginPass string

	// CookieSecure 给会话 Cookie 加 Secure 标记。
	// 默认 false 兼容家用 HTTP 部署；服务端会自动依据 r.TLS / X-Forwarded-Proto
	// 判断 HTTPS，所以只有在「HTTPS 但反代不透传协议头」时才需要显式打开。
	CookieSecure bool

	// AllowedOrigins 额外允许的 CORS / WebSocket 来源（同源始终允许）。
	AllowedOrigins []string

	// AllowPrivateUpstreams 允许服务端请求内网地址（家用 NAS / 内网订阅源）。
	// 默认 false：/api/proxy 等回显型接口拒绝私有 IP，防 SSRF。
	AllowPrivateUpstreams bool

	// AllowedHosts 上游域名白名单，空表示不限制（仍做私网校验）。
	AllowedHosts []string
}

// Option 函数式配置项。
type Option func(*Options)

// WithDataDir 设置数据目录。
func WithDataDir(dir string) Option {
	return func(o *Options) { o.DataDir = dir }
}

// WithLogger 设置日志器。
func WithLogger(l *log.Logger) Option {
	return func(o *Options) { o.Logger = l }
}

// 升级相关类型的公开别名，允许外部包实现 Upgrader 接口（同 v1.3.x 兼容层）。
type (
	Release         = upgrade.Release
	UpgradeProgress = upgrade.UpgradeProgress
	UpgradeStage    = upgrade.UpgradeStage
	Upgrader        = upgrade.Upgrader
)

// 升级阶段常量
const (
	StageChecking    = upgrade.StageChecking
	StageDownloading = upgrade.StageDownloading
	StageVerifying   = upgrade.StageVerifying
	StageReplacing   = upgrade.StageReplacing
	StageDone        = upgrade.StageDone
)

// WithUpgrader 注入在线升级实现（复刻 gofs.WithUpgrader）。
func WithUpgrader(u Upgrader) Option {
	return func(o *Options) { o.Upgrader = u }
}

// WithVersion 设置当前版本号（升级检测时与远端比较）。
func WithVersion(v string) Option {
	return func(o *Options) { o.Version = v }
}

// WithAuthToken 开启访问口令保护（公网部署建议设置）。
func WithAuthToken(token string) Option {
	return func(o *Options) { o.AuthToken = strings.TrimSpace(token) }
}

// WithCookieSecure 强制会话 Cookie 带 Secure 标记。
// 仅在「HTTPS 部署但反向代理不透传 X-Forwarded-Proto」时需要打开；
// 注意 HTTP 访问时打开会导致浏览器丢弃 Cookie、登录后立刻被踢回登录页。
func WithCookieSecure(on bool) Option {
	return func(o *Options) { o.CookieSecure = on }
}

// WithLogin 开启账号密码登录。
// 用户名与密码都非空才生效；任一项为空视为不启用，避免半配置导致「永远登不进去」。
func WithLogin(user, pass string) Option {
	return func(o *Options) {
		u, p := strings.TrimSpace(user), pass
		if u == "" || p == "" {
			o.LoginUser, o.LoginPass = "", ""
			return
		}
		o.LoginUser, o.LoginPass = u, p
	}
}

// WithAllowedOrigins 设置额外的 CORS / WebSocket 允许来源。
func WithAllowedOrigins(origins ...string) Option {
	return func(o *Options) { o.AllowedOrigins = origins }
}

// WithAllowPrivateUpstreams 允许请求内网上游（家庭 NAS 场景）。
func WithAllowPrivateUpstreams(allow bool) Option {
	return func(o *Options) { o.AllowPrivateUpstreams = allow }
}

// WithAllowedHosts 限制上游域名白名单。
func WithAllowedHosts(hosts ...string) Option {
	return func(o *Options) { o.AllowedHosts = hosts }
}

// Version 返回当前版本号（可通过 ldflags -X 注入 internal/api.AppVersion）。
func Version() string {
	return api.AppVersion
}

// 默认初始化：只跑一次，线程安全。
func ensureInit() error {
	initOnce.Do(func() {
		api.SetStartTime(time.Now())
		ws.Init()
		api.SetWSHub(ws.GlobalHub)
	})
	return initErr
}

// New 创建一个服务器实例。所有 Option 均在此时生效。
func New(opts ...Option) (*Server, error) {
	o := Options{
		DataDir: "data",
		Logger:  log.Default(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}

	if !sysconf.Loaded() {
		if err := sysconf.Init(""); err != nil {
			o.Logger.Printf("读取 config.json 失败，使用默认参数: %v", err)
		}
	}
	httpc.Apply()

	if err := ensureInit(); err != nil {
		return nil, err
	}

	// 安全配置必须在任何请求进来之前注入
	api.SetSecurity(api.SecurityConfig{
		AuthToken:             o.AuthToken,
		LoginUser:             o.LoginUser,
		LoginPass:             o.LoginPass,
		CookieSecure:          o.CookieSecure,
		AllowedOrigins:        o.AllowedOrigins,
		AllowPrivateUpstreams: o.AllowPrivateUpstreams,
		AllowedHosts:          o.AllowedHosts,
		Logger:                o.Logger,
	})
	ws.SetOriginChecker(api.OriginAllowed)
	// 新版本能正常启动才清理上一版 .backup，否则可用它手工回滚
	upgrade.ConfirmStartup()

	// 注入升级实现与版本号
	api.SetUpgrader(o.Upgrader)
	if o.Version != "" {
		api.AppVersion = o.Version
	}

	// 数据文件
	dataFile := filepath.Join(o.DataDir, "store.json")
	if err := store.Init(dataFile); err != nil {
		return nil, fmt.Errorf("tvbox: 加载数据失败: %w", err)
	}
	store.InitSiteCrawlCache(o.DataDir)
	store.InitFeedPool(o.DataDir)
	store.InitLiveCrawlCache(o.DataDir)
	cms.InitDiskCache(o.DataDir)

	// Python 站源：必须在首次构建站源列表（下面的 Restore）之前就绪。
	relay := crawler.StartSpiderRelay()
	pyspider.RelayURL, jsspider.RelayURL = relay, relay
	pyspider.Init(o.DataDir)
	pyspider.Fetch = crawler.FetchScript
	jsspider.Init(o.DataDir)
	jsspider.Fetch = crawler.FetchScript
	store.SetSpiders(
		func(typ int, api, ext string) string {
			switch {
			case pyspider.IsPySite(typ, api, ext):
				return "py"
			case jsspider.IsJSSite(typ, api, ext):
				return "js"
			}
			return ""
		},
		func(kind string) string {
			if kind == "js" {
				if !jsspider.Available() {
					return "JS 站源：" + jsspider.Reason()
				}
			} else if !pyspider.Available() {
				return "Python 站源：" + pyspider.Reason()
			}
			return ""
		})
	if pyspider.Available() {
		o.Logger.Printf("Python 站源已启用")
	} else {
		o.Logger.Printf("Python 站源未启用：%s", pyspider.Reason())
	}
	if jsspider.Available() {
		o.Logger.Printf("JS 站源已启用")
	} else {
		o.Logger.Printf("JS 站源未启用：%s", jsspider.Reason())
	}

	// 启动时恢复缓存站点列表
	if store.TotalSites() == 0 {
		crawler.Restore()
	}

	// store 就绪后再同步可信域名（含网盘源/订阅/直播/EPG），
	// 否则「NAS 公网域名在内网解析成 ULA」会被严格模式拒绝。
	api.RefreshTrustedHosts()

	s := &Server{opts: o, mux: api.Handler(), stop: make(chan struct{})}

	// WebSocket 端点
	wsHandler := ws.NewHandler(ws.GlobalHub)
	s.mux.HandleFunc("/ws", wsHandler.ServeWS)

	// 内嵌前端静态资源
	if err := s.mountStatic(); err != nil {
		return nil, err
	}

	return s, nil
}

// mountStatic 挂载内嵌前端静态资源到根路径。
//
// go:embed 出来的文件 mtime 是编译时刻（同一个二进制里全部文件都一样），
// 跟真实的"这份内容多久没变过"毫无关系；http.FileServer 默认只靠
// Last-Modified 做启发式缓存，浏览器可能照着这个编译时间久不久直接把
// index.html 这类"每次发布都要换新内容"的文件缓存很久，导致发完新版本、
// 用户点普通刷新（非强制刷新）却还停在旧页面/旧资源上——每次发版都要
// 手动改 sw.js 里的版本号正是为了绕开这一点。这里显式按路径分两类：
// index.html/根路径/sw.js 这些"内容会变但 URL 不变"的必须每次都回源校验
// （no-cache：允许缓存但强制带条件请求去问服务器"你这份新不新"），带
// hash 文件名的 /assets/* 内容不变 URL 就不变，可以放心长期强缓存。
func (s *Server) mountStatic() error {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return fmt.Errorf("tvbox: 内嵌前端资源不可用: %w", err)
	}
	fileServer := http.FileServer(http.FS(sub))
	s.mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	}))
	return nil
}

// Handler 返回完整的 http.Handler（含全部 API 与内嵌前端）。
//
// 可嵌入到自定义 mux、net/http 或任意支持 http.Handler 的框架中。
func (s *Server) Handler() http.Handler {
	return api.CORS(s.mux)
}

// ParseListenAddrs 解析 -addr：逗号 / 分号分隔的 host:port 列表，如
// "127.0.0.1:8090,192.168.1.10:9000"（不同 IP 可用不同端口）、"0.0.0.0:8090,0.0.0.0:8091"、":8090"。
// 只写端口号（"8090"）视为 ":8090"。
func ParseListenAddrs(addr string) ([]string, error) {
	out, err := sysconf.ParseAddrs(addr)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("监听地址为空")
	}
	return out, nil
}

// listenAll 依次监听全部地址；任何一个失败则关闭已开的并返回错误。
func listenAll(addrs []string) ([]net.Listener, error) {
	var lns []net.Listener
	for _, a := range addrs {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			for _, l := range lns {
				_ = l.Close()
			}
			return nil, fmt.Errorf("监听 %s 失败: %w", a, err)
		}
		lns = append(lns, ln)
	}
	return lns, nil
}

// ListenAndServe 在指定地址启动服务并阻塞，直到出错或服务关闭。
//
//	addr 形如 ":8090"、"127.0.0.1:9000"，也可以是逗号分隔的列表
//	"127.0.0.1:8090,192.168.1.10:8090"、"0.0.0.0:8090,0.0.0.0:8091"。
//	addr 为空时用系统设置（config.json 的 srv.addr）；这种情况下
//	若按配置监听失败（例如填了本机没有的 IP），退回「配置里用到的各端口监听全部网卡」，避免改坏配置后起不来。
func (s *Server) ListenAndServe(addr string) error {
	var lns []net.Listener
	var addrs []string
	if strings.TrimSpace(addr) == "" {
		addrs = sysconf.ListenAddrs()
		var err error
		if lns, err = listenAll(addrs); err != nil {
			fb := sysconf.FallbackAddrs()
			s.opts.Logger.Printf("按系统设置监听失败（%v），退回 %v", err, fb)
			addrs = fb
			if lns, err = listenAll(addrs); err != nil {
				return err
			}
		}
	} else {
		var err error
		if addrs, err = ParseListenAddrs(addr); err != nil {
			return err
		}
		if lns, err = listenAll(addrs); err != nil {
			return err
		}
	}
	sysconf.SetListening(addrs)

	// 后台定时抓取：启动先跑一轮，之后按 CrawlInterval 常驻调度
	s.startScheduler()
	api.RestartFn = s.restart

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: sysconf.Dur("srv.read_header"),
		ReadTimeout:       sysconf.Dur("srv.read"),
		// 视频流为长连接，不设整体 WriteTimeout；用 IdleTimeout 兜底
		IdleTimeout:    sysconf.Dur("srv.idle"),
		MaxHeaderBytes: 1 << 20,
	}

	// SIGINT/SIGTERM 优雅退出：先停止接收新请求，再让在途请求收尾
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		s.opts.Logger.Printf("收到退出信号，正在优雅关闭…")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	errc := make(chan error, len(lns))
	for i, ln := range lns {
		s.opts.Logger.Printf("夜帧影视 Web 服务已启动：http://%s", displayAddr(addrs[i]))
		go func(ln net.Listener) { errc <- srv.Serve(ln) }(ln)
	}
	// 任何一个监听出错 / 服务被 Shutdown 就整体退出
	err := <-errc
	if err == http.ErrServerClosed {
		err = nil
	}
	_ = srv.Close()
	_ = s.Close()
	return err
}

func displayAddr(a string) string {
	host, port, err := net.SplitHostPort(a)
	if err != nil {
		return a
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}

// restart 用同样的命令行参数原地重启本进程（先落盘缓存）。
// 在 systemd / docker / while 循环下同样适用：进程 PID 不变，不依赖外部拉起。
func (s *Server) restart() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	go func() {
		time.Sleep(400 * time.Millisecond) // 让 HTTP 响应先发出去
		_ = s.Close()
		if err := reexec(exe, os.Args); err != nil {
			s.opts.Logger.Printf("重启失败: %v", err)
		}
	}()
	return nil
}

// startScheduler 常驻定时抓取：修复原先「只在启动时检查一次」的问题。
func (s *Server) startScheduler() {
	go func() {
		if store.TotalSites() > 0 {
			crawler.MaybeRun()
		}
		t := time.NewTicker(crawler.CrawlInterval())
		defer t.Stop()
		for {
			select {
			case <-t.C:
				crawler.MaybeRun()
			case <-s.stop:
				return
			}
		}
	}()
	s.startCacheFlusher()
	api.StartLiveScan()
}

// startCacheFlusher 定期把 cms 的分类/片单/搜索响应缓存落盘，不等到进程
// 优雅退出才存——树莓派这类设备随时可能直接断电。10 分钟一次：既能让
// 一天里绝大部分已经抓到的缓存扛得住中途重启，又不会把 SD 卡写得太勤。
// 退出时的最后一次 flush 不靠这里的 goroutine 去抢时间点——main() 一看到
// ListenAndServe 返回就直接退出，不会等任何后台 goroutine 把 s.stop 关闭
// 的事件处理完，那样会竞态漏掉最后一批缓存——而是放在 Close() 里同步做。
func (s *Server) startCacheFlusher() {
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				cms.FlushDiskCache()
			case <-s.stop:
				return
			}
		}
	}()
}

// Close 释放资源（停止调度器、关闭 WebSocket Hub）。幂等，可重复调用。
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		if s.stop != nil {
			close(s.stop)
		}
		ws.Shutdown()
		// 同步落盘最后一批响应缓存：main() 看到 ListenAndServe 返回就
		// 直接退出，不会等后台 goroutine 把 s.stop 关闭事件处理完。
		cms.FlushDiskCache()
	})
	return nil
}

// ListenAndServe 是包级便捷函数：用默认配置在 addr 启动服务并阻塞。
func ListenAndServe(addr string, opts ...Option) error {
	srv, err := New(opts...)
	if err != nil {
		return err
	}
	return srv.ListenAndServe(addr)
}

// Handler 是包级便捷函数：返回一个用默认配置构建的 http.Handler。
func Handler(opts ...Option) (http.Handler, error) {
	srv, err := New(opts...)
	if err != nil {
		return nil, err
	}
	return srv.Handler(), nil
}
