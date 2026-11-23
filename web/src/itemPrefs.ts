// 条目数量偏好（跟 gridPrefs.ts 一样存浏览器 cookie，纯本机显示偏好）：
//   - feedLimit：首页「最近更新」最多显示多少条（0 = 全部，不限制）
//   - vodPerPage：点播页每页显示多少条（0 = 跟随站源自己的分页大小）
import { ref } from 'vue'
import { getCookie, setCookie } from '@/utils/cookie'

function load(key: string, max: number): number {
  const n = parseInt(getCookie(key) || '', 10)
  return Number.isFinite(n) && n > 0 ? Math.min(max, n) : 0
}

const FEED_KEY = 'yz_feed_limit'
const PER_KEY = 'yz_vod_per_page'

export const feedLimit = ref<number>(load(FEED_KEY, 5000))
export const vodPerPage = ref<number>(load(PER_KEY, 200))

export function setFeedLimit(n: number | null | undefined): void {
  const v = n && n > 0 ? Math.min(5000, Math.round(n)) : 0
  feedLimit.value = v
  setCookie(FEED_KEY, String(v))
}

export function setVodPerPage(n: number | null | undefined): void {
  const v = n && n > 0 ? Math.min(200, Math.round(n)) : 0
  vodPerPage.value = v
  setCookie(PER_KEY, String(v))
}

// 首页榜单的排序依据：按所选指标降序取前 N 条（排序下标由后端算好，见 /api/feed 的
// index；不提供该指标的站源的影片不会入选），'orig' = 站点排序（站源给的原始轮转顺序）。
// n 是下拉框里的名字，title 是首页小节标题。默认按更新时间。
export const FEED_SORTS = [
  { k: 'ts', n: '更新时间', title: '最近更新' },
  { k: 'hits', n: '总点击', title: '最热剧(综合)' },
  { k: 'hitsWeek', n: '周点击', title: '最热剧(周)' },
  { k: 'hitsMonth', n: '月点击', title: '最热剧(月)' },
  { k: 'up', n: '点赞数', title: '最爱剧' },
  { k: 'score', n: '站内评分', title: '最优剧' },
  { k: 'douban', n: '豆瓣评分', title: '最优剧(豆瓣)' },
  { k: 'orig', n: '站点排序', title: '站点推荐剧' }
] as const
export type FeedSortKey = (typeof FEED_SORTS)[number]['k']

const SORT_KEY = 'yz_feed_sort'
function loadSort(): FeedSortKey {
  const v = getCookie(SORT_KEY)
  return FEED_SORTS.some((s) => s.k === v) ? (v as FeedSortKey) : 'ts'
}
export const feedSort = ref<FeedSortKey>(loadSort())
export function setFeedSort(k: string): void {
  const v = FEED_SORTS.some((s) => s.k === k) ? (k as FeedSortKey) : 'ts'
  feedSort.value = v
  setCookie(SORT_KEY, v)
}
