// API 客户端：统一封装对 Go 后端的请求。

// auth.ts 内部对 api 的引用是动态 import()，因此这里静态导入不会形成循环依赖。
import { getMediaToken } from './auth'

import type {
  Site,
  SubRef,
  SiteHealthStatus,
  Category,
  Vod,
  VodPage,
  HistItem,
  FavItem,
  FavGroup,
  RawLive,
  LiveDir,
  ChannelGroup,
  Channel,
  LiveScore,
  LineQuality,
  LiveScanStatus,
  EpgResult,
  QLevel,
  Subscription,
  WebdavSource,
  WebdavEntry,
  WebdavVod
} from '@/types'
import { idbGet, idbSet, idbDelete, idbKeys } from '@/idb'
import {
  historyMode,
  localHistoryList,
  localHistoryUpdate,
  localHistoryRemove,
  localHistoryClear
} from '@/historyPrefs'

// 播放历史按模式分流：sync 走服务端；local 走 localStorage；none 什么都不记。
const noopOk = () => Promise.resolve({ ok: true } as any)
function histRoute<T>(kind: 'vod' | 'wd', op: 'list' | 'update' | 'remove' | 'clear', arg: any, remote: () => Promise<T>): Promise<T> {
  const m = historyMode.value
  if (m === 'sync') return remote()
  if (m === 'none') return op === 'list' ? (Promise.resolve([]) as any) : noopOk()
  if (op === 'list') return Promise.resolve(localHistoryList(kind)) as any
  if (op === 'update') localHistoryUpdate(kind, arg)
  else if (op === 'remove') localHistoryRemove(kind, arg)
  else localHistoryClear(kind)
  return noopOk()
}

const BASE = '/api'

// 封面图代理：服务端会把原图缓存到内存文件系统（/dev/shm），同一张图
// 第二次访问直接秒出，不用再找原始站源重新下载。所有 <img :src> 渲染
// vod.pic / h.pic 之类字段时都应该过这一层，而不是直接拿原始地址。
export function imgUrl(url?: string | null): string {
  if (!url) return ''
  if (url.startsWith(`${BASE}/img?`)) return url // 已经是代理过的地址，避免重复包一层
  return `${BASE}/img?url=${encodeURIComponent(url)}`
}

// ===== 401 统一处理 =====
// api 层不直接依赖路由，由 main.ts 注入「跳登录页」的动作，避免循环依赖。
let unauthorizedHandler: (() => void) | null = null

export function setUnauthorizedHandler(fn: () => void): void {
  unauthorizedHandler = fn
}

// ===== 可选访问口令（服务端 -token 开启时才需要）=====
const AUTH_KEY = 'yz_auth_token'

export function getAuthToken(): string {
  try { return localStorage.getItem(AUTH_KEY) || '' } catch { return '' }
}

export function setAuthToken(token: string): void {
  try {
    if (token) localStorage.setItem(AUTH_KEY, token)
    else localStorage.removeItem(AUTH_KEY)
  } catch { /* 隐私模式下 localStorage 不可用，忽略 */ }
}

// ===== 写后失效 + 并发去重 =====
// 任何写操作成功后清掉 GET 缓存（IndexedDB / Service Worker / 内存），
// 避免「收藏后返回详情页仍显示旧数据」。
const inflight = new Map<string, Promise<unknown>>()

export function invalidateApiCache(): void {
  inflight.clear()
  idbKeys()
    .then((keys) => keys.forEach((k) => { if (k.startsWith('api:')) idbDelete(k) }))
    .catch(() => {})
  if (typeof caches !== 'undefined') {
    caches.keys().then((names) => Promise.all(names.map(async (name) => {
      const c = await caches.open(name)
      const reqs = await c.keys()
      await Promise.all(reqs
        .filter((r) => { try { return new URL(r.url).pathname.startsWith('/api/') } catch { return false } })
        .map((r) => c.delete(r)))
    }))).catch(() => {})
  }
}

// 相同 GET 在 5s 内复用同一个 Promise：跨页面来回切换不再重复打后端。
function cachedGet<T>(key: string, run: () => Promise<T>): Promise<T> {
  const hit = inflight.get(key)
  if (hit) return hit as Promise<T>
  const p = run()
  inflight.set(key, p as Promise<unknown>)
  const done = () => { if (inflight.get(key) === p) inflight.delete(key) }
  p.then(done, done)
  return p
}

