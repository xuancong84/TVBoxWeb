// Package imgcache 把首页/详情页这类「封面图」缓存到内存文件系统
// （优先 /dev/shm，没有就退回系统临时目录），同一张图第二次请求直接读
// 本地文件返回，不用再找原始站源重新下载。缓存本来就该是「丢了大不了
// 重新下」的定位：不落常规磁盘、不用持久化，进程重启/机器重启清空都没关系。
package imgcache

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"github.com/xxl6097/TVBoxWeb/internal/sysconf"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/httpc"
)

const (
	// maxImageBytes 单张图片上限：封面图正常几百 KB，封顶避免个别异常大图
	// 把缓存空间占爆。
	maxImageBytes = 6 << 20
	// fullThreshold/evictTarget：缓存所在文件系统（一般是 /dev/shm，即内存）
	// 用量超过 95% 才开始清，一次性清到 85% 为止，避免每次写入都卡在临界点
	// 反复触发清理。
	fullThreshold = 0.95
	evictTarget   = 0.85
)

var (
	dirOnce sync.Once
	dir     string

	// inflight 给同一个 key 的并发缺失请求串行化：首页一次性加载几十张新
	// 封面图时，同一张图不会被并发发出好几份重复的上游请求。
	inflight sync.Map // key -> *sync.Mutex

	// failed 记录「上一次尝试过但没拉成功」的 key：这张图再被请求时不再
	// 同步阻塞重试（避免每次打开页面都要为一张挂掉的图等满一次超时），
	// 而是后台悄悄再试一次，这次请求立刻返回空图（零延迟）。
	failed sync.Map // key -> struct{}
	// retrying 防止同一个失败中的 key 被好几个并发请求各自触发一次后台重试。
	retrying sync.Map // key -> struct{}
)

// Dir 返回缓存目录，首次调用时决定好之后固定不变。
func Dir() string {
	dirOnce.Do(func() {
		d := sysconf.CacheDir("img")
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			dir = d
			return
		}
		// 缓存目录不可写：退回系统临时目录
		d = filepath.Join(os.TempDir(), "tvboxweb-imgcache")
		if err := os.MkdirAll(d, 0o700); err == nil {
			dir = d
		}
	})
	return dir
}

func keyFor(url string) string {
	h := sha1.Sum([]byte(url))
	return hex.EncodeToString(h[:])
}

// Get 取某张封面图的字节内容 + Content-Type：命中本地缓存直接读文件返回。
// 没有缓存时分两种情况：
//   - 这张图从没试过：跟原来一样同步联网拉一次（用户确实在等这张没见过的图）。
//   - 这张图上次试过但失败了：不再同步阻塞重试——后台悄悄再拉一次，这次
//     调用立刻返回 pending=true（上层应该直接回一张空图，零延迟），
//     拉成功了会落盘，下一次同一个 URL 的请求自然就能命中。
func Get(ctx context.Context, url string) (data []byte, contentType string, pending bool, err error) {
	d := Dir()
	key := keyFor(url)
	path := filepath.Join(d, key)

	if b, err := os.ReadFile(path); err == nil {
		return b, detectContentType(b), false, nil
	}

	if _, knownFailed := failed.Load(key); knownFailed {
		retryInBackground(key, d, path, url)
		return nil, "", true, nil
	}

	muAny, _ := inflight.LoadOrStore(key, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	defer func() {
		mu.Unlock()
		inflight.Delete(key)
	}()
	// double-check：等锁的这段时间，另一个 goroutine 可能已经把它抓回来落盘了
	if b, err := os.ReadFile(path); err == nil {
		return b, detectContentType(b), false, nil
	}

	body, ferr := fetch(ctx, url)
	if ferr != nil {
		failed.Store(key, struct{}{})
		return nil, "", false, ferr
	}
	writeCache(d, path, body)
	return body, detectContentType(body), false, nil
}

// retryInBackground 给已经失败过的 key 在后台再拉一次，不阻塞当前请求。
// 同一个 key 只要有一次重试在跑，后面并发进来的请求就不重复再起一个。
func retryInBackground(key, dir, path, url string) {
	if _, already := retrying.LoadOrStore(key, struct{}{}); already {
		return
	}
	go func() {
		defer retrying.Delete(key)
		body, err := fetch(context.Background(), url)
		if err != nil {
			return // 还是没拉到，failed 标记留着，下次用户再请求时继续重试
		}
		writeCache(dir, path, body)
		failed.Delete(key)
	}()
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, sysconf.Dur("to.img"))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", httpc.WebUA)
	req.Header.Set("Accept", "image/*,*/*;q=0.8")
	res, err := httpc.StreamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接失败：%v", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxImageBytes))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("空响应")
	}
	return body, nil
}

func detectContentType(b []byte) string {
	n := len(b)
	if n > 512 {
		n = 512
	}
	return http.DetectContentType(b[:n])
}

// writeCache 原子落盘（先写临时文件再 rename，避免并发读到半份文件），
// 写完顺带检查一次缓存所在文件系统的用量，满了就清掉最老的一批。
func writeCache(dir, path string, data []byte) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return
	}
	evictIfNeeded(dir)
}

// evictIfNeeded 用量超过 fullThreshold 时，按文件修改时间（也就是写入
// 缓存的时间）从老到新删，直到用量回到 evictTarget 以下——「delete from
// the oldest ones」，不是按访问时间淘汰的 LRU。
func evictIfNeeded(dir string) {
	total, free, ok := diskUsage(dir)
	if !ok || total == 0 {
		return
	}
	used := 1 - float64(free)/float64(total)
	if used < fullThreshold {
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type fileInfo struct {
		path string
		mod  time.Time
		size int64
	}
	files := make([]fileInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{filepath.Join(dir, e.Name()), info.ModTime(), info.Size()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })

	for _, f := range files {
		if used < evictTarget {
			return
		}
		if err := os.Remove(f.path); err != nil {
			continue
		}
		used -= float64(f.size) / float64(total)
	}
}

func diskUsage(dir string) (total, free uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, false
	}
	bsize := uint64(st.Bsize)
	return st.Blocks * bsize, st.Bavail * bsize, true
}
