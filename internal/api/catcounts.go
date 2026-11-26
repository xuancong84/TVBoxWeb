package api

import (
	"context"
	"github.com/xxl6097/TVBoxWeb/internal/sysconf"
	"net/http"
	"sync"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/cms"
	"github.com/xxl6097/TVBoxWeb/internal/model"
	"github.com/xxl6097/TVBoxWeb/internal/store"
)

// 空分类探测：很多采集站的分类列表里有一大半是空的。分类接口本身不带数量，
// 只能逐个分类取第一页看 total。这件事慢（每个分类一次上游请求），所以
// 绝不能挡住用户：首次访问只触发后台探测并立刻返回 ready=false（前端照常
// 显示全部分类），探测完成后结果缓存起来，之后再进来的请求直接命中。

type catProbeEntry struct {
	empty   []string // 确认为空的分类 id
	at      time.Time
	version int64 // 探测时的站源版本，站源变了就作废
}

var (
	catProbeMu      sync.Mutex
	catProbeCache   = map[string]*catProbeEntry{}
	catProbeRunning = map[string]bool{}
)

type catEmptyResp struct {
	Ready bool     `json:"ready"`
	Empty []string `json:"empty"`
}

// hCategoriesEmpty 永远立即返回：有新鲜缓存给结果，否则后台起一轮探测。
func hCategoriesEmpty(w http.ResponseWriter, r *http.Request) {
	siteKey := q(r, "site")
	site := store.Site(siteKey)
	if site == nil {
		fail(w, 400, "站源不存在")
		return
	}
	ver := store.SitesVersion()
	catProbeMu.Lock()
	e := catProbeCache[siteKey]
	fresh := e != nil && e.version == ver && time.Since(e.at) < sysconf.Dur("ca.cat_probe")
	if fresh {
		catProbeMu.Unlock()
		ok(w, catEmptyResp{Ready: true, Empty: e.empty})
		return
	}
	if !catProbeRunning[siteKey] {
		catProbeRunning[siteKey] = true
		go probeEmptyCategories(siteKey, *site, ver)
	}
	catProbeMu.Unlock()
	ok(w, catEmptyResp{Ready: false, Empty: []string{}})
}

func probeEmptyCategories(siteKey string, site model.Site, ver int64) {
	empty := []string{}
	done := false
	defer func() {
		catProbeMu.Lock()
		delete(catProbeRunning, siteKey)
		if done {
			catProbeCache[siteKey] = &catProbeEntry{empty: empty, at: time.Now(), version: ver}
		}
		catProbeMu.Unlock()
	}()

	c := cms.New(site)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cats, err := c.CategoriesCtx(ctx)
	if err != nil {
		return
	}
	var ids []string
	for _, ct := range cats {
		if ct.ID != "" {
			ids = append(ids, ct.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, sysconf.Int("cc.cat_probe"))
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			cctx, ccancel := context.WithTimeout(ctx, sysconf.Dur("to.cms_list"))
			defer ccancel()
			pg, err := c.ListCtx(cctx, id, 1)
			// 出错 = 状态未知，不能当成空分类藏起来
			if err == nil && pg.Total == 0 && len(pg.Items) == 0 {
				mu.Lock()
				empty = append(empty, id)
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
	// 全部都「空」多半是站源整体不可用/接口异常，不如什么都不藏。
	if len(empty) >= len(ids) {
		empty = []string{}
	}
	done = true
}
