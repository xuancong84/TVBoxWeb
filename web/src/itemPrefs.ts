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
