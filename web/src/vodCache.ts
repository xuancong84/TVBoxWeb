// 点播详情页「免重复请求」缓存：点播/搜索/首页最近更新这些列表页，
// 点进某张卡片之前就已经在内存里拿到过一份 Vod 对象了——如果这份数据
// 已经带着 playUrl（不少站源的片单接口其实也带，见 VodCard.vue 的
// epCount 注释），详情页完全没必要为了同一部片子再打一次网络请求。
//
// 只是「同一个标签页内、这一次会话」的内存缓存，不落盘、不跨标签页
// （中键新开标签页是全新的 JS 环境，天然拿不到，只能老老实实现查一次，
// 这也是预期行为——跨标签页看不到彼此的内存状态）。
import type { Vod } from '@/types'

const cache = new Map<string, Vod>()

function cacheKey(siteKey: string, vodId: string): string {
  return `${siteKey}::${vodId}`
}

// 只缓存「信息齐全」的 Vod：playUrl 非空才说明这就是详情页要的那份完整
// 数据。首页「继续观看」「最近更新」这类从磁盘快照拼出来的卡片，后端
// 为了省存储把 playUrl 砍掉了（见 model.Vod 的注释），没资格进这个缓存，
// 该交给详情页自己按需现查。
export function cacheVod(v: Vod | null | undefined) {
  if (!v || !v.playUrl || !v.siteKey || !v.id) return
  cache.set(cacheKey(v.siteKey, v.id), v)
}

export function getCachedVod(siteKey: string, vodId: string): Vod | undefined {
  return cache.get(cacheKey(siteKey, vodId))
}
