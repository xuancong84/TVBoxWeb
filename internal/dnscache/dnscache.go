// Package dnscache 磁盘持久化的 DNS 缓存（域名 -> IP 列表）+ 三级解析兜底。
//
// 所有出站连接（点播/订阅/直播/视频流代理，包括重定向后的各级域名）都经 DialContext 拨号，
// 因此从原始站点到最终视频数据的每个域名都会被缓存到 dns-cache.json（与 config.json 同目录）。
//
// 拨号顺序：
//  1. 缓存里有该域名 → 直接连缓存的 IP（跳过 DNS，省一次解析延迟）；
//  2. 缓存没有或连不上 → 系统 DNS 解析后再连，成功则更新缓存；
//  3. 系统 DNS 也失败（或解析出的 IP 全连不上）→ 经 SSH 跳板在远端解析（getent/dig），再连。
//
// TLS 的 SNI/证书校验仍按 URL 里的域名进行（这里只替换 TCP 目标 IP），所以不影响 HTTPS 安全性。
// 仅「解析域名」会走 SSH，视频等数据本身永远是本机直连。
package dnscache

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/sysconf"
)

type entry struct {
	IPs []string `json:"ips"`
	At  int64    `json:"at"`            // 最近一次解析成功的 unix 秒
	Src string   `json:"src,omitempty"` // dns | ssh
}

const (
	maxEntries    = 5000
	maxIPsPerHost = 8
	cachePhase    = 5 * time.Second // 缓存 IP 这一阶段总共最多花多久
	negativeTTL   = 60 * time.Second
)

var (
	mu      sync.Mutex
	entries = map[string]*entry{}
	dirty   bool
	file    string
	started bool

	failMu    sync.Mutex
	refreshAt = map[string]time.Time{}
	fail      = map[string]time.Time{} // 三级都失败的域名：短时间内不再重复折腾（尤其是 SSH）

	flightMu sync.Mutex
	flight   = map[string]*call{}
)

type call struct {
	done chan struct{}
	ips  []string
	err  error
}

var hostRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Enabled 是否启用缓存（设置→系统→DNS 缓存）。
func Enabled() bool { return sysconf.Bool("dns.cache") }

// Start 读取磁盘缓存并启动落盘协程。幂等。
func Start() {
	mu.Lock()
	if started {
		mu.Unlock()
		return
	}
	started = true
	file = sysconf.CacheFile("dns", "dns-cache.json", filepath.Join(filepath.Dir(sysconf.Path()), "dns-cache.json"))
	mu.Unlock()
	if b, err := os.ReadFile(file); err == nil {
		var m map[string]*entry
		if json.Unmarshal(b, &m) == nil {
			mu.Lock()
			for k, v := range m {
				if v != nil && len(v.IPs) > 0 {
					entries[k] = v
				}
			}
			mu.Unlock()
		}
	}
	go func() {
		for range time.Tick(5 * time.Second) {
			flush()
		}
	}()
}

func flush() {
	mu.Lock()
	if !dirty || file == "" {
		mu.Unlock()
		return
	}
	dirty = false
	b, err := json.MarshalIndent(entries, "", " ")
	f := file
	mu.Unlock()
	if err != nil {
		return
	}
	tmp := f + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, f)
	}
}

func get(host string) (entry, bool) {
	mu.Lock()
	defer mu.Unlock()
	e := entries[host]
	if e == nil || len(e.IPs) == 0 {
		return entry{}, false
	}
	return entry{IPs: append([]string(nil), e.IPs...), At: e.At, Src: e.Src}, true
}

func put(host string, ips []string, src string) {
	if len(ips) == 0 {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	now := time.Now().Unix()
	merged := append([]string(nil), ips...)
	if old := entries[host]; old != nil {
		// 合并而不是替换：新解析出的 IP 排前面，旧 IP 保留在后面。
		// 域名被撤销/解析变了，只要旧 IP 还能连就不能把它从缓存里丢掉。
		seen := map[string]bool{}
		for _, x := range merged {
			seen[x] = true
		}
		for _, x := range old.IPs {
			if !seen[x] {
				merged = append(merged, x)
			}
		}
		if len(merged) > maxIPsPerHost {
			merged = merged[:maxIPsPerHost]
		}
		if sameList(old.IPs, merged) && old.Src == src && now-old.At < 3600 {
			return // 没变化且一小时内刚更新过：不重复落盘
		}
	}
	entries[host] = &entry{IPs: merged, At: now, Src: src}
	dirty = true
	if len(entries) > maxEntries {
		type kv struct {
			k string
			t int64
		}
		var all []kv
		for k, v := range entries {
			all = append(all, kv{k, v.At})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].t < all[j].t })
		for _, x := range all[:len(all)-maxEntries+maxEntries/10] {
			delete(entries, x.k)
		}
	}
}

func sameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return true
}

// Stats 返回缓存条数（给设置页展示）。
func Stats() int { mu.Lock(); defer mu.Unlock(); return len(entries) }

