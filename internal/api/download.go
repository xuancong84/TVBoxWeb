package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/xxl6097/TVBoxWeb/internal/crawler"
	"github.com/xxl6097/TVBoxWeb/internal/httpc"
	"github.com/xxl6097/TVBoxWeb/internal/netguard"
	"github.com/xxl6097/TVBoxWeb/internal/resolve"
	"github.com/xxl6097/TVBoxWeb/internal/store"
	"github.com/xxl6097/TVBoxWeb/internal/sysconf"
)

// 视频下载：服务端把一集（或整部剧的所有集）下载到「视频下载文件夹」。
// 主体数据直连下载，绝不走 SSH 跳板。m3u8 按分片并发下载、按序写入（支持 AES-128），
// 直链（mp4 等）整文件流式保存。任务串行执行，进度可在前端查看。

type dlJob struct {
	ID      string `json:"id"`
	Vod     string `json:"vod"`
	Name    string `json:"name"`   // 集名
	File    string `json:"file"`   // 完成后的文件路径
	Status  string `json:"status"` // queued/running/done/error/canceled
	Err     string `json:"err,omitempty"`
	Done    int    `json:"done"`  // 已完成分片数（直链为 0）
	Total   int    `json:"total"` // 总分片数（直链为 0）
	Bytes   int64  `json:"bytes"`
	Size    int64  `json:"size"` // 直链总大小，未知为 0
	Updated int64  `json:"updated"`
	Kind    string `json:"kind,omitempty"` // 空/"download"=下载；"convert"=转换为 mp4
	Keep    bool   `json:"keep,omitempty"` // convert：转换后是否保留原始文件

	urls   []string
	cancel context.CancelFunc
	seq    int
}

var dl = struct {
	mu    sync.Mutex
	jobs  []*dlJob
	seq   int
	queue chan *dlJob
	once  sync.Once
}{queue: make(chan *dlJob, 4096)}

