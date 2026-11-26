// Package jsspider 让 TVBox 配置里的 JS 站源（drpy2 规则：type=3，api 是 drpy2 引擎、
// ext 是规则 .js 文件）在 Web 端可用。
//
// 做法：Go 起一小池 Node worker 进程（jsfiles/ 内嵌在二进制里，启动时释放到数据目录），
// worker 里加载 drpy2 引擎并补上宿主能力（req、pdfh/pdfa/pd、cheerio、CryptoJS 等），
// 用跟 pyspider 同一套「一行一个 JSON」协议调用 home/category/detail/search/play/proxy。
// 引擎的网络请求是同步阻塞的（curl 子进程），所以同一个 worker 内串行、靠多个 worker 并发。
package jsspider

import (
	"bufio"
	"context"
	"crypto/sha1"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xxl6097/TVBoxWeb/internal/sysconf"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/httpc"
	"github.com/xxl6097/TVBoxWeb/internal/model"
)

//go:embed all:jsfiles
var jsFiles embed.FS

// RelayURL 是 Go 端本机的「跳板中转」接口地址（含密钥）；worker 里的脚本直连失败时
// 通过它让 Go 走 SSH_CMDS_GEO_BLOCK 跳板重试。不把 SSH 密钥/命令交给第三方脚本。
var RelayURL string

// Fetch 下载规则文件的函数；默认直连，上层可替换成带地区限制跳板兜底的版本。
var Fetch = func(ctx context.Context, u string) (string, error) {
	return httpc.TextCtx(ctx, u, nil, sysconf.Dur("to.spider_dl"))
}

var (
	mu        sync.Mutex
	dir       string
	nodeBin   string
	available atomic.Bool
	reason    string
	maxProcs  = 4

	procs    []*worker
	seq      atomic.Int64
	fileLock sync.Mutex
)

// Available 能力是否可用（找到了 Node ≥ 18 和 curl）。
func Available() bool { return available.Load() }

// Reason 不可用的原因。
func Reason() string { mu.Lock(); defer mu.Unlock(); return reason }

// Init 释放 worker 文件并探测 Node 环境。启动时调用一次。
func Init(dataDir string) {
	mu.Lock()
	defer mu.Unlock()
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	dir = filepath.Join(abs, "jsspider")
	if err := os.MkdirAll(filepath.Join(dir, "rules"), 0o700); err != nil {
		reason = "无法创建目录：" + err.Error()
		return
	}
	err = fs.WalkDir(jsFiles, "jsfiles", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := jsFiles.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, strings.TrimPrefix(p, "jsfiles/"))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	})
	if err != nil {
		reason = "释放 JS 文件失败：" + err.Error()
		return
	}
	maxProcs = sysconf.Int("cc.js_workers")
	if n, _ := strconv.Atoi(os.Getenv("JSSPIDER_WORKERS")); n > 0 && sysconf.Int("cc.js_workers") == 4 {
		maxProcs = n
	}
	if _, err := exec.LookPath("curl"); err != nil {
		reason = "JS 站源需要系统里有 curl"
		return
	}
	cands := []string{}
	if p := strings.TrimSpace(sysconf.Str("rt.node")); p != "" {
		cands = append(cands, p)
	}
	if p, err := exec.LookPath("node"); err == nil {
		cands = append(cands, p)
	}
	cands = append(cands, "/usr/local/bin/node", "/usr/bin/node")
	for _, c := range cands {
		out, err := exec.Command(c, "-p", "process.versions.node").Output()
		if err != nil {
			continue
		}
		major, _ := strconv.Atoi(strings.SplitN(strings.TrimSpace(string(out)), ".", 2)[0])
		if major >= 18 {
			nodeBin = c
			available.Store(true)
			reason = ""
			return
		}
		reason = fmt.Sprintf("Node 版本太低（%s），需要 ≥ 18", strings.TrimSpace(string(out)))
	}
	if reason == "" {
		reason = "没有找到 Node.js（可用环境变量 JSSPIDER_NODE 指定）"
	}
}

// ---------- 站源定位 ----------

// IsJSSite 判断是不是 drpy2 规则站源。
func IsJSSite(typ int, api, ext string) bool {
	if typ != 3 {
		return false
	}
	_, _, ok := Spec(api, ext, "")
	return ok
}

func pathOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	return s
}

// Spec 求出规则文件地址 ruleURL 和传给 init() 的 initExt（有 ? 参数的规则原样把完整地址交给引擎，
// 让它自己带参数去取；没参数的由 Go 先下载好，把代码交给引擎）。
func Spec(api, ext, base string) (ruleURL, initExt string, ok bool) {
	b := strings.ToLower(path.Base(pathOf(api)))
	if !strings.HasPrefix(b, "drpy") || !strings.HasSuffix(b, ".js") {
		return "", "", false
	}
	p := pathOf(ext)
	if !strings.HasSuffix(strings.ToLower(p), ".js") {
		return "", "", false
	}
	full := strings.TrimSpace(ext)
	if !strings.HasPrefix(full, "http://") && !strings.HasPrefix(full, "https://") {
		if base == "" {
			return p, "", true
		}
		bu, err := url.Parse(base)
		if err != nil {
			return "", "", false
		}
		ru, err := bu.Parse(full)
		if err != nil {
			return "", "", false
		}
		full = ru.String()
	}
	if strings.ContainsAny(full, "?") {
		return pathOnly(full), full, true
	}
	return full, "", true
}