// Clear 清空缓存（内存 + 磁盘）。
func Clear() {
	mu.Lock()
	entries = map[string]*entry{}
	dirty = true
	mu.Unlock()
	failMu.Lock()
	fail = map[string]time.Time{}
	failMu.Unlock()
	flush()
}

// ---------- 解析 ----------

func resolveSystem(ctx context.Context, host string) ([]string, error) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(rctx, host)
	if err != nil {
		return nil, err
	}
	var v4, v6 []string
	for _, a := range addrs {
		if a.IP.To4() != nil {
			v4 = append(v4, a.IP.String())
		} else {
			v6 = append(v6, a.IP.String())
		}
	}
	return append(v4, v6...), nil
}

// 远端解析：优先 getent（Linux），其次 dig、nslookup、dscacheutil（macOS）。
func sshScript(host string) string {
	h := "'" + host + "'" // host 已通过 hostRe 校验，不含引号
	return "getent ahosts " + h + " 2>/dev/null || dig +short " + h + " 2>/dev/null || " +
		"dscacheutil -q host -a name " + h + " 2>/dev/null || nslookup " + h + " 2>/dev/null"
}

func controlMaster(sshCmd string) string {
	if !strings.HasPrefix(sshCmd, "ssh ") {
		return sshCmd
	}
	// 与 crawler.withControlMaster 用同一套 ControlPath，共用同一条 SSH 连接
	h := sha1.Sum([]byte(sshCmd))
	sock := "/tmp/tvboxweb-sshcm-" + hex.EncodeToString(h[:])[:12]
	return "ssh -o ControlMaster=auto -o ControlPersist=60s -o ControlPath=" + sock + strings.TrimPrefix(sshCmd, "ssh")
}

func parseIPs(out string) []string {
	seen := map[string]bool{}
	var v4, v6 []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "#") || strings.HasPrefix(line, "Server:") { // nslookup 的「Address: x.x.x.x#53」是 DNS 服务器自身
			continue
		}
		for _, tok := range strings.Fields(line) {
			tok = strings.TrimPrefix(tok, "Address:")
			ip := net.ParseIP(tok)
			if ip == nil || ip.IsUnspecified() || seen[ip.String()] {
				continue
			}
			seen[ip.String()] = true
			if ip.To4() != nil {
				v4 = append(v4, ip.String())
			} else {
				v6 = append(v6, ip.String())
			}
		}
	}
	return append(v4, v6...)
}

func resolveSSH(ctx context.Context, host string) ([]string, error) {
	if !sysconf.Bool("dns.ssh") {
		return nil, errors.New("SSH 解析未启用")
	}
	raw := strings.TrimSpace(sysconf.Str("ssh.jump"))
	if raw == "" {
		return nil, errors.New("未配置 SSH 跳板")
	}
	if !hostRe.MatchString(host) {
		return nil, errors.New("域名含非法字符")
	}
	last := errors.New("所有跳板都没能解析")
	for _, c := range strings.Split(raw, ";") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cmd := exec.CommandContext(cctx, "sh", "-c", controlMaster(c)+" '"+sshScript(host)+"'")
		var out bytes.Buffer
		cmd.Stdout = &out
		err := cmd.Run()
		cancel()
		if ips := parseIPs(out.String()); len(ips) > 0 {
			return ips, nil
		} else if err != nil {
			last = errors.New(c + "：" + err.Error())
		} else {
			last = errors.New(c + "：远端没有解析结果")
		}
	}
	return nil, last
}

// singleflight：同一域名并发解析合并成一次（避免同时开一堆 SSH）。
func do(key string, fn func() ([]string, error)) ([]string, error) {
	flightMu.Lock()
	if c, ok := flight[key]; ok {
		flightMu.Unlock()
		<-c.done
		return c.ips, c.err
	}
	c := &call{done: make(chan struct{})}
	flight[key] = c
	flightMu.Unlock()
	c.ips, c.err = fn()
	flightMu.Lock()
	delete(flight, key)
	flightMu.Unlock()
	close(c.done)
	return c.ips, c.err
}

// Lookup 供 SSRF 校验等「只要 IP、不拨号」的场景：缓存 → 系统 DNS → SSH。
func Lookup(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	if !Enabled() {
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var r []net.IP
		for _, a := range addrs {
			r = append(r, a.IP)
		}
		return r, nil
	}
	if e, ok := get(host); ok {
		return toIPs(e.IPs), nil
	}
	ips, err := resolveFresh(ctx, host)
	if err != nil {
		return nil, err
	}
	return toIPs(ips), nil
}

func toIPs(ss []string) []net.IP {
	var r []net.IP
	for _, s := range ss {
		if ip := net.ParseIP(s); ip != nil {
			r = append(r, ip)
		}
	}
	return r
}

