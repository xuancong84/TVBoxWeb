// 画中画跨路由续播：PlayerView 卸载时如果正处于系统画中画，不銷毀播放器，
// 而是把 <video> 搬到 App 级的常驻容器里，让 hls.js/mpegts.js 和组件闭包
// 里的播放状态原样继续跑。这里只存「当前这一个」续播中的会话——同一时间
// 只应该有一个画中画会话在后台跑，开始播放新内容（无论点播还是直播）时，
// 新一次挂载会调 stopCurrentGhost() 把旧会话彻底收尾。
export const PIP_HOST_ID = 'pip-video-host'

export interface GhostHandle {
  // restore：是否要把用户带回这个会话的播放页（画中画窗口被用户「返回
  // 标签页」关掉时），而不是单纯收尾。stopCurrentGhost()（新内容开始
  // 播放时）调用时不传——旧会话没理由把用户拉回它自己的页面。
  stop: (opts?: { restore?: boolean }) => void
}

let current: GhostHandle | null = null

export function stopCurrentGhost() {
  if (current) {
    const h = current
    current = null
    h.stop()
  }
}

export function setCurrentGhost(h: GhostHandle) {
  current = h
}

/** 只在 h 仍是当前登记的会话时才清掉，避免误清一个更新的会话。 */
export function clearCurrentGhost(h: GhostHandle) {
  if (current === h) current = null
}
