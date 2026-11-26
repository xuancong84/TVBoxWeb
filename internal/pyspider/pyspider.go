// Package pyspider 让 TVBox 配置里的 Python 站源（type=3、api 指向 .py 文件）在 Web 端可用。
//
// 做法：Go 端常驻一个 Python worker 进程（pyfiles/worker.py，内嵌在二进制里），
// 通过 stdin/stdout 的「一行一个 JSON」协议调用站源的 homeContent / categoryContent /
// detailContent / searchContent / playerContent / localProxy。站源文件（.py）从订阅里
// 下载后缓存在数据目录。没有 Python 或缺依赖时整个能力自动关闭，站源保持「不可用」。
package pyspider

import (
	"bufio"
	"context"
	"crypto/sha1"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xxl6097/TVBoxWeb/internal/sysconf"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/httpc"
	"github.com/xxl6097/TVBoxWeb/internal/model"
)

//go:embed pyfiles
var pyFiles embed.FS

// RelayURL 是 Go 端本机的「跳板中转」接口地址（含密钥）；worker 里的脚本直连失败时
// 通过它让 Go 走 SSH_CMDS_GEO_BLOCK 跳板重试。不把 SSH 密钥/命令交给第三方脚本。
var RelayURL string

// Fetch 下载站源文件的函数；默认直连，上层可以替换成「带地区限制跳板兜底」的版本。
var Fetch = func(ctx context.Context, u string) (string, error) {
	return httpc.TextCtx(ctx, u, nil, sysconf.Dur("to.spider_dl"))
}

var (
	mu        sync.Mutex
	dir       string // 数据目录下的 pyspider/
	pyBin     string
	available atomic.Bool
	reason    string

	proc     *worker
	seq      atomic.Int64
	fileLock sync.Mutex
)

// Available 站源能力是否可用（找到了带全部依赖的 Python）。
func Available() bool { return available.Load() }

// Reason 不可用时的原因（给设置页展示）。
func Reason() string { mu.Lock(); defer mu.Unlock(); return reason }

