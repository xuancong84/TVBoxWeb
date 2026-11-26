package cms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xxl6097/TVBoxWeb/internal/sysconf"
	"strconv"
	"strings"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/jsspider"
	"github.com/xxl6097/TVBoxWeb/internal/model"
	"github.com/xxl6097/TVBoxWeb/internal/pyspider"
)

// Python 站源（site.Kind == "py"）的适配层：把 worker 返回的 TVBox 风格 JSON
// （class / list / vod_id 为字符串 ...）转成项目内部的 model 结构。

func pyTTLCat() time.Duration    { return sysconf.Dur("ca.py_cat") }
func pyTTLList() time.Duration   { return sysconf.Dur("ca.py_list") }
func pyTTLDetail() time.Duration { return sysconf.Dur("ca.py_detail") }

func (c *Cms) isPy() bool { return c.site.Kind == "py" || c.site.Kind == "js" }

// SpiderCall 按站源类型调用对应的 worker（Python 或 JS）。
func SpiderCall(ctx context.Context, site model.Site, op string, args map[string]any) (json.RawMessage, error) {
	if site.Kind == "js" {
		return jsspider.Call(ctx, site, op, args)
	}
	return pyspider.Call(ctx, site, op, args)
}

// pyCall 带缓存的调用：缓存的是 worker 返回的原始 JSON。
func (c *Cms) pyCall(ctx context.Context, op string, args map[string]any, ttl time.Duration) ([]byte, error) {
	ab, _ := json.Marshal(args)
	key := "py://" + c.site.Key + "/" + op + "?" + string(ab)
	if ttl > 0 {
		if body := hit(key, ttl); body != "" {
			return []byte(body), nil
		}
	}
	cctx, cancel := context.WithTimeout(ctx, sysconf.Dur("to.py"))
	defer cancel()
	raw, err := SpiderCall(cctx, c.site, op, args)
	if err != nil {
		return nil, err
	}
	// 空结果（站源这会儿连不上/被拦时脚本往往吞掉错误返回空列表）不缓存，
	// 否则一次失败会被记住一小时。
	if ttl > 0 && pyHasContent(op, raw) {
		keep(key, string(raw))
	}
	return raw, nil
}

type pyVod struct {
	ID       any    `json:"vod_id"`
	Name     string `json:"vod_name"`
	Pic      string `json:"vod_pic"`
	Remarks  string `json:"vod_remarks"`
	Year     any    `json:"vod_year"`
	Area     string `json:"vod_area"`
	Actor    string `json:"vod_actor"`
	Director string `json:"vod_director"`
	Content  string `json:"vod_content"`
	From     string `json:"vod_play_from"`
	URL      string `json:"vod_play_url"`
	TypeName string `json:"type_name"`
}

func anyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case float64:
		return strconv.FormatInt(int64(x), 10)
	default:
		return fmt.Sprint(x)
	}
}

func (c *Cms) pyVodOf(j pyVod) model.Vod {
	v := model.Vod{
		ID:       anyStr(j.ID),
		Name:     unescapeEntities(s(j.Name)),
		Pic:      s(j.Pic),
		Remark:   unescapeEntities(s(j.Remarks)),
		Year:     anyStr(j.Year),
		Area:     s(j.Area),
		Actor:    s(j.Actor),
		Director: s(j.Director),
		TypeName: s(j.TypeName),
		From:     s(j.From),
		Content:  plain(s(j.Content)),
		SiteKey:  c.site.Key,
		SiteName: c.site.Name,
	}
	v.PlayURL = c.pyWrapPlay(v.From, j.URL)
	return v
}

