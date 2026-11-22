// 网格列数偏好：每行显示几张卡片，存浏览器 cookie（不同浏览器/设备各自
// 独立，不走服务端 store.json——这是纯粹的本机显示偏好，不是账号数据）。
//
// 默认值取「当前浏览器按 CSS auto-fill 规则会自动算出的列数」：VodGrid.vue
// 原来的网格靠 `repeat(auto-fill, minmax(卡片最小宽度, 1fr))` 由浏览器自己
// 填满一行能放下几张卡片，这里用同一条公式（容器可用宽度 = 视口宽度 - 左右
// padding，列数 = floor((可用宽度 + 间距) / (卡片最小宽度 + 间距))）在 JS
// 里重新算一遍，保证「用户还没手动调整过」时跟原来的自动效果完全一致。
import { ref } from 'vue'
import { getCookie, setCookie } from '@/utils/cookie'

const COOKIE_KEY = 'yz_grid_cols'
const MIN_COLS = 1
const MAX_COLS = 20

export function autoCols(): number {
  const mobile = window.innerWidth <= 640
  const minCardW = mobile ? 110 : 160
  const gap = mobile ? 5 : 8
  const pad = mobile ? 6 : 12
  const avail = Math.max(0, window.innerWidth - pad * 2)
  return Math.max(1, Math.floor((avail + gap) / (minCardW + gap)))
}

function clamp(n: number): number {
  return Math.max(MIN_COLS, Math.min(MAX_COLS, Math.round(n)))
}

function loadInitial(): number {
  const saved = getCookie(COOKIE_KEY)
  const n = saved ? parseInt(saved, 10) : NaN
  return Number.isFinite(n) && n > 0 ? clamp(n) : autoCols()
}

/** 当前生效的每行列数；所有网格（VodGrid、收藏网格等）都读这一个全局值。 */
export const gridCols = ref<number>(loadInitial())

/** 用户手动调整列数：立即生效并写入 cookie。 */
export function setGridCols(n: number): void {
  const v = clamp(n)
  gridCols.value = v
  setCookie(COOKIE_KEY, String(v))
}

/** 恢复成「当前窗口尺寸下浏览器会自动算出的列数」。 */
export function resetGridColsToAuto(): void {
  setGridCols(autoCols())
}