function authHeaders(): Record<string, string> {
  const t = getAuthToken()
  return t ? { 'X-Auth-Token': t } : {}
}

// 可离线缓存的 GET 路径（历史/收藏/分类/站源等只读数据）
// 视频详情、搜索等也缓存，但 TTL 较短
//
// 故意不含 /live/channels：这是个 stale-while-revalidate 缓存——命中缓存
// 时先把（可能过期的）旧值立刻返回，后台另起一次请求把 IndexedDB 里的值
// 刷新成最新的，但那次刷新的结果只会便宜到"下一次"调用，不会推给这次已经
// 拿到旧值的调用方。频道目录里每个频道的备用线路是按可靠度分数排好序
// 发下来的（见服务端 annotateLineScores），分数会随后台扫描/播放反馈持续
// 变化——命中这层缓存会让用户看到的线路顺序定格在最近一次真正打到网络
// 的那一刻，之后同一个频道反复进出（尤其是 5 分钟内比较频繁地切来切去）
// 可能永远刷不到最新排序，表现成"明明后台一直在学哪条线好使，这边却
// 死活還是先试那几条老掉线"。这个接口本来就有服务端 15s 的 HTTP 缓存头
// （Cache-Control: max-age=15）兜底重复请求，不需要再叠一层 5 分钟的
// IndexedDB 缓存——/live/playable（"可播频道"默认视图用的那个接口）出于
// 同样的理由也没放进这个列表。
const CACHEABLE_PREFIXES = [
  '/status',
  '/sites',
  '/categories',
  '/history',
  '/favorites',
  '/keywords',
  '/feed',
  '/live/url',
  '/live/sources',
  '/prefs',
  '/privacy',
  '/webdav/sources',
  '/webdav/vods',
  '/direct'
]

// 判断路径是否可缓存
function isCacheable(path: string): boolean {
  const clean = path.split('?')[0]
  return CACHEABLE_PREFIXES.some((p) => clean === p || clean.startsWith(p + '/'))
}

// 判断请求是否为 GET（只有 GET 可缓存）
function isGet(opts: RequestInit): boolean {
  return !opts.method || opts.method.toUpperCase() === 'GET'
}

// 错误码 → 可操作的中文建议（与后端 api.go 的 ErrXxx 常量一一对应）
const ERROR_HINTS: Record<string, string> = {
  ERR_TIMEOUT: '站源或网络响应超时，建议切换站源 / 线路后重试',
  ERR_UPSTREAM: '上游服务暂时不可用，稍后再试或换一个站源',
  ERR_SITE: '该站源可能已失效，可到「设置 → 站源测速」查看并换源',
  ERR_AUTH: '访问被拒绝，检查网盘账号密码或订阅链接鉴权',
  ERR_NETWORK: '检查本机网络，或确认服务端是否在运行',
  ERR_BAD_REQUEST: '请求参数有误，刷新页面后重试',
  ERR_NOT_FOUND: '请求的内容不存在，可能已被站源下线',
  ERR_INTERNAL: '服务端处理出错，可查看日志或稍后重试'
}

/** 带结构化错误码的请求异常。 */
export class ApiError extends Error {
  code: string
  status: number
  hint: string
  constructor(message: string, code = 'ERR_INTERNAL', status = 0, hint = '') {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.status = status
    this.hint = hint
  }
}

/** 把任意异常转成用户能看懂的一句话（供 showTip / 错误面板使用）。 */
export function humanizeApiError(e: unknown): string {
  if (e instanceof ApiError) {
    return e.hint ? `${e.message} · ${e.hint}` : e.message
  }
  const msg = e instanceof Error ? e.message : String(e || '')
  if (/failed to fetch|networkerror|load failed|network request failed/i.test(msg)) {
    return `网络连接失败 · ${ERROR_HINTS.ERR_NETWORK}`
  }
  return msg || '发生未知错误'
}