// resolveFresh 系统 DNS → SSH，成功即写缓存。
func resolveFresh(ctx context.Context, host string) ([]string, error) {
	failMu.Lock()
	if t, ok := fail[host]; ok && time.Since(t) < negativeTTL {
		failMu.Unlock()
		return nil, errors.New("域名 " + host + " 刚解析失败过，稍后再试")
	}
	failMu.Unlock()
	ips, err := do("fresh:"+host, func() ([]string, error) {
		if ips, err := resolveSystem(ctx, host); err == nil && len(ips) > 0 {
			put(host, ips, "dns")
			return ips, nil
		}
		ips, err := resolveSSH(ctx, host)
		if err == nil && len(ips) > 0 {
			put(host, ips, "ssh")
			return ips, nil
		}
		return nil, err
	})
	if err != nil {
		failMu.Lock()
		fail[host] = time.Now()
		failMu.Unlock()
	}
	return ips, err
}

// ---------- 拨号 ----------

func family(network string, ips []string) []string {
	var r []string
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}
		is4 := ip.To4() != nil
		if (network == "tcp4" && !is4) || (network == "tcp6" && is4) {
			continue
		}
		r = append(r, s)
	}
	return r
}

func dialIPs(ctx context.Context, d *net.Dialer, network string, ips []string, port string) (net.Conn, string, error) {
	var last error
	for _, s := range ips {
		c, err := d.DialContext(ctx, network, net.JoinHostPort(s, port))
		if err == nil {
			return c, s, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	if last == nil {
		last = errors.New("没有可用的 IP")
	}
	return nil, "", last
}

func promote(ips []string, good string) []string {
	r := []string{good}
	for _, s := range ips {
		if s != good {
			r = append(r, s)
		}
	}
	return r
}

func subtract(ips []string, tried map[string]bool) []string {
	var r []string
	for _, s := range ips {
		if !tried[s] {
			r = append(r, s)
		}
	}
	return r
}

// NewDialContext 返回带缓存/兜底的 DialContext；base 提供拨号超时等参数。
func NewDialContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil || !Enabled() || net.ParseIP(host) != nil || host == "localhost" {
			return base.DialContext(ctx, network, addr)
		}
		tried := map[string]bool{}
		var lastErr error

		// 1) 缓存
		cached, hit := get(host)
		if hit {
			ips := family(network, cached.IPs)
			cctx, cancel := context.WithTimeout(ctx, cachePhase)
			c, good, err := dialIPs(cctx, base, network, ips, port)
			cancel()
			if err == nil {
				if time.Since(time.Unix(cached.At, 0)) > sysconf.Dur("ca.dns_refresh") {
					go refresh(host) // 缓存偏旧：这次照用，后台悄悄更新
				}
				if good != ips[0] {
					put(host, promote(cached.IPs, good), cached.Src)
				}
				return c, nil
			}
			lastErr = err
			for _, s := range ips {
				tried[s] = true
			}
		}

		// 2) 系统 DNS
		ips, derr := resolveSystem(ctx, host)
		if derr == nil && len(ips) > 0 {
			if c, good, err := dialIPs(ctx, base, network, subtract(family(network, ips), tried), port); err == nil {
				put(host, promote(ips, good), "dns")
				return c, nil
			} else {
				lastErr = err
			}
			for _, s := range ips {
				tried[s] = true
			}
		} else if derr != nil && lastErr == nil {
			lastErr = derr
		}

		// 3) SSH 跳板解析（失败过的域名短时间内不重复尝试）
		failMu.Lock()
		t, bad := fail[host]
		failMu.Unlock()
		if !bad || time.Since(t) >= negativeTTL {
			sips, serr := do("ssh:"+host, func() ([]string, error) { return resolveSSH(ctx, host) })
			if serr == nil {
				if c, good, err := dialIPs(ctx, base, network, subtract(family(network, sips), tried), port); err == nil {
					put(host, promote(sips, good), "ssh")
					return c, nil
				} else {
					lastErr = err
				}
			} else if derr != nil {
				failMu.Lock()
				fail[host] = time.Now()
				failMu.Unlock()
				lastErr = errors.New(derr.Error() + "；SSH 解析：" + serr.Error())
			}
		}
		if lastErr == nil {
			lastErr = errors.New("无法解析 " + host)
		}
		return nil, lastErr
	}
}

// refresh 后台用系统 DNS 刷新一个偏旧的缓存项（不动 SSH）。
func refresh(host string) {
	// 域名已被撤销时每次拨号都会触发刷新：10 分钟内只试一次；失败不会动缓存里的 IP
	failMu.Lock()
	if t, ok := refreshAt[host]; ok && time.Since(t) < 10*time.Minute {
		failMu.Unlock()
		return
	}
	refreshAt[host] = time.Now()
	failMu.Unlock()
	_, _ = do("refresh:"+host, func() ([]string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		ips, err := resolveSystem(ctx, host)
		if err == nil && len(ips) > 0 {
			put(host, ips, "dns")
		}
		return ips, err
	})
}
