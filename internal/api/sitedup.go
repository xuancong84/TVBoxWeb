package api

import (
	"net/url"
	"sort"
	"strings"

	"github.com/xxl6097/TVBoxWeb/internal/model"
	"github.com/xxl6097/TVBoxWeb/internal/store"
)

// siteDupKey 把「指向同一个 CMS 接口」的站源归到同一个键：订阅里经常把同一个
// 接口（如暴风）换着名字、协议、结尾斜杠、?ac=list、/at/xml 重复收录十几次，
// 它们的影片 id 空间完全一样，可以互为备用。非标准 CMS（js/py/csp、相对路径）
// 返回 ""，不参与合并。
func siteDupKey(s model.Site) string {
	if s.Kind != "" || (s.Type != 0 && s.Type != 1) {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(s.API))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	p := u.Path
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	p = strings.TrimSuffix(strings.TrimSuffix(p, "/"), "/at/xml")
	p = strings.TrimSuffix(p, "/")
	qv := u.Query()
	qv.Del("ac")
	return host + p + "?" + qv.Encode() + "|" + s.Ext
}

// dedupSites 保留每组重复接口里的第一个，其余丢弃（多站搜索用，避免同一个接口被搜十几遍）。
func dedupSites(in []model.Site) []model.Site {
	seen := map[string]bool{}
	out := make([]model.Site, 0, len(in))
	for _, s := range in {
		if k := siteGroupKey(s); k != "" {
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		out = append(out, s)
	}
	return out
}

// markSiteDups 给 /api/sites 的结果标注：每组挑一个代表（启用 > 没标死 > 片量多 >
// 原顺序），代表带 Alts（其余成员 key），其余成员带 DupOf=代表 key。
func markSiteDups(out []model.Site) {
	groups := map[string][]int{}
	for i := range out {
		if !out[i].Usable {
			continue
		}
		if k := siteGroupKey(out[i]); k != "" {
			groups[k] = append(groups[k], i)
		}
	}
	for _, idx := range groups {
		if len(idx) < 2 {
			continue
		}
		sort.SliceStable(idx, func(a, b int) bool {
			x, y := out[idx[a]], out[idx[b]]
			if x.Enabled != y.Enabled {
				return x.Enabled
			}
			if x.Dead != y.Dead {
				return !x.Dead
			}
			return x.Count > y.Count
		})
		rep := idx[0]
		for _, i := range idx[1:] {
			out[rep].Alts = append(out[rep].Alts, out[i].Key)
			out[i].DupOf = out[rep].Key
		}
	}
}

// altSiteKeys 返回与 key 指向同一接口的其他站源 key，供请求失败时换源重试。
func altSiteKeys(key string) []model.Site {
	all := store.Sites()
	var me string
	for _, s := range all {
		if s.Key == key {
			me = siteGroupKey(s)
			break
		}
	}
	if me == "" {
		return nil
	}
	var out []model.Site
	for _, s := range all {
		if s.Key != key && s.Usable && siteGroupKey(s) == me {
			out = append(out, s)
		}
	}
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}