async function request<T>(path: string, opts: RequestInit = {}): Promise<T> {
  // 带 signal 的请求必须独占（可取消），其余 GET 做并发去重
  if (isGet(opts) && !opts.signal) {
    return cachedGet('GET ' + path, () => withRetry(() => doRequest<T>(path, opts)))
  }
  return doRequest<T>(path, opts)
}

// 幂等 GET 遇到网络抖动 / 5xx 自动重试一次（400ms 退避），4xx 不重试。
async function withRetry<T>(run: () => Promise<T>): Promise<T> {
  try {
    return await run()
  } catch (e) {
    if ((e as Error)?.name === 'AbortError') throw e
    const status = (e as { status?: number })?.status
    if (status === 0 || (status !== undefined && status >= 500)) {
      await new Promise((r) => setTimeout(r, 400))
      return run()
    }
    throw e
  }
}

async function doRequest<T>(path: string, opts: RequestInit = {}): Promise<T> {
  const cacheable = isGet(opts) && isCacheable(path)

  // 命中 IndexedDB 缓存则秒回（离线第二层兜底）
  if (cacheable) {
    const cacheKey = 'api:' + path
    const cached = await idbGet<T>(cacheKey)
    if (cached) {
      // 后台刷新缓存（不阻塞返回）
      refreshCache(path, cacheKey)
      return cached
    }
  }

  let res: Response
  try {
    res = await fetch(BASE + path, {
      ...opts,
      credentials: 'same-origin',
      headers: {
        'Content-Type': 'application/json',
        ...authHeaders(),
        ...(opts.headers as Record<string, string> | undefined || {})
      }
    })
  } catch (e) {
    // 主动取消不能被包装成「网络失败」，否则调用方无法区分竞态取消
    if ((e as Error)?.name === 'AbortError') throw e
    throw new ApiError('网络连接失败', 'ERR_NETWORK', 0, ERROR_HINTS.ERR_NETWORK)
  }
  if (!res.ok) {
    // 会话过期/未登录：交给上层跳登录页
    if (res.status === 401) unauthorizedHandler?.()
    let msg = `请求失败 (${res.status})`
    let code = ''
    try {
      // 检查 Content-Type，避免 HTML 错误页面解析失败导致错误信息丢失
      const ct = res.headers.get('Content-Type') || ''
      if (ct.includes('application/json')) {
        const j = await res.json()
        if (j && j.error) msg = j.error
        if (j && j.code) code = j.code
      } else {
        const text = await res.text()
        // 截取前 200 字符作为提示，避免显示完整 HTML
        if (text && !text.trim().startsWith('<')) {
          msg = text.slice(0, 200)
        }
      }
    } catch {
      /* ignore */
    }
    // 兜底：老版本后端 / 纯文本错误页没有 code 时按状态码推断
    if (!code) {
      code = res.status === 504 || res.status === 408 ? 'ERR_TIMEOUT'
        : res.status >= 500 ? 'ERR_UPSTREAM'
        : res.status === 404 ? 'ERR_NOT_FOUND'
        : 'ERR_BAD_REQUEST'
    }
    throw new ApiError(msg, code, res.status, ERROR_HINTS[code] || '')
  }

  const data = (await res.json()) as T

  // 写操作成功后立即失效所有只读缓存，保证跨页面数据一致
  if (!isGet(opts)) invalidateApiCache()

  // 缓存成功响应（IndexedDB）
  if (cacheable) {
    const cacheKey = 'api:' + path
    idbSet(cacheKey, data, 5 * 60 * 1000) // TTL 5 分钟
  }

  return data
}

// 后台刷新缓存（stale-while-revalidate）
async function refreshCache(path: string, cacheKey: string): Promise<void> {
  try {
    const res = await fetch(BASE + path, {
      headers: { 'Content-Type': 'application/json', ...authHeaders() }
    })
    if (res.ok) {
      const data = await res.json()
      idbSet(cacheKey, data, 5 * 60 * 1000)
    }
  } catch {
    /* 后台刷新失败忽略 */
  }
}

const get = <T>(path: string, signal?: AbortSignal) => request<T>(path, signal ? { signal } : {})
const post = <T>(path: string, body?: unknown, signal?: AbortSignal) =>
  request<T>(path, { method: 'POST', body: body === undefined ? undefined : JSON.stringify(body), ...(signal ? { signal } : {}) })