// pyWrapPlay 把 vod_play_url（线路用 $$$ 分，集用 # 分，名字和 id 用 $ 分）里
// 不是「明显的视频直链」的集 id 换成播放令牌，播放时由 /api/resolve 调用
// 站源的 playerContent 解出真正的地址。
func (c *Cms) pyWrapPlay(from, playURL string) string {
	if strings.TrimSpace(playURL) == "" {
		return ""
	}
	flags := strings.Split(from, "$$$")
	lines := strings.Split(playURL, "$$$")
	for li, line := range lines {
		flag := ""
		if li < len(flags) {
			flag = flags[li]
		}
		eps := strings.Split(line, "#")
		for ei, ep := range eps {
			name, id := "", ep
			if i := strings.Index(ep, "$"); i >= 0 {
				name, id = ep[:i], ep[i+1:]
			}
			id = strings.TrimSpace(id)
			if id == "" || IsDirect(id) {
				continue
			}
			eps[ei] = name + "$" + pyspider.PlayToken(c.site.Key, flag, id)
		}
		lines[li] = strings.Join(eps, "#")
	}
	return strings.Join(lines, "$$$")
}

func (c *Cms) pyCategories(ctx context.Context) ([]model.Category, error) {
	raw, err := c.pyCall(ctx, "home", nil, pyTTLCat())
	if err != nil {
		return nil, err
	}
	var h struct {
		Class []struct {
			ID   any    `json:"type_id"`
			Name string `json:"type_name"`
		} `json:"class"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, err
	}
	var out []model.Category
	for _, cl := range h.Class {
		id, name := anyStr(cl.ID), unescapeEntities(s(cl.Name))
		if id != "" && name != "" {
			out = append(out, model.Category{ID: id, Name: name})
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return prependAll(out), nil
}

func (c *Cms) pyList(ctx context.Context, tid string, page int) (model.VodPage, error) {
	if tid == "" { // 「全部」：Python 站源没有这个概念，取第一个分类
		cats, err := c.pyCategories(ctx)
		if err != nil {
			return model.VodPage{}, err
		}
		for _, k := range cats {
			if k.ID != "" {
				tid = k.ID
				break
			}
		}
		if tid == "" {
			return model.VodPage{}, errors.New("站源没有返回任何分类")
		}
	}
	raw, err := c.pyCall(ctx, "category", map[string]any{"tid": tid, "pg": page}, pyTTLList())
	if err != nil {
		return model.VodPage{}, err
	}
	var r struct {
		Page      any     `json:"page"`
		PageCount any     `json:"pagecount"`
		Total     any     `json:"total"`
		List      []pyVod `json:"list"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return model.VodPage{}, err
	}
	pg := model.VodPage{Page: page, PageCount: 1}
	if n, _ := strconv.Atoi(anyStr(r.PageCount)); n > 0 {
		pg.PageCount = n
	}
	if n, _ := strconv.Atoi(anyStr(r.Total)); n > 0 && n < 900000 {
		pg.Total = n
	}
	for _, it := range r.List {
		v := c.pyVodOf(it)
		if v.ID != "" && v.Name != "" {
			pg.Items = append(pg.Items, v)
		}
	}
	return pg, nil
}

func (c *Cms) pyDetail(ctx context.Context, id string) (model.Vod, error) {
	raw, err := c.pyCall(ctx, "detail", map[string]any{"id": id}, pyTTLDetail())
	if err != nil {
		return model.Vod{}, err
	}
	var r struct {
		List []pyVod `json:"list"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return model.Vod{}, err
	}
	if len(r.List) == 0 {
		return model.Vod{}, errors.New("接口没有返回详情数据")
	}
	v := c.pyVodOf(r.List[0])
	if v.ID == "" {
		v.ID = id
	}
	return v, nil
}

func (c *Cms) pySearch(ctx context.Context, kw string) []model.Vod {
	raw, err := c.pyCall(ctx, "search", map[string]any{"key": kw, "pg": 1}, pyTTLList())
	if err != nil {
		return nil
	}
	var r struct {
		List []pyVod `json:"list"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil
	}
	var out []model.Vod
	for _, it := range r.List {
		v := c.pyVodOf(it)
		if v.ID != "" && v.Name != "" {
			out = append(out, v)
		}
	}
	return out
}

func pyHasContent(op string, raw []byte) bool {
	var r struct {
		Class []json.RawMessage `json:"class"`
		List  []json.RawMessage `json:"list"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return false
	}
	if op == "home" {
		return len(r.Class) > 0
	}
	return len(r.List) > 0
}