func pathOnly(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i]
	}
	return u
}

func ensureFile(ctx context.Context, ruleURL string) (string, error) {
	sum := sha1.Sum([]byte(ruleURL))
	p := filepath.Join(dir, "rules", hex.EncodeToString(sum[:8])+".js")
	st, err := os.Stat(p)
	if err == nil && time.Since(st.ModTime()) < 24*time.Hour {
		return p, nil
	}
	fileLock.Lock()
	defer fileLock.Unlock()
	if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) < 24*time.Hour {
		return p, nil
	}
	txt, ferr := Fetch(ctx, ruleURL)
	if ferr != nil || !strings.Contains(txt, "rule") || strings.HasPrefix(strings.TrimSpace(txt), "<") {
		if err == nil {
			return p, nil
		}
		if ferr == nil {
			ferr = errors.New("下载到的内容不是 drpy2 规则")
		}
		return "", ferr
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(txt), 0o600); err != nil {
		return "", err
	}
	return p, os.Rename(tmp, p)
}

// ---------- worker 池 ----------

type response struct {
	ID   int64           `json:"id"`
	OK   bool            `json:"ok"`
	Data json.RawMessage `json:"data"`
	Err  string          `json:"err"`
}

type worker struct {
	cmd      *exec.Cmd
	stdin    *json.Encoder
	inMu     sync.Mutex
	pending  sync.Map
	dead     chan struct{}
	inflight atomic.Int32
	loaded   sync.Map // site key -> struct{}
}

func startWorker() (*worker, error) {
	cmd := exec.Command(nodeBin, "--max-old-space-size=256", filepath.Join(dir, "worker.mjs"))
	cmd.Dir = filepath.Join(dir, "rules")
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "LANG=C.UTF-8", "TMPDIR=" + os.TempDir()}
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
			case v.(chan response) <- response{ID: k.(int64), Err: "JS worker 已退出"}:
			default:
			}
			return true
		})
		_ = cmd.Wait()
	}()
	return w, nil
}

// pick 选一个 worker：优先「已经加载过这个站源且空闲」的，其次任意空闲的，
// 还能再开就再开一个，最后才排到最不忙的那个后面。
func pick(site string) (*worker, error) {
	mu.Lock()
	defer mu.Unlock()
	alive := procs[:0]
	for _, w := range procs {
		select {
		case <-w.dead:
		default:
			alive = append(alive, w)
		}
	}
	procs = alive
	var idleLoaded, idle, least *worker
	for _, w := range procs {
		n := w.inflight.Load()
		if _, ok := w.loaded.Load(site); ok && n == 0 && idleLoaded == nil {
			idleLoaded = w
		}
		if n == 0 && idle == nil {
			idle = w
		}
		if least == nil || n < least.inflight.Load() {
			least = w
		}
	}
	switch {
	case idleLoaded != nil:
		return idleLoaded, nil
	case idle != nil:
		return idle, nil
	case len(procs) < maxProcs:
		w, err := startWorker()
		if err != nil {
			return nil, err
		}
		procs = append(procs, w)
		return w, nil
	}
	return least, nil
}

// Call 调用站源的某个操作。
func Call(ctx context.Context, site model.Site, op string, args map[string]any) (json.RawMessage, error) {
	if !Available() {
		return nil, errors.New("JS 站源未启用：" + Reason())
	}
	ruleURL, initExt, ok := Spec(site.API, site.Ext, site.Base)
	if !ok {
		return nil, errors.New("站源规则地址无效")
	}
	if initExt == "" {
		file, err := ensureFile(ctx, ruleURL)
		if err != nil {
			return nil, fmt.Errorf("下载规则文件失败：%w", err)
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		initExt = string(b)
	}
	w, err := pick(site.Key)
	if err != nil {
		return nil, err
	}
	id := seq.Add(1)
	ch := make(chan response, 1)
	w.pending.Store(id, ch)
	w.inflight.Add(1)
	defer w.inflight.Add(-1)
	w.inMu.Lock()
	err = w.stdin.Encode(map[string]any{"id": id, "op": op, "site": site.Key, "ext": initExt, "args": args})
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
		w.loaded.Store(site.Key, struct{}{})
		return r.Data, nil
	case <-ctx.Done():
		w.pending.Delete(id)
		return nil, ctx.Err()
	}
}

// Stop 结束所有 worker。
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	for _, w := range procs {
		if w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
		}
	}
	procs = nil
}
