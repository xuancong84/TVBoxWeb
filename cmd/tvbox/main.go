// 夜帧影视 · 可执行入口
//
// 基于 tvbox 库构建的独立二进制，内嵌前端，开箱即用。
// 通过 githubupgrade 注入 GitHub Releases 在线升级能力。
//
// 用法：
//
//	tvbox -addr :8090 -data data
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	tvbox "github.com/xxl6097/TVBoxWeb"
	"github.com/xxl6097/TVBoxWeb/githubupgrade"
	"github.com/xxl6097/TVBoxWeb/internal/sysconf"
)

func main() {
	addr := flag.String("addr", "", "监听地址，host:port；可逗号分隔多个，如 127.0.0.1:8090,192.168.1.10:8090 或 0.0.0.0:8090,0.0.0.0:8091（留空用 config.json 的 监听地址，默认 :8090）")
	dataDir := flag.String("data", "", "数据目录（留空用 config.json，默认 data）")
	token := flag.String("token", "", "访问口令（公网部署强烈建议设置；留空则不鉴权）")
	user := flag.String("user", "", "登录用户名（与 -pass 同时设置才会启用登录页）")
	pass := flag.String("pass", "", "登录密码（与 -user 同时设置才会启用登录页）")
	secureCookie := flag.Bool("secure-cookie", false, "会话 Cookie 加 Secure 标记（HTTPS 部署时建议开启；HTTP 访问下开启会导致登录失效）")
	origins := flag.String("origins", "", "额外允许的 CORS 来源，逗号分隔")
	allowPrivate := flag.Bool("allow-private", false, "允许请求内网上游（家用 NAS / 内网订阅源）")
	hosts := flag.String("hosts", "", "上游域名白名单，逗号分隔（留空不限制）")
	cfgPath := flag.String("config", "config.json", "系统参数文件（超时/并发/缓存/SSH 跳板/启动参数等，可在 设置→系统 里编辑）")
	flag.Parse()

	if err := sysconf.Init(*cfgPath); err != nil {
		log.Printf("读取系统参数 %s 失败，使用默认值: %v", *cfgPath, err)
	}

	// 启动参数优先级：命令行显式指定 > config.json（设置→系统）> 默认值。
	// （登录账号密码另外还有环境变量 YZ_USER / YZ_PASS，优先级最高。）
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var overridden []string
	mark := func(flagName string, keys ...string) {
		if set[flagName] {
			overridden = append(overridden, keys...)
		}
	}
	mark("addr", "srv.addr")
	mark("data", "launch.data")
	mark("token", "launch.token")
	mark("user", "launch.user")
	mark("pass", "launch.pass")
	mark("secure-cookie", "launch.secure_cookie")
	mark("allow-private", "launch.allow_private")
	mark("origins", "launch.origins")
	mark("hosts", "launch.hosts")
	sysconf.SetOverrides(overridden)

	pickS := func(flagName, flagVal, key string) string {
		if set[flagName] {
			return flagVal
		}
		return sysconf.Str(key)
	}
	pickB := func(flagName string, flagVal bool, key string) bool {
		if set[flagName] {
			return flagVal
		}
		return sysconf.Bool(key)
	}
	// 环境变量优先于其它来源：容器部署时把口令放 env 更常见
	envOr := func(key, def string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		return def
	}

	dir := strings.TrimSpace(pickS("data", *dataDir, "launch.data"))
	if dir == "" {
		dir = "data"
	}
	opts := []tvbox.Option{
		tvbox.WithDataDir(dir),
		tvbox.WithAuthToken(pickS("token", *token, "launch.token")),
		tvbox.WithLogin(envOr("YZ_USER", pickS("user", *user, "launch.user")), envOr("YZ_PASS", pickS("pass", *pass, "launch.pass"))),
		tvbox.WithCookieSecure(pickB("secure-cookie", *secureCookie, "launch.secure_cookie")),
		tvbox.WithAllowedOrigins(sysconf.SplitList(pickS("origins", *origins, "launch.origins"))...),
		tvbox.WithAllowPrivateUpstreams(pickB("allow-private", *allowPrivate, "launch.allow_private")),
		tvbox.WithAllowedHosts(sysconf.SplitList(pickS("hosts", *hosts, "launch.hosts"))...),
	}

	// 注入 GitHub Releases 在线升级（复刻 go-fs 的实现方式）。
	// 失败不阻塞启动：升级是可选项。
	if up, err := githubupgrade.New(githubupgrade.Options{
		Owner: "xxl6097",
		Repo:  "TVBoxWeb",
	}); err == nil {
		opts = append(opts, tvbox.WithUpgrader(up))
	} else {
		log.Printf("在线升级未启用: %v", err)
	}

	// -addr 留空时 tvbox 按 config.json 的 srv.addr 监听
	if err := tvbox.ListenAndServe(*addr, opts...); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
