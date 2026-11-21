// 顶栏时钟显示偏好：日期/时间/星期各自独立开关，存浏览器 cookie（纯本机
// 显示偏好，不走服务端 store.json，道理跟 gridPrefs.ts 的每行列数一致）。
import { ref, watch } from 'vue'
import { getCookie, setCookie } from '@/utils/cookie'

const COOKIE_KEY = 'yz_clock_prefs'

interface ClockPrefs {
  date: boolean
  time: boolean
  dow: boolean
}

function loadInitial(): ClockPrefs {
  const raw = getCookie(COOKIE_KEY)
  if (!raw) return { date: true, time: true, dow: true }
  try {
    const p = JSON.parse(raw)
    return {
      date: p.date !== false,
      time: p.time !== false,
      dow: p.dow !== false
    }
  } catch {
    return { date: true, time: true, dow: true }
  }
}

const initial = loadInitial()

/** 顶栏是否显示日期（如 2026-11-21）。 */
export const showClockDate = ref(initial.date)
/** 顶栏是否显示时间（时:分:秒）。 */
export const showClockTime = ref(initial.time)
/** 顶栏是否显示星期（如 周六）。 */
export const showClockDow = ref(initial.dow)

watch([showClockDate, showClockTime, showClockDow], ([date, time, dow]) => {
  setCookie(COOKIE_KEY, JSON.stringify({ date, time, dow }))
})
