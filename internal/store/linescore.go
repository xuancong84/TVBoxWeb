package store

import (
	"sort"
	"time"
)

// LineScore 单条直播线路的「可靠度」整数分：
//
//   - 第一次成功：按这次连接延迟直接定级（越快分越高），不看之前的分数——
//     「刚刚成功过一次」永远是最有参考价值的信号，旧分数（不管多低）没有
//     理由继续压着这次的结果。
//   - 已经是正分（历史上成功过，还没被失败削没）时失败一次：分数减半
//     （整数除法，向下取整），而不是直接打回负数或清零——线路偶尔抖一下
//     很常见，减半能在「还有希望」和「正在变差」之间留出缓冲地带，真要
//     判定成「这条线不行了」需要连续失败好几次（减半到 0 之后）才会翻进
//     负分区间。
//   - 已经是零分或负分（从没成功过，或者已经被失败削没了）时再失败：
//     继续往下扣，分越负代表越多次确认失败。
//   - 从没测过的线路：不在 map 里，调用方按 0 分处理——排在「确认负分」
//     之后、「确认正分」之前，不偏袒也不惩罚一条没有信息的新线路。
//
// 用整数而不是浮点数：这个分数只用来给同一个频道的几条到几十条备用线路
// 排序，整数比较够快够用，没有理由为了不需要的精度去摊浮点开销。
type LineScore struct {
	Score   int   `json:"score"`
	Samples int   `json:"samples"` // 诊断用：累计被记录过多少次结果
	At      int64 `json:"at"`      // 最近一次更新时间（毫秒）
}

const (
	lineScoreMax          = 1000 // 满分：连接延迟趋近 0 的成功
	lineScoreSuccessFloor = 50   // 成功但很慢的连接，分数下限——仍然要排在任何失败线路前面
	lineScoreFailStep     = 50   // 从没成功过（或已经衰减到 0）的线路，每失败一次再扣这么多
	lineScoreMin          = -1000
)

// scoreFromLatencyMs 延迟越低分越高，线性映射后钳到
// [lineScoreSuccessFloor, lineScoreMax]。刻意不用更复杂的曲线——这个分数
// 只用来排序，不需要物理意义上精确。
func scoreFromLatencyMs(latencyMs int64) int {
	s := lineScoreMax - int(latencyMs)
	if s < lineScoreSuccessFloor {
		s = lineScoreSuccessFloor
	}
	if s > lineScoreMax {
		s = lineScoreMax
	}
	return s
}

// lineScores 与 liveCache/channelStatus 共用同一把锁（liveCacheMu，见
// livecache.go）、同一份落盘文件——三者生命周期/落盘时机本来就是一回事，
// 没必要为这一份数据单独起一把锁、一份文件。
var lineScores = map[string]LineScore{}

// RecordLineOutcome 记一次「真测过一条线路」的结果——不管是后台可播性扫描、
// 播放页轻量测速轮询，评分规则都走这一个入口，保证只有一处。立即落盘，
// 给稀疏写入场景用；批量场景见 RecordLineOutcomeBulk。
func RecordLineOutcome(url string, ok bool, latencyMs int64) {
	RecordLineOutcomeBulk(url, ok, latencyMs)
	flushLiveCache()
}

// RecordLineOutcomeBulk 跟 RecordLineOutcome 语义一致，但不立即落盘——
// 调用方（后台扫描一轮测几百上千条线路、或者一次 lines/probe 请求测
// 好几条）批量测完自己调 FlushLiveCache。
func RecordLineOutcomeBulk(url string, ok bool, latencyMs int64) {
	if url == "" {
		return
	}
	liveCacheMu.Lock()
	cur := lineScores[url]
	if ok {
		// 成功：不看旧分数，按这次延迟直接重新定级——"刚成功过一次"永远
		// 是最有参考价值的信号。
		cur.Score = scoreFromLatencyMs(latencyMs)
	} else if cur.Score > 0 {
		cur.Score /= 2
	} else {
		cur.Score -= lineScoreFailStep
		if cur.Score < lineScoreMin {
			cur.Score = lineScoreMin
		}
	}
	cur.Samples++
	cur.At = time.Now().UnixMilli()
	lineScores[url] = cur
	liveCacheDirty = true
	liveCacheMu.Unlock()
}

// GetLineScore 取一条线路当前的可靠度分数；没测过返回 0（中性）。
func GetLineScore(url string) int {
	liveCacheMu.RLock()
	defer liveCacheMu.RUnlock()
	return lineScores[url].Score
}

// SnapshotLineScores 返回当前全部线路分数的一份纯读快照（整数 map，不带锁）。
// 给"一次性要给几十万个频道的线路排序"这种需要反复查很多次分数的场景用——
// 比起让调用方每查一条线路都去抢一次锁，整体快照一份更便宜。
func SnapshotLineScores() map[string]int {
	liveCacheMu.RLock()
	defer liveCacheMu.RUnlock()
	out := make(map[string]int, len(lineScores))
	for k, v := range lineScores {
		out[k] = v.Score
	}
	return out
}

// SortURLsByScore 返回一份按可靠度分数从高到低排好序的新切片，不修改
// urls 本身——调用方传进来的经常是只读共享缓存（比如直播频道候选集的
// 短期缓存，明确要求「只读遍历，不能被修改」），原地排序会意外改到别处
// 也在用的底层数组。分数相同的保持原有相对顺序（stable）——原始顺序通常
// 是源站自己给的优先级/合并顺序，分数打平时没理由打乱这条线索。
func SortURLsByScore(urls []string, scores map[string]int) []string {
	if len(urls) < 2 {
		return urls
	}
	out := make([]string, len(urls))
	copy(out, urls)
	sort.SliceStable(out, func(i, j int) bool {
		return scores[out[i]] > scores[out[j]]
	})
	return out
}
