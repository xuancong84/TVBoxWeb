package sysconf

import (
	"os"
	"path/filepath"
	"strings"
)

// CacheRoot 缓存根目录：设置→系统「缓存目录」，留空为 ./cache（相对运行目录）。
func CacheRoot() string {
	d := strings.TrimSpace(Str("cache.dir"))
	if d == "" {
		d = "cache"
	}
	if abs, err := filepath.Abs(d); err == nil {
		return abs
	}
	return d
}

// CacheDir 返回缓存根目录下的子目录（自动创建）。
func CacheDir(sub string) string {
	d := filepath.Join(CacheRoot(), sub)
	_ = os.MkdirAll(d, 0o755)
	return d
}

// CacheFile 返回 <缓存目录>/<sub>/<name>；新位置不存在而 legacy 旧路径存在时，
// 把旧文件（含 .bak）搬过来，升级后不丢已有缓存。
func CacheFile(sub, name string, legacy ...string) string {
	p := filepath.Join(CacheDir(sub), name)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	for _, old := range legacy {
		if old == "" || old == p {
			continue
		}
		if _, err := os.Stat(old); err != nil {
			continue
		}
		if os.Rename(old, p) == nil {
			if _, e := os.Stat(old + ".bak"); e == nil {
				_ = os.Rename(old+".bak", p+".bak")
			}
			break
		}
	}
	return p
}
