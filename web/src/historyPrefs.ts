// 播放历史模式（纯本机偏好，存浏览器 cookie）：
//   none  不记录播放历史（历史页/继续观看为空）
//   local 只存在本浏览器（localStorage），不上传服务端
//   sync  存服务端 store.json，各设备共享（原有行为，默认）
import { ref } from 'vue'
import { getCookie, setCookie } from '@/utils/cookie'

export type HistoryMode = 'none' | 'local' | 'sync'
const KEY = 'yz_history_mode'
const MAX = 40

function loadMode(): HistoryMode {
  const v = getCookie(KEY)
  return v === 'none' || v === 'local' ? v : 'sync'
}
export const historyMode = ref<HistoryMode>(loadMode())
export function setHistoryMode(m: HistoryMode): void {
  historyMode.value = m
  setCookie(KEY, m)
}

type Kind = 'vod' | 'wd'
const LS = { vod: 'yz_hist_vod', wd: 'yz_hist_wd' }
const keyOf = (k: Kind, h: any) => (k === 'vod' ? `${h.siteKey}|${h.vodId}` : `${h.srcId}|${h.vodName}`)

function read(k: Kind): any[] {
  try {
    const a = JSON.parse(localStorage.getItem(LS[k]) || '[]')
    return Array.isArray(a) ? a : []
  } catch {
    return []
  }
}
function write(k: Kind, list: any[]) {
  try {
    localStorage.setItem(LS[k], JSON.stringify(list))
  } catch {
    /* 存储满/被禁用：尽力而为 */
  }
}

export function localHistoryList(k: Kind): any[] {
  return read(k)
}

/** 与服务端 UpdateHistoryPos / UpdateWdHistory 同语义：命中则更新并置顶，否则新增（上限 40）。 */
export function localHistoryUpdate(k: Kind, h: any): void {
  const list = read(k)
  const key = keyOf(k, h)
  const i = list.findIndex((x) => keyOf(k, x) === key)
  let item = { ...h, time: Date.now() }
  if (i >= 0) {
    const old = list[i]
    list.splice(i, 1)
    if (k === 'vod') {
      item = {
        ...old,
        ...h,
        name: h.name || old.name,
        pic: h.pic || old.pic,
        from: h.from || old.from || '',
        siteName: h.siteName || old.siteName,
        time: Date.now()
      }
      const eps: number[] = Array.isArray(old.watchedEps) ? old.watchedEps : []
      if (h.epIndex >= 0 && !eps.includes(h.epIndex)) eps.push(h.epIndex)
      item.watchedEps = eps.slice(-200)
    }
  } else if (k === 'vod' && h.epIndex >= 0) {
    item.watchedEps = [h.epIndex]
  }
  list.unshift(item)
  write(k, list.slice(0, MAX))
}

export function localHistoryRemove(k: Kind, h: any): void {
  const key = keyOf(k, h)
  write(k, read(k).filter((x) => keyOf(k, x) !== key))
}

export function localHistoryClear(k: Kind): void {
  write(k, [])
}
