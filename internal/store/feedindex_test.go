package store

import (
	"reflect"
	"testing"

	"github.com/xxl6097/TVBoxWeb/internal/model"
)

func ranks(rs []SiteRank) map[string][]int {
	m := map[string][]int{}
	for _, r := range rs {
		m[r.Site] = r.Idx
	}
	return m
}

func TestBuildFeedIndex(t *testing.T) {
	feed := []model.Vod{
		{Name: "a", SiteKey: "A", Hits: 5, Time: 10},
		{Name: "b", SiteKey: "B", Time: 30},          // B 不提供点击数
		{Name: "c", SiteKey: "A", Hits: 9, Time: 20}, // A 提供
		{Name: "d", SiteKey: "A", Time: 5},           // 点击为 0 → 不进点击榜
	}
	idx, _ := BuildFeedIndex(feed, nil)
	if got, want := ranks(idx["hits"]), map[string][]int{"A": {2, 0}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hits = %v, want %v", got, want)
	}
	if got, want := ranks(idx["ts"]), map[string][]int{"A": {2, 0, 3}, "B": {1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ts = %v, want %v", got, want)
	}
	if idx["ts"][0].Site != "A" || idx["ts"][1].Site != "B" {
		t.Fatal("站源顺序应为 feed 中首次出现的先后")
	}
	if _, ok := idx["douban"]; ok {
		t.Fatal("没有站源提供的指标不应出现")
	}
}

func TestBuildFeedIndexPool(t *testing.T) {
	feed := []model.Vod{{Name: "a", SiteKey: "A", Hits: 5}}
	extra := []model.Vod{
		{Name: "x", SiteKey: "A", Hits: 1},
		{Name: "a", SiteKey: "B", Hits: 50}, // 不同站源的同名片各自保留，不跨站去重
		{Name: "z", SiteKey: "C"},           // C 不提供点击 → 排除
	}
	idx, pool := BuildFeedIndex(feed, extra)
	if len(pool) != 2 || pool[0].Name != "x" || pool[1].SiteKey != "B" {
		t.Fatalf("pool = %+v", pool)
	}
	if got, want := ranks(idx["hits"]), map[string][]int{"A": {0, 1}, "B": {2}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hits = %v, want %v", got, want)
	}
}
