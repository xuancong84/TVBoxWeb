// 站源信息的共享只读缓存：给视频卡片（VodCard）hover 展示「点播源/单仓源」
// 用。很多张卡片会同时问「这个 siteKey 是哪个站源」，不能一张卡片一次
// 请求——这里用一个模块级单例把 /api/sites 只拉一次，之后全部从内存读。
import { api } from './api'
import type { Site } from './types'

let cache: Map<string, Site> | null = null
let pending: Promise<Map<string, Site>> | null = null

export function siteInfoMap(): Promise<Map<string, Site>> {
  if (cache) return Promise.resolve(cache)
  if (!pending) {
    pending = Promise.all([api.sites(), api.subRefs().catch(() => [])])
      .then(([list, refs]) => {
        for (const s of list) s.subs = (s.subIdx || []).map((i) => refs[i]).filter(Boolean)
        cache = new Map(list.map((s) => [s.key, s]))
        return cache
      })
      .catch(() => {
        pending = null
        return new Map<string, Site>()
      })
  }
  return pending
}

// 订阅/站源启停变了之后调一下，下次 hover 能拿到最新的单仓源信息。
export function invalidateSiteInfo() {
  cache = null
  pending = null
}
