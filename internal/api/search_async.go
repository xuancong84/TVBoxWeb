// 搜索改成「边搜边出」：POST /api/search/start 立即起一轮多站并发搜索、
// 马上返回一个 session id，不等任何一个站源返回；前端拿着 id 轮询
// GET /api/search/poll，哪个站源先响应就先看到哪个的结果，而不是像老的
// 阻塞式 /api/search 那样，要等（最多 18 个）站源全部返回或超时才能拿到
// 第一条结果。
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/cms"
	"github.com/xxl6097/TVBoxWeb/internal/model"
	"github.com/xxl6097/TVBoxWeb/internal/pinyin"
	"github.com/xxl6097/TVBoxWeb/internal/store"
)

// searchSessionTTL 超过这么久没人来轮询就清掉，避免用户搜一下就切走、
// 会话一直占内存。
const searchSessionTTL = 3 * time.Minute

type searchSession struct {
	mu        sync.Mutex
	list      []model.Vod
	seen      map[string]bool // 去重键：Name|SiteKey
	total     int             // 本次搜索实际请求的站源数
	done      int             // 已经返回（成功或失败）的站源数
	alive     int             // 返回了至少一条结果的站源数
	finished  bool
	touchedAt time.Time
}

func (s *searchSession) snapshot() (list []model.Vod, total, done, alive int, finished bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touchedAt = time.Now()
	list = make([]model.Vod, len(s.list))
	copy(list, s.list)
	return list, s.total, s.done, s.alive, s.finished
}

var (
	searchSessMu sync.Mutex
	searchSess   = map[string]*searchSession{}
)

func newSearchSessionID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sweepSearchSessions 清掉超过 TTL 没人轮询的会话。
func sweepSearchSessions() {
	cutoff := time.Now().Add(-searchSessionTTL)
	searchSessMu.Lock()
	defer searchSessMu.Unlock()
	for id, s := range searchSess {
		s.mu.Lock()
		stale := s.touchedAt.Before(cutoff)
		s.mu.Unlock()
		if stale {
			delete(searchSess, id)
		}
	}
}

// hSearchStart POST {kw}：立即起一轮多站并发搜索并返回 session id。
func hSearchStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kw string `json:"kw"`
	}
	if err := body(r, &req); err != nil || strings.TrimSpace(req.Kw) == "" {
		fail(w, 400, "缺少关键词")
		return
	}
	kw := strings.TrimSpace(req.Kw)

	sites := store.ActiveSites()
	var searchable []model.Site
	for _, s := range sites {
		if s.Searchable {
			searchable = append(searchable, s)
		}
	}
	if len(searchable) > 18 {
		searchable = searchable[:18]
	}

	sess := &searchSession{seen: map[string]bool{}, total: len(searchable), touchedAt: time.Now()}
	// 拼音首字母联想是本地匹配（不用等网络），直接当这次搜索最先出来的一批结果。
	if isAsciiLetters(kw) {
		for _, v := range store.Feed() {
			key := v.Name + "|" + v.SiteKey
			if !sess.seen[key] && pinyin.MatchAbbr(v.Name, kw) {
				sess.seen[key] = true
				sess.list = append(sess.list, v)
			}
		}
	}
	if len(searchable) == 0 {
		sess.finished = true
	}

	id := newSearchSessionID()
	searchSessMu.Lock()
	searchSess[id] = sess
	searchSessMu.Unlock()
	go sweepSearchSessions()

	// 搜索要在一次次轮询请求之间存活，不能绑定发起 /start 这一次 HTTP 请求的
	// ctx——那个 ctx 在这个 handler 返回时就会被取消，后台 goroutine 还没跑完。
	ctx := context.Background()
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for _, s := range searchable {
		wg.Add(1)
		// sem 的获取必须在 goroutine 内部做，不能在这个 for 循环（也就是
		// hSearchStart 这个 handler 自己的 goroutine）里做——放外面的话，
		// 跑满 6 个并发后，第 7 个站源的 sem <- struct{}{} 会同步卡住这个
		// for 循环，等于 handler 要等前 6 个站源里最慢的那个先返回才能继续
		// 往下发起第 7 个，直接把「立即返回、不等任何站源」的设计写没了。
		go func(s model.Site) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c := cms.New(s)
			vods := c.SearchCtx(ctx, kw)
			sess.mu.Lock()
			sess.done++
			if len(vods) > 0 {
				sess.alive++
				for _, v := range vods {
					key := v.Name + "|" + v.SiteKey
					if !sess.seen[key] {
						sess.seen[key] = true
						sess.list = append(sess.list, v)
					}
				}
			}
			sess.mu.Unlock()
		}(s)
	}
	go func() {
		wg.Wait()
		sess.mu.Lock()
		sess.finished = true
		sess.mu.Unlock()
	}()

	ok(w, map[string]any{"id": id, "total": len(searchable)})
}

// hSearchPoll GET ?id=...：取这次搜索目前为止的结果快照 + 进度；
// finished=true 之后结果就不会再变，前端可以停止轮询。
func hSearchPoll(w http.ResponseWriter, r *http.Request) {
	id := q(r, "id")
	searchSessMu.Lock()
	sess := searchSess[id]
	searchSessMu.Unlock()
	if sess == nil {
		fail(w, 404, "搜索会话不存在或已过期，请重新搜索")
		return
	}
	list, total, done, alive, finished := sess.snapshot()
	ok(w, map[string]any{
		"list": list, "total": total, "done": done, "sites": alive, "finished": finished,
	})
}