// Init 在数据目录里释放 worker 与基类，并探测 Python 环境。启动时调用一次。
func Init(dataDir string) {
	mu.Lock()
	defer mu.Unlock()
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	dir = filepath.Join(abs, "pyspider")
	if err := os.MkdirAll(filepath.Join(dir, "spiders"), 0o700); err != nil {
		reason = "无法创建目录：" + err.Error()
		return
	}
	err = fs.WalkDir(pyFiles, "pyfiles", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := pyFiles.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, strings.TrimPrefix(p, "pyfiles/"))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	})
	if err != nil {
		reason = "释放 Python 文件失败：" + err.Error()
		return
	}
	for _, c := range candidates() {
		cmd := exec.Command(c, "-c", "import requests, Crypto, lxml, bs4, pyquery")
		if out, err := cmd.CombinedOutput(); err == nil {
			pyBin = c
			available.Store(true)
			reason = ""
			return
		} else if reason == "" {
			reason = fmt.Sprintf("%s 缺少依赖（需要 requests pycryptodome lxml beautifulsoup4 pyquery）：%s", c, lastLine(string(out)))
		}
	}
	if reason == "" {
		reason = "没有找到 Python 3（可用环境变量 PYSPIDER_PYTHON 指定）"
	}
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func candidates() []string {
	var out []string
	if p := strings.TrimSpace(sysconf.Str("rt.python")); p != "" {
		out = append(out, p)
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, "miniconda3", "bin", "python"))
	}
	for _, n := range []string{"python3", "python"} {
		if p, err := exec.LookPath(n); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// ---------- 站源定位 ----------

// IsPySite 判断一个原始站源是不是 Python 站源（type=3，api 或 ext 指向 .py）。
func IsPySite(typ int, api, ext string) bool {
	if typ != 3 {
		return false
	}
	_, _, ok := Spec(api, ext, "")
	return ok
}

// Spec 求出站源文件地址和传给 init() 的 ext。
// base 是该站源所在配置的地址，api/ext 里的相对路径相对它解析。
func Spec(api, ext, base string) (fileURL, initExt string, ok bool) {
	pick := ""
	switch {
	case isPyPath(api):
		pick = api
		initExt = ext
	case isPyPath(ext) && strings.HasPrefix(api, "py_"):
		pick = ext
	default:
		return "", "", false
	}
	pick = strings.TrimSpace(pick)
	if i := strings.IndexAny(pick, "?#"); i >= 0 {
		pick = pick[:i]
	}
	if strings.HasPrefix(pick, "http://") || strings.HasPrefix(pick, "https://") {
		return pick, initExt, true
	}
	if base == "" {
		return pick, initExt, true // 仅用于判断类型
	}
	bu, err := url.Parse(base)
	if err != nil {
		return "", "", false
	}
	ru, err := bu.Parse(pick)
	if err != nil {
		return "", "", false
	}
	return ru.String(), initExt, true
}

func isPyPath(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	return strings.HasSuffix(s, ".py")
}

// ensureFile 把站源文件下载到本地（已有且 24 小时内下的直接用；刷新失败沿用旧文件）。
func ensureFile(ctx context.Context, fileURL string) (string, error) {
	sum := sha1.Sum([]byte(fileURL))
	p := filepath.Join(dir, "spiders", hex.EncodeToString(sum[:8])+".py")
	st, err := os.Stat(p)
	if err == nil && time.Since(st.ModTime()) < 24*time.Hour {
		return p, nil
	}
	fileLock.Lock()
	defer fileLock.Unlock()
	if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) < 24*time.Hour {
		return p, nil
	}
	txt, ferr := Fetch(ctx, fileURL)
	if ferr != nil || !strings.Contains(txt, "Spider") {
		if err == nil {
			return p, nil // 沿用旧文件
		}
		if ferr == nil {
			ferr = errors.New("下载到的内容不是 Python 站源")
		}
		return "", ferr
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(txt), 0o600); err != nil {
		return "", err
	}
	return p, os.Rename(tmp, p)
}

// ---------- worker 进程 ----------

type response struct {
	ID   int64           `json:"id"`
	OK   bool            `json:"ok"`
	Data json.RawMessage `json:"data"`
	Err  string          `json:"err"`
}

type worker struct {
	cmd     *exec.Cmd
	stdin   *json.Encoder
	inMu    sync.Mutex
	pending sync.Map // id -> chan response
	dead    chan struct{}
}

func startWorker() (*worker, error) {
	cmd := exec.Command(pyBin, "-u", filepath.Join(dir, "worker.py"))
	cmd.Dir = filepath.Join(dir, "spiders")
	// 精简环境：不把本进程的密钥类环境变量交给第三方站源代码。
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "LANG=C.UTF-8", "PYTHONDONTWRITEBYTECODE=1"}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	if RelayURL != "" {
		env = append(env, "SPIDER_RELAY="+RelayURL)
	}
	cmd.Env = env
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	w := &worker{cmd: cmd, stdin: json.NewEncoder(in), dead: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<20), 128<<20)
		for sc.Scan() {
			var r response
			if json.Unmarshal(sc.Bytes(), &r) != nil {
				continue
			}
			if ch, ok := w.pending.LoadAndDelete(r.ID); ok {
				ch.(chan response) <- r
			}
		}
		close(w.dead)
		w.pending.Range(func(k, v any) bool {
			select {
			case v.(chan response) <- response{ID: k.(int64), Err: "Python worker 已退出"}:
			default:
			}
			return true
		})
		_ = cmd.Wait()
	}()
	return w, nil
}

func getWorker() (*worker, error) {
	mu.Lock()
	defer mu.Unlock()
	if proc != nil {
		select {
		case <-proc.dead:
			proc = nil
		default:
			return proc, nil
		}
	}
	w, err := startWorker()
	if err != nil {
		return nil, err
	}
	proc = w
	return w, nil
}

