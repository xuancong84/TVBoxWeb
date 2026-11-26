package api

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/cms"
	"github.com/xxl6097/TVBoxWeb/internal/model"
	"github.com/xxl6097/TVBoxWeb/internal/store"
)

// 跨域名合并：URL 写法相同的站源由 siteDupKey 直接归组；域名不同的镜像
//（如 app.bfzyapi.com 与 bfzyapi.com）光看地址无法证明是同一个后端，所以
// 这里在后台取两边「全部」第 1 页，按影片 id 对片名，吻合才并成一组。
// 结果只放内存，重启后 30 秒再验一遍。

var (
	xMu    sync.RWMutex
	xGroup = map[string]string{} // siteDupKey -> 簇代表 dupKey
)

// siteGroupKey 是站源最终的分组键：验证过的镜像共用一个键，否则就是 siteDupKey。
func siteGroupKey(s model.Site) string {
	k := siteDupKey(s)
	if k == "" {
		return ""
	}
	xMu.RLock()
	defer xMu.RUnlock()
	if g, ok := xGroup[k]; ok {
		return g
	}
	return k
}

func normTitle(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), ""))
}

// samePage 比较两个站源第 1 页：以较短的一页为准，id 必须在对方出现、片名一致的比例 >= 80%。
func samePage(a, b []model.Vod) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(a) < 5 {
		return false
	}
	m := make(map[string]string, len(b))
	for _, v := range b {
		m[v.ID] = normTitle(v.Name)
	}
	hit := 0
	for _, v := range a {
		if n, ok := m[v.ID]; ok && n == normTitle(v.Name) && n != "" {
			hit++
		}
	}
	return hit*10 >= len(a)*8
}

func crossVerifyOnce() {
	health := store.SiteHealthMap()
	type cand struct {
		dk   string
		site model.Site
	}
	byCount := map[int][]cand{}
	seen := map[string]bool{}
	for _, s := range store.Sites() {
		dk := siteDupKey(s)
		n := health[s.Key].LastTotal
		if dk == "" || !s.Usable || n <= 0 || seen[dk] {
			continue
		}
		seen[dk] = true
		byCount[n] = append(byCount[n], cand{dk, s})
	}
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if p, ok := parent[x]; ok && p != x {
			r := find(p)
			parent[x] = r
			return r
		}
		parent[x] = x
		return x
	}
	for _, cs := range byCount {
		if len(cs) < 2 || len(cs) > 12 {
			continue
		}
		pages := make([][]model.Vod, len(cs))
		var wg sync.WaitGroup
		for i := range cs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
				defer cancel()
				if pg, err := cms.New(cs[i].site).ListCtx(ctx, "", 1); err == nil {
					pages[i] = pg.Items
				}
			}(i)
		}
		wg.Wait()
		for i := 0; i < len(cs); i++ {
			for j := i + 1; j < len(cs); j++ {
				if pages[i] != nil && pages[j] != nil && samePage(pages[i], pages[j]) {
					parent[find(cs[j].dk)] = find(cs[i].dk)
				}
			}
		}
	}
	next := map[string]string{}
	for k := range parent {
		if r := find(k); r != k {
			next[k] = r
		}
	}
	xMu.Lock()
	xGroup = next
	xMu.Unlock()
}

func crossVerifyLoop() {
	time.Sleep(30 * time.Second)
	for {
		crossVerifyOnce()
		time.Sleep(6 * time.Hour)
	}
}
