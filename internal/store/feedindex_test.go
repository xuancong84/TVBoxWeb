package store

import (
	"reflect"
	"testing"

	"github.com/xxl6097/TVBoxWeb/internal/model"
)

func TestBuildFeedIndex(t *testing.T) {
	feed := []model.Vod{
		{Name: "a", SiteKey: "A", Hits: 5, Time: 10},
		{Name: "b", SiteKey: "B", Time: 30},          // B 不提供点击数
		{Name: "c", SiteKey: "A", Hits: 9, Time: 20}, // A 提供
		{Name: "d", SiteKey: "A", Time: 5},           // A 提供点击数，但这条是 0 → 排最后
	}
	idx, _ := BuildFeedIndex(feed, nil)
	if got, want := idx["hits"], []int{2, 0, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hits = %v, want %v", got, want)
	}
	if got, want := idx["ts"], []int{1, 2, 0, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ts = %v, want %v", got, want)
	}
	if _, ok := idx["douban"]; ok {
		t.Fatal("没有站源提供的指标不应出现")
	}
}

func TestBuildFeedIndexPool(t *testing.T) {
	feed := []model.Vod{{Name: "a", SiteKey: "A", Hits: 5}}
	extra := []model.Vod{
		{Name: "x", SiteKey: "A", Hits: 1},  // 没进榜单前 → 仍会进（只有 3 条）
		{Name: "a", SiteKey: "B", Hits: 50}, // 与 feed 里的 a 重名，指标更高 → 取这条
		{Name: "z", SiteKey: "C"},           // C 不提供点击 → 排除
	}
	idx, pool := BuildFeedIndex(feed, extra)
	// 候选按点击降序：extra[1](50) → feed[0](5) → extra[0](1)；重名 a 只留 50 那条
	// pool 保持候选原有顺序：[x, a(B)]；下标接在 feed（1 条）后面
	if len(pool) != 2 || pool[0].Name != "x" || pool[1].SiteKey != "B" {
		t.Fatalf("pool = %+v", pool)
	}
	if got, want := idx["hits"], []int{2, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hits = %v, want %v", got, want)
	}
}