// Call 调用站源的某个操作，返回 worker 给出的 JSON。
func Call(ctx context.Context, site model.Site, op string, args map[string]any) (json.RawMessage, error) {
	if !Available() {
		return nil, errors.New("Python 站源未启用：" + Reason())
	}
	fileURL, initExt, ok := Spec(site.API, rawExt(site), site.Base)
	if !ok {
		return nil, errors.New("站源文件地址无效")
	}
	file, err := ensureFile(ctx, fileURL)
	if err != nil {
		return nil, fmt.Errorf("下载站源文件失败：%w", err)
	}
	w, err := getWorker()
	if err != nil {
		return nil, err
	}
	id := seq.Add(1)
	ch := make(chan response, 1)
	w.pending.Store(id, ch)
	w.inMu.Lock()
	err = w.stdin.Encode(map[string]any{"id": id, "op": op, "site": site.Key, "file": file, "ext": initExt, "args": args})
	w.inMu.Unlock()
	if err != nil {
		w.pending.Delete(id)
		return nil, err
	}
	select {
	case r := <-ch:
		if !r.OK {
			return nil, errors.New(r.Err)
		}
		return r.Data, nil
	case <-ctx.Done():
		w.pending.Delete(id)
		return nil, ctx.Err()
	}
}

func rawExt(s model.Site) string { return s.Ext }

// Stop 结束 worker（进程退出时调用）。
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if proc != nil && proc.cmd.Process != nil {
		_ = proc.cmd.Process.Kill()
	}
	proc = nil
}

// ---------- 播放地址令牌 ----------

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
func unb64(s string) (string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return string(b), err == nil
}

// 令牌里不含 "://" 和 "."，前端的 fixPlayable/isDirectUrl 不会误改它。

// PlayToken 把（站源 key，线路名，集的 id）打成一个播放令牌。
func PlayToken(siteKey, flag, id string) string {
	return "pyplay:" + b64(siteKey) + ":" + b64(flag) + ":" + b64(id)
}

// ParsePlayToken 解开播放令牌。
func ParsePlayToken(tok string) (siteKey, flag, id string, ok bool) {
	if !strings.HasPrefix(tok, "pyplay:") {
		return
	}
	p := strings.Split(strings.TrimPrefix(tok, "pyplay:"), ":")
	if len(p) != 3 {
		return
	}
	var ok1, ok2, ok3 bool
	siteKey, ok1 = unb64(p[0])
	flag, ok2 = unb64(p[1])
	id, ok3 = unb64(p[2])
	return siteKey, flag, id, ok1 && ok2 && ok3
}

// ProxyToken 本地代理令牌：站源返回 http://127.0.0.1:9978/proxy?... 这种「本地代理」地址时，
// 换成它，再由 /api/proxy 解开后调用站源的 localProxy。
func ProxyToken(siteKey, query string) string { return "pyproxy:" + b64(siteKey) + ":" + b64(query) }

// ParseProxyToken 解开本地代理令牌。
func ParseProxyToken(tok string) (siteKey, query string, ok bool) {
	if !strings.HasPrefix(tok, "pyproxy:") {
		return
	}
	p := strings.Split(strings.TrimPrefix(tok, "pyproxy:"), ":")
	if len(p) != 2 {
		return
	}
	var ok1, ok2 bool
	siteKey, ok1 = unb64(p[0])
	query, ok2 = unb64(p[1])
	return siteKey, query, ok1 && ok2
}

// LocalProxyQuery 判断一个地址是不是 TVBox 约定的「本地代理」地址，是的话返回它的 query。
func LocalProxyQuery(u string) (string, bool) {
	pu, err := url.Parse(u)
	if err != nil {
		return "", false
	}
	h := pu.Hostname()
	if h != "127.0.0.1" && h != "localhost" {
		return "", false
	}
	if !strings.HasPrefix(pu.Path, "/proxy") {
		return "", false
	}
	return pu.RawQuery, true
}