func dlWorker() {
	for j := range dl.queue {
		dl.mu.Lock()
		if j.Status != "queued" {
			dl.mu.Unlock()
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		j.cancel = cancel
		j.Status = "running"
		j.Updated = time.Now().Unix()
		dl.mu.Unlock()

		err := runDownload(ctx, j)

		dl.mu.Lock()
		switch {
		case ctx.Err() != nil:
			j.Status = "canceled"
		case err != nil:
			j.Status = "error"
			j.Err = err.Error()
		default:
			j.Status = "done"
		}
		j.Updated = time.Now().Unix()
		j.cancel = nil
		dl.mu.Unlock()
		cancel()
	}
}

var badName = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]+`)

func safeName(s string) string {
	s = strings.TrimSpace(badName.ReplaceAllString(s, "_"))
	s = strings.Trim(s, ". ")
	if s == "" {
		s = "未命名"
	}
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	return s
}

// hDownload POST /api/download {vod, items:[{name, urls:[...]}]}：加入下载队列。
func hDownload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Vod   string `json:"vod"`
		Force bool   `json:"force"` // 已有/进行中的同名集：true=删除旧文件并重新下载
		Items []struct {
			Name string   `json:"name"`
			URLs []string `json:"urls"`
		} `json:"items"`
	}
	if err := body(r, &req); err != nil || len(req.Items) == 0 {
		fail(w, 400, "参数错误")
		return
	}
	if len(req.Items) > 500 {
		fail(w, 400, "一次最多下载 500 集")
		return
	}
	dir := sysconf.DownloadDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, 500, "无法创建下载文件夹："+err.Error())
		return
	}
	dl.once.Do(func() { go dlWorker() })
	vodDir := filepath.Join(dir, safeName(req.Vod))
	dl.mu.Lock()
	// 已下载（磁盘上有文件）或排队/下载中的同名集
	var conflicts []string
	for _, it := range req.Items {
		if dlExists(vodDir, it.Name) || dlActive(req.Vod, it.Name) != nil {
			conflicts = append(conflicts, it.Name)
		}
	}
	if len(conflicts) > 0 && !req.Force {
		dl.mu.Unlock()
		ok(w, map[string]any{"added": 0, "dir": dir, "conflicts": conflicts})
		return
	}
	if req.Force {
		for _, it := range req.Items {
			keep := dl.jobs[:0]
			for _, j := range dl.jobs {
				if j.Kind != "convert" && j.Vod == req.Vod && j.Name == it.Name {
					if j.cancel != nil {
						j.cancel() // 正在下的：取消，.part 由其自行清理
					}
					if j.Status == "queued" {
						j.Status = "canceled"
					}
					continue
				}
				keep = append(keep, j)
			}
			dl.jobs = keep
			dlRemoveFiles(vodDir, it.Name)
		}
	}
	added := 0
	for _, it := range req.Items {
		var urls []string
		for _, u := range it.URLs {
			if u = strings.TrimSpace(u); u != "" {
				urls = append(urls, u)
			}
		}
		if len(urls) == 0 {
			continue
		}
		dl.seq++
		j := &dlJob{ID: uuid.NewString(), Vod: req.Vod, Name: it.Name, Status: "queued",
			urls: urls, seq: dl.seq, Updated: time.Now().Unix()}
		dl.jobs = append(dl.jobs, j)
		dl.queue <- j
		added++
	}
	dl.mu.Unlock()
	ok(w, map[string]any{"added": added, "dir": dir})
}

// hDownloads GET /api/downloads：任务列表与下载文件夹。
func hDownloads(w http.ResponseWriter, r *http.Request) {
	dl.mu.Lock()
	out := make([]dlJob, 0, len(dl.jobs))
	for _, j := range dl.jobs {
		out = append(out, *j)
	}
	dl.mu.Unlock()
	sort.SliceStable(out, func(a, b int) bool { return out[a].seq < out[b].seq })
	ok(w, map[string]any{"dir": sysconf.DownloadDir(), "jobs": out})
}

// hDownloadCtl POST /api/download/cancel|remove {id}；id 为空 + all=true 时清理已结束任务。
func hDownloadCtl(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string `json:"id"`
		Clear bool   `json:"clear"` // 清除所有已结束任务
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	cancelOnly := strings.HasSuffix(r.URL.Path, "/cancel")
	dl.mu.Lock()
	defer dl.mu.Unlock()
	keep := dl.jobs[:0]
	for _, j := range dl.jobs {
		hit := req.ID == j.ID
		finished := j.Status == "done" || j.Status == "error" || j.Status == "canceled"
		if cancelOnly && hit {
			if j.Status == "queued" {
				j.Status = "canceled"
			} else if j.cancel != nil {
				j.cancel()
			}
		}
		if !cancelOnly && ((hit && finished) || (req.Clear && finished)) {
			continue
		}
		if !cancelOnly && hit && !finished { // 进行中的先取消
			if j.cancel != nil {
				j.cancel()
			}
			if j.Status == "queued" {
				j.Status = "canceled"
			}
		}
		keep = append(keep, j)
	}
	dl.jobs = keep
	ok(w, map[string]any{"ok": true})
}

func dlUpdate(j *dlJob, f func()) {
	dl.mu.Lock()
	f()
	j.Updated = time.Now().Unix()
	dl.mu.Unlock()
}

// runDownload 依次尝试该集的各个备用地址，任一成功即完成。
func runDownload(ctx context.Context, j *dlJob) error {
	if j.Kind == "convert" {
		return runConvert(ctx, j)
	}
	var last error
	for _, raw := range j.urls {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := downloadOne(ctx, j, raw)
		if err == nil {
			return nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("没有可用的播放地址")
	}
	return last
}

func dlGet(ctx context.Context, target string, hdr map[string]string) (*http.Response, error) {
	if err := netguard.Check(target); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", httpc.WebUA)
	req.Header.Set("Accept", "*/*")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := httpc.StreamClient.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 {
		res.Body.Close()
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return res, nil
}

func dlBytes(ctx context.Context, target string, hdr map[string]string, max int64) ([]byte, string, error) {
	res, err := dlGet(ctx, target, hdr)
	if err != nil {
		// 与播放代理一致：直连被地区封锁时，m3u8 / 密钥这类小文件走 SSH 跳板重试
		// （视频分片等主体数据绝不走 SSH）。
		if relayEligible(target) {
			if data, rerr := crawler.FetchSmallViaGeoRelay(ctx, target, relayMaxBytes); rerr == nil && int64(len(data)) <= max {
				return data, target, nil
			}
		}
		return nil, "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, max))
	final := target
	if res.Request != nil && res.Request.URL != nil {
		final = res.Request.URL.String()
	}
	return b, final, err
}

func downloadOne(ctx context.Context, j *dlJob, raw string) error {
	var rs resolve.Resolved
	if strings.HasPrefix(raw, "pyplay:") {
		rs = resolvePyPlay(ctx, raw)
	} else {
		rs = resolve.ResolveCtx(ctx, raw, store.Parses())
	}
	target := rs.URL
	if strings.HasPrefix(target, "pyproxy:") {
		return errors.New("暂不支持下载本地代理（py/js）源")
	}
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		return errors.New("无法解析出播放地址：" + rs.Via)
	}
	// 与播放页的 fixPlayable 一致：末段没有扩展名的当 m3u8 目录补 index.m3u8
	target = dlFixURL(target)

	dir := filepath.Join(sysconf.DownloadDir(), safeName(j.Vod))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	base := filepath.Join(dir, safeName(j.Name))

	head, final, err := dlBytes(ctx, target, rs.Header, 4<<20)
	if err != nil && len(head) == 0 {
		return err
	}
	if bytes.HasPrefix(bytes.TrimSpace(head), []byte("#EXTM3U")) {
		return dlHLS(ctx, j, string(head), final, rs.Header, base)
	}
	return dlDirect(ctx, j, target, rs.Header, base)
}

func dlFixURL(u string) string {
	lu := strings.ToLower(u)
	for _, e := range []string{".m3u8", ".mp4", ".flv", ".mkv", ".ts", ".m4v"} {
		if strings.Contains(lu, e) {
			return u
		}
	}
	noQ := strings.TrimRight(strings.SplitN(u, "?", 2)[0], "/")
	if i := strings.LastIndex(noQ, "/"); i >= 0 && i+1 < len(noQ) && !strings.Contains(noQ[i+1:], ".") && strings.Contains(noQ, "://") {
		return noQ + "/index.m3u8"
	}
	return u
}

// dlDirect 直链整文件流式保存。
func dlDirect(ctx context.Context, j *dlJob, target string, hdr map[string]string, base string) error {
	res, err := dlGet(ctx, target, hdr)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	ext := ".mp4"
	if u, e := url.Parse(target); e == nil {
		if x := strings.ToLower(filepath.Ext(u.Path)); x == ".flv" || x == ".mkv" || x == ".m4v" || x == ".mp4" || x == ".ts" {
			ext = x
		}
	}
	dlUpdate(j, func() { j.Size = res.ContentLength; j.Done, j.Total, j.Bytes = 0, 0, 0 })
	out := base + ext
	part := out + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	buf := make([]byte, 256<<10)
	for {
		n, rerr := res.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(part)
				return werr
			}
			dlUpdate(j, func() { j.Bytes += int64(n) })
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(part)
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return err
	}
	if err := os.Rename(part, out); err != nil {
		return err
	}
	dlUpdate(j, func() { j.File = out })
	return nil
}

var attrRe = regexp.MustCompile(`([A-Z0-9-]+)=("[^"]*"|[^,]*)`)