const del = <T>(path: string) => request<T>(path, { method: 'DELETE' })

export const api = {
  get,
  post,
  del,

  status: () => get<any>('/status'),
  metrics: () => get<any>('/metrics'),
  sites: () => get<Site[]>('/sites'),
  subRefs: () => get<SubRef[]>('/sites/subrefs'),
  setEnabled: (keys: string[]) => post('/sites/enabled', { keys }),
  probeSite: (key: string) => post('/sites/probe', { key }),
  sitesSpeed: () => get<{ key: string; name: string; ms: number; ok: boolean }[]>('/sites/speed'),
  sitesHealth: () =>
    get<{ statuses: SiteHealthStatus[]; time: number }>('/sites/health').catch(() => ({ statuses: [], time: 0 })),
  loadConfig: (source: string) => post<{ count: number }>('/config', { source }),
  // 添加（或按地址/文本原地更新）一条订阅；可以多次调用来同时订阅多个来源，
  // 不会互相替换，站源/直播源按去重规则合并。
  subscribe: (source: string) => post<{ name: string; sites: number; lives: number }>('/subscription', { source }),
  subscriptions: () => get<Subscription[]>('/subscriptions'),
  removeSubscription: (id: string) => post<{ name: string; sites: number; lives: number }>('/subscription/remove', { id }),
  toggleSubscription: (id: string, enabled: boolean) =>
    post<{ name: string; sites: number; lives: number }>('/subscription/toggle', { id, enabled }),
  // 批量启停（设置页「单仓源」的全选/全部反选）：一次请求、后端只重建一遍
  // 合并站源列表，不要挨个调 toggleSubscription 并发几十次。
  bulkToggleSubscriptions: (updates: Record<string, boolean>) =>
    post<{ name: string; sites: number; lives: number }>('/subscriptions/bulk-enabled', { updates }),
  // 「订阅」文本框上次保存的原始根地址（一行一个，可能含聚合清单地址）
  subscriptionRoots: () => get<string[]>('/subscriptions/roots'),
  // 把文本框当前的根地址列表递归展开成叶子（单链接）订阅并同步：新出现的
  // 新增，不再出现的删除，还在的只刷新内容、保留原有启停状态。
  syncSubscriptionRoots: (roots: string[]) =>
    post<{ name: string; sites: number; lives: number; leaves: number; added: number; removed: number; errs?: Record<string, string> }>(
      '/subscriptions/sync',
      { roots }
    ),

  categories: (site: string) => get<Category[]>(`/categories?site=${encodeURIComponent(site)}`),
  // 空分类探测：后端永远立即返回；ready=false 表示后台还在探测（路径刻意不放在 /categories 下，避免被 IndexedDB 缓存住 ready=false）
  categoriesEmpty: (site: string) => get<{ ready: boolean; empty: string[] }>(`/catempty?site=${encodeURIComponent(site)}`),
  vodList: (site: string, tid: string, page: number, ext = '', children = '') =>
    get<VodPage>(
      `/vod?site=${encodeURIComponent(site)}&tid=${encodeURIComponent(tid)}&page=${page}` +
        (ext ? `&ext=${encodeURIComponent(ext)}` : '') +
        (children ? `&children=${encodeURIComponent(children)}` : '')
    ),
  vodDetail: (site: string, id: string) =>
    get<Vod>(`/vod/detail?site=${encodeURIComponent(site)}&id=${encodeURIComponent(id)}`),
  // 点播 tab「筛选片名」：站内搜索（走该站自己的搜索接口），不是只在已
  // 加载的这一页本地过滤——站源片单动辄几万条，本地过滤基本搜不到。
  vodSiteSearch: (site: string, kw: string, signal?: AbortSignal) =>
    get<Vod[]>(`/vod/search?site=${encodeURIComponent(site)}&kw=${encodeURIComponent(kw)}`, signal),
  search: (kw: string, signal?: AbortSignal) => get<{ list: Vod[]; sites: number; total: number }>(`/search?kw=${encodeURIComponent(kw)}`, signal),
  // 搜索改成边搜边出：start 立即拿到 session id（不等任何站源返回），
  // poll 取目前为止的结果快照 + 进度，直到 finished=true。
  searchStart: (kw: string) => post<{ id: string; total: number }>('/search/start', { kw }),
  searchPoll: (id: string, signal?: AbortSignal) =>
    get<{ list: Vod[]; total: number; done: number; sites: number; finished: boolean }>(
      `/search/poll?id=${encodeURIComponent(id)}`,
      signal
    ),
  suggest: (kw: string, signal?: AbortSignal) => get<string[]>(`/suggest?kw=${encodeURIComponent(kw)}`, signal),
  resolve: (url: string) => post<{ url: string; header: Record<string, string>; via: string; ok: boolean }>('/resolve', { url }),

  history: () => histRoute('vod', 'list', null, () => get<HistItem[]>('/history')),
  updateHistoryPos: (h: HistItem) => histRoute('vod', 'update', h, () => post('/history/pos', h)),
  removeHistory: (h: HistItem) => histRoute('vod', 'remove', h, () => post('/history/remove', h)),
  clearHistory: () => histRoute('vod', 'clear', null, () => post('/history/clear')),

  favorites: () => get<FavItem[]>('/favorites'),
  toggleFavorite: (f: FavItem) => post<{ added: boolean }>('/favorites/toggle', f),
  // 收藏分组
  favGroups: () => get<{ groups: FavGroup[]; defaultCount: number; total: number }>('/favorites/groups'),
  createFavGroup: (name: string) => post<FavGroup>('/favorites/group/create', { name }),
  updateFavGroup: (id: string, name: string) => post<{ ok: boolean }>('/favorites/group/update', { id, name }),
  deleteFavGroup: (id: string) => post<{ ok: boolean }>('/favorites/group/delete', { id }),
  moveFav: (siteKey: string, vodId: string, groupId: string) =>
    post<{ ok: boolean }>('/favorites/move', { siteKey, vodId, groupId }),
  batchRemoveFav: (items: FavItem[]) => post<{ removed: number }>('/favorites/batch/remove', { items }),
  batchMoveFav: (items: FavItem[], groupId: string) =>
    post<{ moved: number }>('/favorites/batch/move', { items, groupId }),
  importFav: (items: FavItem[]) => post<{ added: number; duplicate: number }>('/favorites/import', { items }),

  keywords: () => get<string[]>('/keywords'),
  pushKeyword: (kw: string) => post<string[]>('/keywords', { kw }),
  removeKeyword: (kw: string) => post<string[]>('/keywords/remove', { kw }),
  clearKeywords: () => del('/keywords'),
  hotKeywords: () => get<string[]>('/keywords/hot'),

  liveUrl: () => get<{ url: string }>('/live/url'),
  setLiveUrl: (url: string) => post('/live/url', { url }),
  liveSources: () => get<RawLive[]>('/live/sources'),
  setLiveSources: (lives: RawLive[]) => post('/live/sources', { lives }),
  setLiveSourcesEnabled: (urls: string[]) => post('/live/sources/enabled', { urls }),
  // 上报一次直播频道「真实播放」的结果，服务端持久化——能播永久有效，
  // 播不通 24 小时后过期重测，用于直播页频道列表按「能播/播不通/没测过」
  // 上色，也是「可播频道」分类的数据来源之一（另一来源是后台扫描）。
  setLiveChannelStatus: (name: string, ok: boolean) => post('/live/channel-status', { name, ok }),
  // 把一个频道从「可播频道」分类里移除：清掉它的测试记录（变回「没测过」），
  // 不是标记成播不通——下一轮后台扫描会重新测一次。
  removeLiveChannelStatus: (name: string) => post('/live/channel-status/remove', { name }),
  // 「可播频道」分类的后台扫描进度：total 当前已知全部频道数，scanned 其中
  // 已有测试结果（且没过期）的数量，playable 其中确认能播的数量，scanning
  // 表示后台扫描这会儿是不是正在跑。
  liveScanStatus: () => get<LiveScanStatus>('/live/scan/status'),
  // 「可播频道」分类专用的轻量频道列表：只有确认能播的那几千条，不像
  // liveChannels() 那样要把全部直播源合并后的十几万条频道目录整个下发——
  // 可播频道视图只需要这一小撮数据，没必要为了它拉全量目录。
  livePlayable: () => get<{ channels: Channel[] }>('/live/playable'),
  // 不传 url：返回所有已启用直播源合并后的统一频道目录（同一频道的全部线路
  // 都在 Channel.urls 里）；force 跳过服务端缓存强制重新拉取全部源。
  liveChannels: (opts?: { url?: string; force?: boolean }) => {
    const params: string[] = []
    if (opts?.url) params.push(`url=${encodeURIComponent(opts.url)}`)
    if (opts?.force) params.push('refresh=1')
    return get<{ root: LiveDir; groups?: ChannelGroup[]; sources?: number; errs?: Record<string, string> }>(
      `/live/channels${params.length ? '?' + params.join('&') : ''}`
    )
  },
  liveFavs: () => get<Channel[]>('/live/favs'),
  toggleLiveFav: (ch: Channel) => post<{ added: boolean }>('/live/fav/toggle', ch),
  liveRankOne: (name: string, url: string) => post<LiveScore>('/live/rank/one', { name, url }),
  liveRank: (items: LiveScore[]) => post<LiveScore[]>('/live/rank', { items }),
  liveProbeChannels: (channels: Channel[]) => post<{ dead: number; total: number }>('/live/probe/channels', { channels }),
  // 轻量批量测一批播放线路的延迟/粗略吞吐（只取开头 16KB），用于判断要不要
  // 自动切换到更好的线路；最多一次测 16 条。
  liveLinesProbe: (urls: string[]) => post<LineQuality[]>('/live/lines/probe', { urls }),
  liveEpg: (name: string, url?: string) =>
    get<EpgResult>(`/live/epg?name=${encodeURIComponent(name)}` + (url ? `&url=${encodeURIComponent(url)}` : '')),
  peek: (url: string) => get<{ alive: boolean }>(`/peek?url=${encodeURIComponent(url)}`),

  crawlSummary: () => get<{ summary: string; last: number; running: boolean; processed: number; total: number }>('/crawl'),
  // 全站抓取现在是后台按 24 小时间隔预先跑的（见 crawler.CrawlInterval），
  // 这个调用只是「问一下要不要补跑一轮」：没到间隔、或者已经在跑，
  // started 都是 false，不会真的现抓现等。
  triggerCrawl: () => post<{ started: boolean; running?: boolean }>('/crawl', {}),
  // index：各排行指标下、每个站源自己降序排好的下标（站源间不混排），指向 feed 后面接上 pool 的拼接列表（不提供该指标的站源不出现）
  feed: () => get<{ feed: Vod[]; pool?: Vod[]; index?: Record<string, { s: string; i: number[] }[]>; deadKeys: string[]; summary: string }>('/feed'),
  qualityMaster: (url: string) => post<QLevel[]>('/quality/master', { url }),

  // skipA/skipB 用于按剧读写片头片尾设置（点播 site+vodId，网盘 srcId+vodName）。
  // 不传则读写全局默认值。
  prefs: (skipA?: string, skipB?: string) =>
    get<{
      qPref: number; speedPref: number; volumePref: number; epgUrl: string
      /** 全局默认值（没单独设置过的剧用它） */
      introSkip: number; outroSkip: number
      /** 当前这部剧实际生效的值 */
      curIntroSkip: number; curOutroSkip: number
      /** 当前剧是否有单独设置 */
      skipOwn: boolean
      skipScoped: boolean
      skipCfgCount: number
      tapSound: number; tapVibrate: number
      liveJunkFilter: boolean
      liveScanEnabled: boolean
    }>(
      '/prefs' +
        (skipA && skipB
          ? `?skipA=${encodeURIComponent(skipA)}&skipB=${encodeURIComponent(skipB)}`
          : '')
    ),
  setPrefs: (p: Record<string, number | string | boolean>, opts?: { skipA: string; skipB: string }) => {
    // 带 skipA/skipB 时表示「只改这部剧的片头片尾」，不带则改全局默认值
    const suffix =
      opts && opts.skipA && opts.skipB
        ? `?skipA=${encodeURIComponent(opts.skipA)}&skipB=${encodeURIComponent(opts.skipB)}`
        : ''
    return post(`/prefs${suffix}`, p)
  },
  // 清掉某部剧的片头片尾单独设置（回落到全局默认）
  clearSkipCfg: (a: string, b: string) =>
    del(`/prefs/skip?skipA=${encodeURIComponent(a)}&skipB=${encodeURIComponent(b)}`),

  webdavSources: () => get<WebdavSource[]>('/webdav/sources'),
  // 用源 id 换播放所需的 Basic Auth：列表接口的密码是掩码（******），
  // 直接拿来拼认证会 401。这个接口在服务端用真实密码生成。
  webdavAuth: (id: string) => get<{ auth: string }>(`/webdav/auth?id=${encodeURIComponent(id)}`),
  saveWebdavSource: (src: WebdavSource) => post('/webdav/sources', src),
  removeWebdavSource: (id: string) => del(`/webdav/sources?id=${encodeURIComponent(id)}`),
  webdavProbe: (src: WebdavSource) => post<{ count: number }>('/webdav/probe', src),
  webdavList: (src: WebdavSource, path: string) => post<WebdavEntry[]>('/webdav/list', { src, path }),
  webdavScan: (src: WebdavSource, maxDepth: number) => post<WebdavVod[]>('/webdav/scan', { src, maxDepth }),
  webdavVods: (srcId: string) => get<WebdavVod[]>(`/webdav/vods?srcId=${encodeURIComponent(srcId)}`),
  webdavHistory: () => histRoute('wd', 'list', null, () => get<any[]>('/webdav/history')),
  updateWdHistory: (h: any) => histRoute('wd', 'update', h, () => post('/webdav/history/update', h)),
  removeWdHistory: (h: any) => histRoute('wd', 'remove', h, () => post('/webdav/history/remove', h)),
  clearWebdavHistory: () => histRoute('wd', 'clear', null, () => post('/webdav/history/clear')),

  directList: () => get<string[]>('/direct'),
  pushDirect: (url: string) => post('/direct', { url }),
  privacy: () => get<{ agreed: boolean; guideDone: boolean }>('/privacy'),
  setPrivacy: (agree?: boolean, guide?: boolean) => post('/privacy', { agree, guide }),

  backup: () => get<Record<string, unknown>>('/backup'),
  restore: (data: unknown) => post('/restore', data),
  reset: () => post('/reset'),

  pinyinAbbr: (word: string) => get<{ abbrs: string[] }>(`/pinyin/abbr?word=${encodeURIComponent(word)}`)
}

