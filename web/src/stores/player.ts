import { defineStore } from 'pinia'
import type { Episode, Line } from '@/types'

// 进程内播放列表单例，等价于鸿蒙影视 Playlist.ets：
// 避免用路由传几百集的序列化数据。
interface PlaylistState {
  eps: Episode[]
  idx: number
  vodTitle: string
  isLive: boolean
  directMode: boolean
  header: Record<string, string>
  seek: number
  wdSrcId: string
  wdSrcName: string
  wdVod: string
  wdDir: string
  wdSeries: boolean
  /** 点播：这部剧的全部线路（播放源），用于播放页内切换 */
  lines: Line[]
  lineIdx: number
}

export const usePlayerStore = defineStore('player', {
  state: (): PlaylistState => ({
    eps: [],
    idx: 0,
    vodTitle: '',
    isLive: false,
    directMode: false,
    header: {},
    seek: 0,
    wdSrcId: '',
    wdSrcName: '',
    wdVod: '',
    wdDir: '',
    wdSeries: false,
    lines: [],
    lineIdx: 0
  }),
  getters: {
    count: (s) => s.eps.length,
    current: (s): Episode | null => (s.idx >= 0 && s.idx < s.eps.length ? s.eps[s.idx] : null),
    hasNext: (s) => s.idx + 1 < s.eps.length,
    hasPrev: (s) => s.idx > 0
  },
  actions: {
    set(eps: Episode[], idx: number, vodTitle: string, isLive = false, directMode = false) {
      this.eps = eps.map((e) => ({ name: e.name, url: e.url, urls: [...(e.urls || [])] }))
      this.idx = idx >= 0 && idx < eps.length ? idx : 0
      this.vodTitle = vodTitle
      this.isLive = isLive
      this.directMode = directMode
      this.lines = []
      this.lineIdx = 0
    },
    setLines(lines: Line[], idx: number) {
      this.lines = lines.map((l) => ({ name: l.name, direct: l.direct, eps: l.eps.map((e) => ({ name: e.name, url: e.url, urls: [...(e.urls || [])] })) }))
      this.lineIdx = idx
    },
    /** 换线路：按同一集序号（越界取最后一集）换成目标线路的剧集列表 */
    switchVodLine(i: number): boolean {
      const l = this.lines[i]
      if (!l || !l.eps.length) return false
      this.eps = l.eps.map((e) => ({ name: e.name, url: e.url, urls: [...(e.urls || [])] }))
      this.idx = Math.min(this.idx, this.eps.length - 1)
      this.lineIdx = i
      this.directMode = false
      return true
    },
    step(delta: number): Episode | null {
      const t = this.idx + delta
      if (t < 0 || t >= this.eps.length) return null
      this.idx = t
      return this.eps[t]
    },
    clear() {
      this.eps = []
      this.idx = 0
      this.vodTitle = ''
      this.isLive = false
      this.directMode = false
      this.lines = []
      this.lineIdx = 0
    }
  }
})
