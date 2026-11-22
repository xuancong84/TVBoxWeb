/**
 * 夜帧影视 Service Worker
 * 
 * 缓存策略：
 * 1. 应用壳（index.html / manifest / 图标）：预缓存 + Network-First
 * 2. 静态资源（/assets/* 带 hash 文件名）：Cache-First（不可变）
 * 3. API GET 请求（/api/*）：Network-First（离线兜底）
 * 4. 视频代理流（/api/proxy/*）：不缓存（流媒体）
 * 5. WebSocket（/ws）：不拦截
 */

const VERSION = 'yzz-v68'

const CACHE_SHELL = `${VERSION}-shell`
const CACHE_STATIC = `${VERSION}-static`
const CACHE_API = `${VERSION}-api`

// 预缓存的应用壳资源
const PRECACHE_URLS = [
  '/',
  '/index.html',
  '/manifest.json',
  '/icons/icon-192.png',
  '/icons/icon-512.png',
  '/icons/icon-maskable-192.png',
  '/icons/favicon-32.png',
  '/icons/apple-touch-icon.png'
]

// install：预缓存应用壳
self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE_SHELL)
      .then((cache) => cache.addAll(PRECACHE_URLS))
      .then(() => self.skipWaiting())
  )
})

// activate：清理旧版本缓存
self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => {
        const valid = [CACHE_SHELL, CACHE_STATIC, CACHE_API]
        return Promise.all(
          keys.filter((k) => !valid.includes(k)).map((k) => caches.delete(k))
        )
      })
      .then(() => self.clients.claim())
  )
})

// message：处理 SKIP_WAITING（手动更新时立即生效）
self.addEventListener('message', (event) => {
  if (event.data && event.data.type === 'SKIP_WAITING') {
    self.skipWaiting()
  }
})

// fetch：按 URL 类型分流处理
self.addEventListener('fetch', (event) => {
  const request = event.request

  // 只处理 GET 请求
  if (request.method !== 'GET') return

  const url = new URL(request.url)

  // 跨域请求不拦截（视频源、图片 CDN 等）
  if (url.origin !== self.location.origin) return

  // WebSocket 升级请求不拦截
  if (request.headers.get('upgrade') === 'websocket') return

  // 视频代理流：不缓存（流媒体，实时性优先）
  if (url.pathname.startsWith('/api/proxy')) return

  // API GET 请求：Network-First（离线兜底）
  //
  // 这里原来是 Stale-While-Revalidate：有缓存就直接返回缓存，网络请求只在
  // 后台悄悄把缓存更新成最新的，供「下一次」请求使用。问题是像首页「最近
  // 更新」(/api/feed) 这类接口，页面只在 mount 时 fetch 一次、拿到结果就
  // 渲染，不会再去感知「缓存后台更新好了」这件事——如果缓存里恰好存的是
  // 一次空列表/过期快照（比如服务重启后、crawl 还没跑完时的那一次响应），
  // 那它会被永远当成「最新结果」直接渲染出来，表现为「返回/刷新回首页后
  // 列表突然空了，怎么等都不会自己恢复」，要等再跳一次别的页面触发新的
  // fetch（这次缓存已经被上一次的后台更新刷新过了）才会恢复正常。
  // 换成 Network-First：在线时永远以网络结果为准（同时更新缓存），只有
  // 请求失败（离线/断网）时才回退到缓存，离线兜底能力不丢，但不会再把
  // 过期快照当成当前结果展示。
  if (url.pathname.startsWith('/api/')) {
    event.respondWith(networkFirstAPI(request, CACHE_API))
    return
  }

  // 静态资源（带 hash 文件名，不可变）：Cache-First
  if (url.pathname.startsWith('/assets/') || url.pathname.startsWith('/icons/')) {
    event.respondWith(cacheFirst(request, CACHE_STATIC))
    return
  }

  // 导航请求（HTML 页面）：Network-First，离线回退缓存
  if (request.mode === 'navigate') {
    event.respondWith(networkFirst(request, CACHE_SHELL))
    return
  }
})

/**
 * Cache-First：先查缓存，命中即返回；未命中则请求并缓存。
 * 适用于带 hash 文件名的不可变静态资源。
 */
async function cacheFirst(request, cacheName) {
  const cache = await caches.open(cacheName)
  const cached = await cache.match(request)
  if (cached) return cached

  try {
    const response = await fetch(request)
    // 只缓存成功响应
    if (response.ok) {
      cache.put(request, response.clone())
    }
    return response
  } catch (err) {
    // 离线且无缓存：返回离线兜底（仅对导航请求有意义）
    return new Response('', { status: 503, statusText: 'offline' })
  }
}

/**
 * Network-First：优先请求网络，失败则回退缓存。
 * 适用于 HTML 导航请求（保证最新内容，离线兜底）。
 */
async function networkFirst(request, cacheName) {
  try {
    const response = await fetch(request)
    const cache = await caches.open(cacheName)
    cache.put(request, response.clone())
    return response
  } catch (err) {
    const cache = await caches.open(cacheName)
    const cached = await cache.match(request)
    if (cached) return cached
    // 离线且无缓存：返回 index.html 兜底
    const fallback = await cache.match('/index.html')
    if (fallback) return fallback
    return new Response('离线状态，请联网后重试', {
      status: 503,
      headers: { 'Content-Type': 'text/plain; charset=utf-8' }
    })
  }
}

/**
 * Network-First：优先请求网络并更新缓存；网络失败（离线/断网）才回退缓存。
 * 适用于 API GET 请求——保证在线时永远拿到当前最新结果，离线时兜底缓存。
 */
async function networkFirstAPI(request, cacheName) {
  const cache = await caches.open(cacheName)
  try {
    const response = await fetch(request)
    if (response.ok) {
      cache.put(request, response.clone())
    }
    return response
  } catch (err) {
    const cached = await cache.match(request)
    if (cached) return cached
    // 完全离线且无缓存
    return new Response(JSON.stringify({ error: 'offline', offline: true }), {
      status: 503,
      headers: { 'Content-Type': 'application/json' }
    })
  }
}