func attrs(line string) map[string]string {
	m := map[string]string{}
	for _, a := range attrRe.FindAllStringSubmatch(line, -1) {
		m[a[1]] = strings.Trim(a[2], `"`)
	}
	return m
}

type hlsKey struct {
	method string
	key    []byte
	iv     []byte // nil = 用分片序号
}

type hlsSeg struct {
	url  string
	seq  int64
	key  *hlsKey
	dur  string // EXTINF 时长原文（秒），写入 .jm3u8
	disc bool   // 该分片前有 #EXT-X-DISCONTINUITY
}

// dlHLS 下载 m3u8：主列表选带宽最高的子流；分片并发取、按序写入。
func dlHLS(ctx context.Context, j *dlJob, text, base string, hdr map[string]string, outBase string) error {
	for depth := 0; depth < 3 && strings.Contains(text, "#EXT-X-STREAM-INF"); depth++ {
		best, bw := "", int64(-1)
		lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
		for i, ln := range lines {
			if !strings.HasPrefix(ln, "#EXT-X-STREAM-INF") {
				continue
			}
			b, _ := strconv.ParseInt(attrs(ln)["BANDWIDTH"], 10, 64)
			for k := i + 1; k < len(lines); k++ {
				if s := strings.TrimSpace(lines[k]); s != "" && !strings.HasPrefix(s, "#") {
					if b > bw {
						bw, best = b, absURL(base, s)
					}
					break
				}
			}
		}
		if best == "" {
			break
		}
		b, final, err := dlBytes(ctx, best, hdr, 4<<20)
		if err != nil && len(b) == 0 {
			return err
		}
		text, base = string(b), final
	}

	var segs []hlsSeg
	var initURL string
	var cur *hlsKey
	var seq int64
	var pendDur string
	var pendDisc bool
	for _, ln := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case ln == "":
		case strings.HasPrefix(ln, "#EXTINF:"):
			pendDur = strings.TrimPrefix(ln, "#EXTINF:")
			if i := strings.Index(pendDur, ","); i >= 0 {
				pendDur = pendDur[:i]
			}
			pendDur = strings.TrimSpace(pendDur)
		case strings.HasPrefix(ln, "#EXT-X-DISCONTINUITY") && !strings.Contains(ln, "SEQUENCE"):
			pendDisc = true
		case strings.HasPrefix(ln, "#EXT-X-MEDIA-SEQUENCE:"):
			seq, _ = strconv.ParseInt(strings.TrimPrefix(ln, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
		case strings.HasPrefix(ln, "#EXT-X-MAP:"):
			if u := attrs(ln)["URI"]; u != "" {
				initURL = absURL(base, u)
			}
		case strings.HasPrefix(ln, "#EXT-X-KEY:"):
			a := attrs(ln)
			switch strings.ToUpper(a["METHOD"]) {
			case "NONE", "":
				cur = nil
			case "AES-128":
				kb, _, err := dlBytes(ctx, absURL(base, a["URI"]), hdr, 1<<16)
				if err != nil || len(kb) != 16 {
					return fmt.Errorf("获取解密密钥失败")
				}
				k := &hlsKey{method: "AES-128", key: kb}
				if iv := strings.TrimPrefix(strings.TrimPrefix(a["IV"], "0x"), "0X"); iv != "" {
					b, err := hex.DecodeString(fmt.Sprintf("%032s", iv))
					if err != nil || len(b) != 16 {
						return fmt.Errorf("IV 格式错误")
					}
					k.iv = b
				}
				cur = k
			default:
				return errors.New("不支持的加密方式：" + a["METHOD"])
			}
		case strings.HasPrefix(ln, "#"):
		default:
			if pendDur == "" {
				pendDur = "0"
			}
			segs = append(segs, hlsSeg{url: absURL(base, ln), seq: seq, key: cur, dur: pendDur, disc: pendDisc})
			pendDur, pendDisc = "", false
			seq++
		}
	}
	if len(segs) == 0 {
		return errors.New("播放列表里没有分片")
	}
	// 拼接后的整体文件：TS 分片叫 .ts；fMP4（带 init 段）叫 .fmp4，避免被当成可直接播放的普通 mp4。
	// 旁边的 .jm3u8 记录每个分片的时长和字节区间，播放时据此生成 BYTERANGE 播放列表交给 hls.js。
	ext := ".ts"
	if initURL != "" {
		ext = ".fmp4"
	}
	out := outBase + ext
	part := out + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	abort := func(e error) error {
		bw.Flush()
		f.Close()
		os.Remove(part)
		return e
	}
	dlUpdate(j, func() { j.Total, j.Done, j.Bytes, j.Size = len(segs), 0, 0, 0 })
	var initLen int64
	if initURL != "" {
		b, _, err := dlBytes(ctx, initURL, hdr, 16<<20)
		if err != nil {
			return abort(err)
		}
		bw.Write(b)
		initLen = int64(len(b))
	}
	sizes := make([]int64, len(segs)) // 每个分片（解密后）实际写入的字节数

	conc := sysconf.Int("download.conc")
	if conc < 1 {
		conc = 6
	}
	type res struct {
		b   []byte
		err error
	}
	chans := make([]chan res, len(segs))
	sem := make(chan struct{}, conc)
	cctx, stop := context.WithCancel(ctx)
	defer stop()
	for i := range segs {
		chans[i] = make(chan res, 1)
	}
	go func() {
		for i := range segs {
			select {
			case sem <- struct{}{}:
			case <-cctx.Done():
				return
			}
			go func(i int) {
				b, err := dlSeg(cctx, segs[i], hdr)
				chans[i] <- res{b, err}
			}(i)
		}
	}()
	for i := range segs {
		var r res
		select {
		case r = <-chans[i]:
		case <-cctx.Done():
			return abort(cctx.Err())
		}
		<-sem
		if r.err != nil {
			stop()
			return abort(fmt.Errorf("第 %d 个分片失败：%w", i+1, r.err))
		}
		if _, err := bw.Write(r.b); err != nil {
			stop()
			return abort(err)
		}
		n := int64(len(r.b))
		sizes[i] = n
		dlUpdate(j, func() { j.Done = i + 1; j.Bytes += n })
	}
	if err := bw.Flush(); err != nil {
		return abort(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return err
	}
	if err := os.Rename(part, out); err != nil {
		return err
	}
	if err := writeJM3U8(outBase+".jm3u8", filepath.Base(out), initLen, segs, sizes); err != nil {
		os.Remove(out)
		return err
	}
	dlUpdate(j, func() { j.File = out })
	return nil
}

// writeJM3U8 写 .jm3u8：一份合法的 m3u8，每个分片带 #EXT-X-BYTERANGE（字节数@偏移），
// 全部指向同一个拼接文件。播放时服务端只需把文件名换成可访问的地址。
func writeJM3U8(path, file string, initLen int64, segs []hlsSeg, sizes []int64) error {
	var b strings.Builder
	maxDur := 1.0
	for _, sg := range segs {
		if d, err := strconv.ParseFloat(sg.dur, 64); err == nil && d > maxDur {
			maxDur = d
		}
	}
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n", int(maxDur+0.999))
	off := int64(0)
	if initLen > 0 {
		fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\",BYTERANGE=\"%d@0\"\n", file, initLen)
		off = initLen
	}
	for i, sg := range segs {
		if sg.disc {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&b, "#EXTINF:%s,\n#EXT-X-BYTERANGE:%d@%d\n%s\n", sg.dur, sizes[i], off, file)
		off += sizes[i]
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	tmp := path + ".part"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// dlSeg 取一个分片（最多重试 3 次）并按需解密。
func dlSeg(ctx context.Context, s hlsSeg, hdr map[string]string) ([]byte, error) {
	var last error
	for try := 0; try < 3; try++ {
		if try > 0 {
			select {
			case <-time.After(time.Duration(try) * 800 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		b, _, err := dlBytes(ctx, s.url, hdr, 256<<20)
		if err != nil {
			last = err
			continue
		}
		if s.key != nil {
			iv := s.key.iv
			if iv == nil {
				iv = make([]byte, 16)
				for k := 0; k < 8; k++ {
					iv[15-k] = byte(s.seq >> (8 * k))
				}
			}
			blk, err := aes.NewCipher(s.key.key)
			if err != nil || len(b) == 0 || len(b)%aes.BlockSize != 0 {
				last = errors.New("解密失败")
				continue
			}
			cipher.NewCBCDecrypter(blk, iv).CryptBlocks(b, b)
			if pad := int(b[len(b)-1]); pad >= 1 && pad <= aes.BlockSize && pad <= len(b) {
				b = b[:len(b)-pad]
			}
		}
		return b, nil
	}
	return nil, last
}

// 下载文件夹里一集可能出现的文件：直链成品（.mp4/.flv/.mkv/.m4v），或 m3u8 下载留下的
// 拼接文件（.ts/.fmp4）+ 分片索引（.jm3u8）。
var dlExts = []string{".mp4", ".flv", ".mkv", ".m4v", ".ts", ".fmp4", ".jm3u8"}

// 可交给 <video> 直接播放的普通视频文件；用户自己把 .ts 转成 .mp4 放进来也算（优先级最高）。
var dlDirectExts = []string{".mp4", ".m4v", ".flv"}

// dlExists 该集在下载文件夹里是否已有文件（成品或拼接文件）。
func dlExists(vodDir, name string) bool {
	base := filepath.Join(vodDir, safeName(name))
	for _, e := range dlExts {
		if _, err := os.Stat(base + e); err == nil {
			return true
		}
	}
	return false
}

func dlRemoveFiles(vodDir, name string) {
	base := filepath.Join(vodDir, safeName(name))
	for _, e := range dlExts {
		os.Remove(base + e)
		os.Remove(base + e + ".part")
	}
}

// dlActive 排队中或下载中的同名任务（调用方持有 dl.mu）。
func dlActive(vod, name string) *dlJob {
	for _, j := range dl.jobs {
		if j.Kind != "convert" && j.Vod == vod && j.Name == name && (j.Status == "queued" || j.Status == "running") {
			return j
		}
	}
	return nil
}

func fileOK(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Size() > 0
}

// dlFind 返回这一集的本地播放方式：
//
//	kind="file"：已有普通视频文件（.mp4 等），<video> 直接播；
//	kind="hls" ：有 .jm3u8 + 拼接文件，走 hls.js（BYTERANGE 播放列表）；
//	都没有返回空 kind。
func dlFind(vod, name string) (kind, path string) {
	base := filepath.Join(sysconf.DownloadDir(), safeName(vod), safeName(name))
	for _, e := range dlDirectExts {
		if fileOK(base + e) {
			return "file", base + e
		}
	}
	if fileOK(base + ".jm3u8") {
		for _, e := range []string{".ts", ".fmp4"} {
			if fileOK(base + e) {
				return "hls", base + ".jm3u8"
			}
		}
	}
	return "", ""
}

// hDownloadLocal GET /api/download/local?vod=&name=：该集已下载则返回本地播放地址，播放页据此不走网络。
func hDownloadLocal(w http.ResponseWriter, r *http.Request) {
	vod, name := q(r, "vod"), q(r, "name")
	kind, f := dlFind(vod, name)
	if kind == "" {
		ok(w, map[string]any{"url": ""})
		return
	}
	// 路径末段带真实扩展名，播放器据此挑 hls.js（.m3u8）/ 原生 <video>（.mp4）/ mpegts.js（.flv）
	ext := filepath.Ext(f)
	if kind == "hls" {
		ext = ".m3u8"
	}
	u := "/api/dlfile/local" + ext + "?vod=" + url.QueryEscape(vod) + "&name=" + url.QueryEscape(name)
	ok(w, map[string]any{"url": u, "kind": kind})
}

var uriAttr = regexp.MustCompile(`URI="[^"]*"`)

// hDownloadFile GET /api/dlfile/local.<ext>?vod=&name=：
//   - .m3u8：读 .jm3u8，把里面的文件名换成本接口的 .ts/.fmp4 地址后输出；
//   - 其余：输出对应文件（支持 Range，BYTERANGE 取分片、拖动都靠它）。
//
// 路径只由 vod/name 经 safeName 拼出，无法跳出下载文件夹。
func hDownloadFile(w http.ResponseWriter, r *http.Request) {
	vod, name := q(r, "vod"), q(r, "name")
	ext := strings.ToLower(filepath.Ext(r.URL.Path))
	base := filepath.Join(sysconf.DownloadDir(), safeName(vod), safeName(name))
	kind, _ := dlFind(vod, name)
	if kind == "" {
		http.NotFound(w, r)
		return
	}
	if ext == ".m3u8" {
		if kind != "hls" {
			http.NotFound(w, r)
			return
		}
		raw, err := os.ReadFile(base + ".jm3u8")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		joint := ".ts"
		if !fileOK(base + ".ts") {
			joint = ".fmp4"
		}
		seg := "/api/dlfile/local" + joint + "?vod=" + url.QueryEscape(vod) + "&name=" + url.QueryEscape(name)
		if t := tokenFrom(r); t != "" {
			seg += "&token=" + url.QueryEscape(t)
		}
		lines := strings.Split(string(raw), "\n")
		for i, ln := range lines {
			t := strings.TrimSpace(ln)
			switch {
			case t == "":
			case strings.HasPrefix(t, "#EXT-X-MAP:"):
				lines[i] = uriAttr.ReplaceAllString(t, `URI="`+seg+`"`)
			case !strings.HasPrefix(t, "#"):
				lines[i] = seg
			}
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(strings.Join(lines, "\n")))
		return
	}
	if (ext == ".ts" || ext == ".fmp4") && kind == "hls" {
		switch ext {
		case ".ts":
			w.Header().Set("Content-Type", "video/mp2t")
		default:
			w.Header().Set("Content-Type", "video/mp4")
		}
		if fileOK(base + ext) {
			http.ServeFile(w, r, base+ext)
			return
		}
		http.NotFound(w, r)
		return
	}
	// 普通视频文件
	if kind == "file" {
		for _, e := range dlDirectExts {
			if e == ext && fileOK(base+e) {
				switch ext {
				case ".flv":
					w.Header().Set("Content-Type", "video/x-flv")
				default:
					w.Header().Set("Content-Type", "video/mp4")
				}
				http.ServeFile(w, r, base+e)
				return
			}
		}
	}
	http.NotFound(w, r)
}

// ---------- 转换为 mp4 ----------

// hDownloadLocalStatus POST /api/download/local-status {vod, names} →
// {kinds:{集名:"file"|"hls"}, ffmpeg:bool}：详情页据此显示「已下载」「转换为 mp4」。
func hDownloadLocalStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Vod   string   `json:"vod"`
		Names []string `json:"names"`
	}
	if err := body(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	kinds := map[string]string{}
	for _, n := range req.Names {
		if k, _ := dlFind(req.Vod, n); k != "" {
			kinds[n] = k
		}
	}
	bin, _ := FFmpegAvailable()
	ok(w, map[string]any{"kinds": kinds, "ffmpeg": bin != ""})
}

// hDownloadConvert POST /api/download/convert {vod, names, keep}：
// 把这些集的 .ts/.fmp4 + .jm3u8 用 ffmpeg 无损转成 mp4；keep=false 时转完删除原始文件。
func hDownloadConvert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Vod   string   `json:"vod"`
		Names []string `json:"names"`
		Keep  bool     `json:"keep"`
	}
	if err := body(r, &req); err != nil || len(req.Names) == 0 {
		fail(w, 400, "参数错误")
		return
	}
	if bin, reason := FFmpegAvailable(); bin == "" {
		fail(w, http.StatusNotImplemented, reason)
		return
	}
	dl.once.Do(func() { go dlWorker() })
	added := 0
	dl.mu.Lock()
	for _, n := range req.Names {
		if k, _ := dlFind(req.Vod, n); k != "hls" {
			continue
		}
		dup := false
		for _, j := range dl.jobs {
			if j.Kind == "convert" && j.Vod == req.Vod && j.Name == n && (j.Status == "queued" || j.Status == "running") {
				dup = true
				break
			}
		}
		if dup || dlActive(req.Vod, n) != nil {
			continue
		}
		dl.seq++
		j := &dlJob{ID: uuid.NewString(), Vod: req.Vod, Name: n, Kind: "convert", Keep: req.Keep,
			Status: "queued", seq: dl.seq, Updated: time.Now().Unix()}
		dl.jobs = append(dl.jobs, j)
		dl.queue <- j
		added++
	}
	dl.mu.Unlock()
	ok(w, map[string]any{"added": added})
}

// runConvert ffmpeg 流复制：拼接文件 → mp4（faststart，带时长索引）。
func runConvert(ctx context.Context, j *dlJob) error {
	bin, reason := FFmpegAvailable()
	if bin == "" {
		return errors.New(reason)
	}
	base := filepath.Join(sysconf.DownloadDir(), safeName(j.Vod), safeName(j.Name))
	src := ""
	for _, e := range []string{".ts", ".fmp4"} {
		if fileOK(base + e) {
			src = base + e
			break
		}
	}
	if src == "" {
		return errors.New("找不到原始下载文件")
	}
	out := base + ".mp4"
	part := out + ".part"
	cmd := exec.CommandContext(ctx, bin, "-hide_banner", "-loglevel", "error", "-y", "-i", src,
		"-map", "0:v?", "-map", "0:a?", "-c", "copy", "-movflags", "+faststart", "-f", "mp4", part)
	cmd.SysProcAttr = sysProcAttrGroup()
	if msg, err := cmd.CombinedOutput(); err != nil {
		os.Remove(part)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("转换失败：%s", firstLine(string(msg)))
	}
	if st, err := os.Stat(part); err != nil || st.Size() == 0 {
		os.Remove(part)
		return errors.New("转换结果为空")
	}
	if err := os.Rename(part, out); err != nil {
		return err
	}
	if !j.Keep {
		os.Remove(src)
		os.Remove(base + ".jm3u8")
	}
	dlUpdate(j, func() { j.File = out })
	return nil
}