// 视频地址走代理（绕开跨域，m3u8 分片也由代理重写）。
/**
 * 把一个容器交给服务端转封装（MKV → fMP4）。
 * 浏览器不支持 Matroska，但里面的 H.264/AAC 是能解的，转封装即可无损播放。
 * @param seek 从第几秒开始（拖进度条时用；转封装流不支持 Range）
 */
export function remuxed(rawUrl: string, auth?: string, seek = 0): string {
  let u = `/api/remux?url=${encodeURIComponent(rawUrl)}`
  if (auth) u += `&auth=${encodeURIComponent(auth)}`
  if (seek > 0) u += `&seek=${Math.floor(seek)}`
  return withToken(u)
}

/**
 * 媒体请求用的 URL 令牌。
 *
 * 优先级：登录派生的媒体令牌 > 旧式访问口令。
 * 为什么必须走 URL：<video>/MSE 发起的子资源请求带不了自定义头，
 * 也不会带 Cookie（没 crossorigin 时不是 CORS 凭证请求）。账号密码登录
 * 启用后只拼旧口令会导致视频 401 —— 这正是「登录后网盘视频播不了」的原因。
 */
function mediaUrlToken(): string {
  return getMediaToken() || getAuthToken()
}

function withToken(u: string): string {
  const t = mediaUrlToken()
  return t ? `${u}&token=${encodeURIComponent(t)}` : u
}

export function proxied(rawUrl: string, auth?: string): string {
  const u = `/api/proxy?url=${encodeURIComponent(rawUrl)}`
  const withAuth = auth ? `${u}&auth=${encodeURIComponent(auth)}` : u
  // 视频分片由浏览器直接加载，无法带自定义头，令牌走 query（后端会透传到 m3u8 内部地址）
  return withToken(withAuth)
}
