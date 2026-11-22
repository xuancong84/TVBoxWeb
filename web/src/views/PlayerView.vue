<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted, onBeforeUnmount } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, proxied, remuxed, humanizeApiError } from '@/api'
import { useAppStore } from '@/stores/app'
import { usePlayerStore } from '@/stores/player'
import { fmtMs } from '@/utils/format'
import { fromHlsLevels, labelOfLevel, labelOf, AUTO, TOP, QLevel } from '@/utils/quality'
import { usePlayerSubtitles } from '@/composables/usePlayerSubtitles'
import { usePlayerGestures } from '@/composables/usePlayerGestures'
import PlayerSettingsPanel from '@/components/player/PlayerSettingsPanel.vue'
import EpisodeDrawer from '@/components/player/EpisodeDrawer.vue'
import TipToast from '@/components/TipToast.vue'
import { PIP_HOST_ID, stopCurrentGhost, setCurrentGhost, clearCurrentGhost, type GhostHandle } from '@/playerGhost'

// hls.js 动态加载（410KB → 只在进播放页时才请求）
let Hls: any = null
async function loadHls(): Promise<any> {
  if (Hls) return Hls
  const mod = await import('hls.js')
  Hls = mod.default || mod
  return Hls
}

// mpegts.js 动态加载：给直播频道里「不是 m3u8」的那条线路用——这类地址
// 合并/抓取阶段看着没有扩展名（常常是跳转到真实 CDN 地址之前的中间层，
// 比如虎牙转播镜像），但实际落地大多是 FLV 或裸 MPEG-TS 容器，<video>
// 原生不认这两种容器（哪怕里面的 H.264/AAC 浏览器本来能解），之前这类
// 频道一律塞进 hls.js（hls.js 只认 m3u8，拿到的其实是一段二进制 FLV/TS
// 数据，解析 manifest 必然失败），表现为背景探活判它「能播」（真的有
// 数据在传），但播放页永远卡在「缓冲中」——用 mpegts.js 转封装成 fMP4
// 喂给 MSE 才能真正放出来。
let MpegtsJS: any = null
async function loadMpegts(): Promise<any> {
  if (MpegtsJS) return MpegtsJS
  const mod = await import('mpegts.js')
  MpegtsJS = mod.default || mod
  return MpegtsJS
}

const route = useRoute()
const router = useRouter()
const app = useAppStore()
const player = usePlayerStore()

// 开始播放新内容（无论点播还是直播）：按用户预期，这应该结束掉上一个还
// 挂在系统画中画里后台跑的旧会话，而不是让两个播放器一起占带宽/解码资源。
// 必须在 setup 最早期就调用，比任何 await/初始化都早——旧会话占的资源才
// 能尽快释放。
stopCurrentGhost()

const SEEK_STEP = 10000

// 这个播放页自己的完整地址，必须在这里（setup 时）就快照下来，不能等到
// onBeforeUnmount 里再读 route.fullPath——Vue Router 导航时，会先把
// route（全局唯一的响应式对象）更新成新路由，然后才去卸载旧组件/挂载新
// 组件，所以 onBeforeUnmount 跑的时候 route.fullPath 早就是「将要去的
// 那个页面」而不是这个播放页自己了（下面 rawUrl0/siteKey 等同理，都是
// 一次性快照，道理一样）。
const ownFullPath = route.fullPath
const rawUrl0 = (route.query.url as string) || ''
const siteKey = (route.query.site as string) || ''
const vodId = (route.query.id as string) || ''
const vodName = (route.query.name as string) || (route.query.vodTitle as string) || ''
const vodPic = (route.query.pic as string) || ''
const vodSiteName = (route.query.siteName as string) || ''
// 画中画「返回标签页」把人带回播放页时，会在 URL 里加这个标记（见
// onBeforeUnmount 的 restore 分支）：这种情况下「返回」按钮不能再简单地
// router.back() 一步——用户很可能在画中画后台播着的时候去别的好几个页面
// 逛了一圈，浏览器历史里真实的「上一页」是那些页面，不是这部剧原本的
// 详情页/直播列表，所以要改成直接导航去「本该返回的那个页面」（见
// buildParentPath/goBack）。
const cameFromPipRestore = route.query.pipRestore === '1'

// 转封装模式：MKV 等容器由服务端实时转成 fMP4。
// 这种流不支持 Range，所以拖进度条要带 seek 重新起转。
const remuxMode = ref(false)
const remuxSeek = ref(0)
// 无扩展名/判不出容器的文件播失败后，是否已经试过转封装兜底。
// 只试一次：转封装也失败就直接报错，否则会在两个分支之间来回弹。
let remuxFallbackTried = false

let rawUrl = rawUrl0
let finalUrl = ''
let isLive = player.isLive
let directMode = player.directMode || route.query.direct === '1'
let qCap = 0
let levelIdx = -1
// 自动模式下 hls.js 实际正在播放的档位（LEVEL_SWITCHED 回写）。
// 与 levelIdx 分开：levelIdx 表示「用户手动选的那一档」，
// 自动模式下它是 -1，但实际档位需要另外记。
let liveLevelIdx = -1
const levels = ref<QLevel[]>([])

// ===== 直播线路质量监控：轻量测速 + 当前线路卡顿检测，自动切到更好的线路 =====
// 只探测「没在播」的备用线路（≤16KB 的 Range 请求），不碰正在播的那条流，
// 不会跟正片抢带宽；当前线路好不好看它自己的缓冲事件（onwaiting/onplaying），
// 不额外发请求去测它。
const LINE_PROBE_INTERVAL = 25000 // 每 25s 探测一轮备用线路
const STALL_WINDOW_MS = 30000     // 卡顿统计滚动窗口
const STALL_RATIO_BAD = 0.35      // 窗口内卡顿占比超过这个值判定为「变差」
const AUTO_SWITCH_COOLDOWN = 20000  // 两次自动切换之间至少隔这么久，避免来回抖
const LINE_AVOID_MS = 60000         // 刚自动切走的线路，这么久内不再切回去
const lineQuality = ref<Record<string, { ok: boolean; ms: number; kbps: number }>>({})
// 自动选线开关：关掉后，当前线路失败/变差时不再自动换线路/退出频道，
// 原地无限重试这条线路——由用户自己在「播放设置」的「线路」里手动选。
const autoLineSwitch = ref(true)
let lineProbeTimer = 0
let bufferWindowStart = 0
let bufferStallMs = 0
let waitingSince = 0
let lastAutoSwitchAt = 0
let avoidLines: Record<string, number> = {}

// ===== 直播「卡死不报错」兜底：有些源的 m3u8 没被正确识别/重写（比如上游
// Content-Type 乱标成 text/html），分片地址没走代理就原样甩给浏览器，
// hls.js 跨域请求分片大概率撞 CORS，而且 hls.js 对"分片一直加载失败"不会
// 很快判定成 fatal 错误——表现成画面永远停在「缓冲中」，上面 ERROR 回调里
// 的换线路逻辑压根没机会触发。这里单独起两层「这条线到底有没有真正站稳」
// 的看门狗，跟下面的卡顿检测（STALL_WINDOW_MS 那套）不是一回事——那套是
// 「播了一阵子之后才变差」，专门给已经确认稳定的线路用，判定要慢、要稳，
// 不能见一次卡顿就跳；这里两层都是「这条线压根没有第一次真正站稳过」：
//   1. LIVE_STALL_WATCHDOG_MS：从切到这条线开始，等不到第一次 onplaying。
//   2. LIVE_EARLY_STALL_GRACE_MS：等到了第一次 onplaying，但只是昙花一现
//      （实测过一些源：广告片花轮播间隙能解出一两帧，随后又永远卡住），
//      给它一个更短的宽限期确认是不是真的撑住了；撑过去之后才彻底交棒
//      给 STALL_WINDOW_MS 那套慢速检测。
const LIVE_STALL_WATCHDOG_MS = 8000
const LIVE_EARLY_STALL_GRACE_MS = 4000
let liveStallTimer = 0
// 自动选线关掉之后「原地无限重试同一条线路」的节流定时器：两次完整重建
// 播放器之间留个短间隔，避免线路瞬间失败（比如地址被猜错容器类型那种，
// 没有真实网络等待）时原地死循环狂打上游请求。
const RETRY_FOREVER_DELAY_MS = 1500
let retryForeverTimer = 0
// 这条线这一轮有没有已经武装过"宽限期"检查——只在第一次 onplaying 时武装
// 一次，避免播放过程中正常的卡顿/恢复反复重新武装，跟 STALL_WINDOW_MS
// 那套抢统治权。
let earlyStallArmed = false

function armLiveStallWatchdog() {
  clearLiveStallWatchdog()
  earlyStallArmed = false
  if (!isLive) return
  liveStallTimer = window.setTimeout(onLiveStallTimeout, LIVE_STALL_WATCHDOG_MS)
}

/** 第一次真正出画面后调用一次：换成更短的宽限期看门狗，确认是不是真的撑住了。 */
function armLiveEarlyStallGrace() {
  if (!isLive || earlyStallArmed) return
  earlyStallArmed = true
  clearLiveStallWatchdog()
  liveStallTimer = window.setTimeout(onLiveStallTimeout, LIVE_EARLY_STALL_GRACE_MS)
}

function onLiveStallTimeout() {
  liveStallTimer = 0
  if (playing.value) return // 保险：真播起来了（没在缓冲）就不该走到这
  lineFatalTotal++
  failLineOrGiveUp('networkError')
}

function clearLiveStallWatchdog() {
  if (liveStallTimer) {
    clearTimeout(liveStallTimer)
    liveStallTimer = 0
  }
}

function resetStallWindow() {
  bufferWindowStart = Date.now()
  bufferStallMs = 0
  waitingSince = 0
}

function startLineQualityWatch() {
  stopLineQualityWatch()
  if (!isLive) return
  resetStallWindow()
  void probeAltLines()
  lineProbeTimer = window.setInterval(probeAltLines, LINE_PROBE_INTERVAL)
}

function stopLineQualityWatch() {
  if (lineProbeTimer) {
    clearInterval(lineProbeTimer)
    lineProbeTimer = 0
  }
}

async function probeAltLines() {
  if (!isLive) return
  const ep = player.current
  if (!ep) return
  const alts = ep.urls.filter((u) => u && u !== rawUrl)
  if (!alts.length) return
  try {
    const results = await api.liveLinesProbe(alts)
    const next = { ...lineQuality.value }
    for (const r of results) next[r.url] = { ok: r.ok, ms: r.ms, kbps: r.kbps }
    lineQuality.value = next
  } catch {
    /* 探测失败不影响播放，静默忽略 */
  }
}

function onBufferingStart() {
  if (!isLive) return
  waitingSince = Date.now()
}

function onBufferingEnd() {
  if (!isLive || !waitingSince) return
  const now = Date.now()
  accumulateStall(now - waitingSince)
  waitingSince = 0
  void checkDegraded()
}

function accumulateStall(ms: number) {
  const now = Date.now()
  // 窗口已经过期（很久没卡过）就整体重置，避免「很久以前卡过一次」一直算数
  if (now - bufferWindowStart > STALL_WINDOW_MS) {
    bufferWindowStart = now
    bufferStallMs = 0
  }
  bufferStallMs += Math.max(0, ms)
}

async function checkDegraded() {
  if (!autoLineSwitch.value) return // 自动选线关闭：不做「卡顿变差就主动换线」这类主动切换
  const now = Date.now()
  const windowElapsed = now - bufferWindowStart
  if (windowElapsed < STALL_WINDOW_MS * 0.5) return // 窗口太短，数据还不够判断
  if (bufferStallMs / windowElapsed < STALL_RATIO_BAD) return
  if (now - lastAutoSwitchAt < AUTO_SWITCH_COOLDOWN) return
  await tryAutoSwitch()
}

async function tryAutoSwitch() {
  const ep = player.current
  if (!ep) return
  const now = Date.now()
  const alts = ep.urls.filter((u) => u && u !== rawUrl && (avoidLines[u] || 0) < now)
  if (!alts.length) return
  let candidates = alts.map((u) => ({ u, q: lineQuality.value[u] })).filter((x) => x.q?.ok)
  if (!candidates.length) {
    // 还没测过数据：临时测一下再决定，总比瞎切强
    try {
      const fresh = await api.liveLinesProbe(alts)
      const next = { ...lineQuality.value }
      for (const r of fresh) next[r.url] = { ok: r.ok, ms: r.ms, kbps: r.kbps }
      lineQuality.value = next
    } catch { /* ignore */ }
    candidates = alts.map((u) => ({ u, q: lineQuality.value[u] })).filter((x) => x.q?.ok)
  }
  if (!candidates.length) return
  candidates.sort((a, b) => a.q!.ms - b.q!.ms)
  const best = candidates[0].u
  lastAutoSwitchAt = now
  avoidLines[rawUrl] = now + LINE_AVOID_MS
  app.showTip('当前线路卡顿明显，已自动切换到更好的线路')
  await switchLine(best, true)
}

// 当前线路反复 fatal 错误、判定为挂了之后调用：在同一个频道里挑一条还没
// 试过的线路切过去。优先挑「探测结果确认能用」的，一条都没探测到能用的
// 时候也别干等——矬子里拔将军，挑一条没试过的硬切一次，总比反复重试同一条
// 已经挂了的线路、或者直接认输好。调用方已经确认存在未试过的线路。
// 同一条坏线路的 fatal 错误往往连续炸好几次（hls.js 内部分片/manifest
// 各自报一次），这几次都会落进这个函数——没有这个锁的话，第一次调用
// 还在 await 探测/切换的路上，后面几次重复调用会并发再切一次，互相打架。
let switchingLine = false

async function trySwitchToUntriedLine() {
  if (switchingLine) return
  const ep = player.current
  if (!ep) return
  const alts = ep.urls.filter((u) => u && u !== rawUrl && !triedLines.has(u))
  if (!alts.length) return
  switchingLine = true
  try {
    // ep.urls 已经是服务端按可靠度分数从高到低排好序的（见后端
    // annotateLineScores），顺着这个顺序选第一条没试过的就是「当前已知
    // 最靠谱」的那条，不需要现场再测一轮来排名——现场测
    // （api.liveLinesProbe）对"这个频道好几条线路都有问题"这种情况反而是
    // 最大的延迟来源：被测的候选本来就大概率是坏线路，每条最多等 4 秒
    // 超时、6 条一批并发，测完一整批轻松吃掉小十秒，换线路本身要等的
    // 时间比播放器真正重试的时间还长。仍然优先用 lineQuality 里已有的
    // 现成数据（后台 25 秒轮询攒下来的，不浪费），只有完全没有任何已知
    // 数据时才退回「按服务端顺序选第一条」，不再现场发起探测。
    const candidates = alts.map((u) => ({ u, q: lineQuality.value[u] })).filter((x) => x.q?.ok)
    const pick = candidates.length ? candidates.sort((a, b) => a.q!.ms - b.q!.ms)[0].u : alts[0]
    app.showTip('当前线路播放失败，自动切换到该频道的其它线路…')
    await switchLine(pick, true)
  } finally {
    switchingLine = false
  }
}

function clearRetryForeverTimer() {
  if (retryForeverTimer) {
    clearTimeout(retryForeverTimer)
    retryForeverTimer = 0
  }
}

/**
 * 自动选线关掉之后的失败收尾：不换线路、不放弃退出，原地无限重试这一条
 * 线路——用户明确表示「就守着这条线」，不想被换到别的线路或被弹回频道
 * 列表。隔 RETRY_FOREVER_DELAY_MS 再重建播放器，避免瞬间失败（比如地址
 * 被猜错容器类型、完全没有真实网络等待）时原地死循环狂打上游请求。
 */
function retryCurrentLineForever() {
  app.showTip('当前线路播放失败，自动选线已关闭，继续重试这条线路…', 2000)
  hlsRecover = 0
  lineFatalTotal = 0
  destroyPlayer()
  prepared.value = false
  playing.value = false
  buffering.value = true
  resolving.value = false
  errMsg.value = ''
  clearRetryForeverTimer()
  retryForeverTimer = window.setTimeout(() => {
    retryForeverTimer = 0
    initPlayer()
  }, RETRY_FOREVER_DELAY_MS)
}

/**
 * 当前线路播放失败时的统一收尾：直播优先在同一频道里换一条没试过的线路，
 * 换完才轮到 onPlayError 真正收尾（提示失败 + 退出）。自动选线关掉时则
 * 两者都不做，原地无限重试同一条线路（见 retryCurrentLineForever）。
 *
 * 之前 hls.js 的 ERROR 回调、onMpegtsFatal 各自写了一遍「isLive → 换线路/
 * 没得换才 onPlayError」，唯独 <video> 原生 onerror（mpegts.js 把一段文本
 * 内容当成 FLV/TS 解析失败时，MediaSource 关闭后 <video> 元素自己会报一个
 * 原生 error）跳过了这一层、直接调 onPlayError——表现成「频道只试了一条
 * 线路（通常是地址里没有 .m3u8 字样、被错误猜成 FLV/TS 容器交给 mpegts.js
 * 的那条）就瞬间判定'所有线路均无法播放'」，41 条备用线路里剩下的 40 条
 * 压根没机会被尝试。把这段收尾逻辑抽成共用函数，三处失败入口（hls fatal、
 * mpegts fatal、<video> 原生 error）统一走同一套「换线路优先」规则。
 */
function failLineOrGiveUp(errKind?: string) {
  if (isLive) {
    if (!autoLineSwitch.value) {
      retryCurrentLineForever()
      return
    }
    triedLines.add(rawUrl)
    const ep = player.current
    const hasUntried = !!ep && ep.urls.some((u) => u && u !== rawUrl && !triedLines.has(u))
    if (hasUntried) {
      void trySwitchToUntriedLine()
      return
    }
  }
  onPlayError(errKind)
}

const videoEl = ref<HTMLVideoElement>()
let hls: any | null = null
let mpegtsPlayer: any | null = null
// 画中画跨路由续播：组件卸载时如果正处于系统画中画，不走下面的正常销毁，
// 而是把 <video> 搬去 App 级常驻容器（见 onBeforeUnmount/playerGhost.ts）。
// ghosted 为 true 期间，这个组件实例的闭包状态（curMs/playing/triedLines
// 等等）、以及 hls/mpegtsPlayer 都继续原样跑，只是没有任何模板在渲染它们。
let ghosted = false
let ghostHandle: GhostHandle | null = null
// 同一条线路先试 flv 再试 mpegts（或反过来）只翻一次类型：猜错容器格式
// 时给一次改猜的机会，但两种都不对的坏线路不能在这两个类型之间无限
// 来回切换。见 initMpegtsPlayer/switchEpisode/switchLine/retry。
let mpegtsTypeFallbackTried = false
let hlsRecover = 0 // hls fatal 错误恢复次数，避免 seek/网络抖动直接判死
// 同一条线路累计报过多少次 fatal 错误，不会被 onplaying 清零（见下）。
// hlsRecover 会在每次恢复到「播了一点」（哪怕只有一两帧）时被 onplaying
// 重置为 0——这对「只是 seek 抖了一下」是对的，但碰上那种「播一下、断，
// 播一下、断」反复横跳的坏线路，hlsRecover 永远不会攒到 3，于是永远停在
// 「网络抖动，正在恢复…」原地重试同一条线路，表现成「提示一直在恢复，
// 但实际上永远不会真的恢复」。lineFatalTotal 只统计「这条线路」累计报过
// 几次 fatal（不管中间有没有短暂恢复过），超过阈值就判定这条线不稳定，
// 换下一条——只在真正换了线路/频道，或用户手动重试时才清零。
let lineFatalTotal = 0
const LINE_FATAL_LIMIT = 5
// 当前频道这一轮已经试过（且确认播不通）的线路：同一条线路反复报 fatal
// 错误还是不行，大概率是这条线真的挂了——「网络抖动，正在恢复…」只在
// 原地无限重试同一条死线路，永远不会恢复。换台（switchEpisode）时清空，
// 换线路（switchLine）时特意不清，否则刚记下的失败线路会被立即抹掉。
let triedLines = new Set<string>()
// 这个频道这一轮试过的线路里，有没有出现过「编码格式浏览器不支持」
// （hls.js 的 MANIFEST_INCOMPATIBLE_CODECS_ERROR，常见于源是 HEVC/H.265
// 而浏览器/系统没有硬解码支持）。很多"央视类"频道的全部备用线路其实都是
// 同一路原始信号的转发/镜像，编码都一样——如果真是这个原因，41 条线路会
// 在很短时间内全部以"清单解析出的编码不兼容"这种不需要真正等网络超时的
// 方式快速失败一轮，表现成「瞬间就说全部线路都不能播」。跟真正的网络/
// 源失效区分开，省得用户以为是软件坏了。换台时清零，换线路不清
// （跟 triedLines 的清零时机保持一致）。
let codecIncompatibleSeen = false
// 这个频道这次打开有没有上报过播放结果：成功只报一次（第一次真正播起来就够
// 了，没必要每次缓冲恢复都报一遍），失败也只在「确认所有线路都不行」时报
// 一次。换台/重试时清零，换线路不清——一个频道里只要有一条线播成了，
// 这个频道就该标「能播」，不该被同频道里别的坏线路覆盖成「不行」。
let liveStatusReported = false

function reportLiveStatus(ok: boolean) {
  if (!isLive || liveStatusReported) return
  liveStatusReported = true
  const name = player.current?.name || title.value
  if (!name) return
  api.setLiveChannelStatus(name, ok).catch(() => {})
}

const title = ref((route.query.title as string) || '')
const epLabel = ref('')
const resolving = ref(true)
const prepared = ref(false)
const playing = ref(false)
const buffering = ref(false)
const curMs = ref(0)
const durMs = ref(0)
const showOsd = ref(true)
const showQ = ref(false)
const showVol = ref(false)
const volume = ref(1)
const muted = ref(false)
const qLabel = ref('自动')
// 画质按钮的 title：把「自动/手动」「正在播哪一档」这层语义放这里，
// 按钮本体只留档位，避免移动端被长文案挤变形。
const qTitle = ref('当前画质：自动')
const via = ref('')
const errMsg = ref('')

// ===== 编码信息面板 =====
// 故意不受 prepared 限制：这个面板的核心用途之一就是「播不出来的时候
// 也能看一眼到底拿到了多少信息」（容器、分辨率、编码、码率……），prepared
// 从没变 true 过也应该能看——哪怕只有 via/url 这两项，也比完全没有强。
const showCodecInfo = ref(false)
interface CodecInfo {
  container: string
  via: string
  url: string
  isLive: boolean
  durationSec: number | null
  width: number | null
  height: number | null
  fps: number | null
  videoCodec: string
  audioCodec: string
  videoBitrateKbps: number | null
  audioBitrateKbps: number | null
  audioSampleRate: number | null
  audioChannels: number | null
  levelCount: number | null
  droppedFrames: number | null
  totalFrames: number | null
  speedKBps: number | null
}
function blankCodecInfo(): CodecInfo {
  return {
    container: '', via: '', url: '', isLive: false, durationSec: null,
    width: null, height: null, fps: null, videoCodec: '', audioCodec: '',
    videoBitrateKbps: null, audioBitrateKbps: null, audioSampleRate: null,
    audioChannels: null, levelCount: null, droppedFrames: null, totalFrames: null,
    speedKBps: null
  }
}
const codecInfo = ref<CodecInfo>(blankCodecInfo())
let codecInfoTimer = 0

function toggleCodecInfo() {
  showCodecInfo.value = !showCodecInfo.value
}

/**
 * 刷新「跟容器/编码库无关、随时能从 <video>/mpegts 读到」的那部分信息：
 * 分辨率、时长、丢帧数、mpegts 的实时下载速度。每秒刷新一次（见
 * initPlayer 里起的定时器），不管面板开没开着都刷新——面板随时打开都要
 * 是新的，没必要为了省这点计算量专门去跟踪面板开关状态。
 * 容器/编码/码率这些「一次解析出来就不怎么变」的字段由各自的解封装库
 * 事件（hls MANIFEST_PARSED/LEVEL_SWITCHED、mpegts MEDIA_INFO）单独写入，
 * 不在这里处理。
 */
function refreshLiveCodecStats() {
  const vid = videoEl.value
  if (!vid) return
  if (vid.videoWidth) codecInfo.value.width = vid.videoWidth
  if (vid.videoHeight) codecInfo.value.height = vid.videoHeight
  if (!isLive && vid.duration && isFinite(vid.duration)) {
    codecInfo.value.durationSec = (remuxMode.value ? remuxSeek.value : 0) + vid.duration
  }
  const q = (vid as any).getVideoPlaybackQuality?.()
  if (q) {
    codecInfo.value.totalFrames = q.totalVideoFrames
    codecInfo.value.droppedFrames = q.droppedVideoFrames
  }
  if (mpegtsPlayer) {
    const si = mpegtsPlayer.statisticsInfo
    if (si) {
      if (typeof si.speed === 'number') codecInfo.value.speedKBps = Math.round(si.speed)
      if (typeof si.decodedFrames === 'number') codecInfo.value.totalFrames = si.decodedFrames
      if (typeof si.droppedFrames === 'number') codecInfo.value.droppedFrames = si.droppedFrames
    }
  }
}

/** 把 hls.js 某一档（Level）的信息写进编码面板。MANIFEST_PARSED（起播档）
 *  和 LEVEL_SWITCHED（ABR 换档）都调这个，保证面板显示的始终是「当前
 *  实际在播」的那一档，而不是固定的第一档。 */
function applyHlsLevelInfo(lv: any) {
  if (!lv) return
  if (lv.width) codecInfo.value.width = lv.width
  if (lv.height) codecInfo.value.height = lv.height
  if (lv.bitrate) codecInfo.value.videoBitrateKbps = Math.round(lv.bitrate / 1000)
  codecInfo.value.videoCodec = lv.videoCodec || lv.attrs?.CODECS || codecInfo.value.videoCodec
  codecInfo.value.audioCodec = lv.audioCodec || codecInfo.value.audioCodec
  const fr = lv.attrs?.['FRAME-RATE']
  if (fr) codecInfo.value.fps = parseFloat(fr)
}

/** 把 codecInfo 摊成「标签 + 值」的行，面板模板直接遍历渲染，值是空串的
 *  行模板那边会跳过——没必要为了凑格式把「未知」硬塞满整个面板。 */
const codecInfoRows = computed(() => {
  const c = codecInfo.value
  const fmtKbps = (v: number | null) => (v != null ? `${v} kbps` : '')
  const fmtHz = (v: number | null) => (v != null ? `${v} Hz` : '')
  const fmtDur = (v: number | null) => {
    if (v == null) return ''
    const h = Math.floor(v / 3600)
    const m = Math.floor((v % 3600) / 60)
    const s = Math.floor(v % 60)
    return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}` : `${m}:${String(s).padStart(2, '0')}`
  }
  return [
    { label: '播放方式', value: c.via },
    { label: '容器/解封装', value: c.container },
    { label: '类型', value: c.isLive ? '直播' : '点播' },
    { label: '分辨率', value: c.width && c.height ? `${c.width} × ${c.height}` : '' },
    { label: '帧率', value: c.fps != null ? `${c.fps.toFixed(2)} fps` : '' },
    { label: '视频编码', value: c.videoCodec },
    { label: '音频编码', value: c.audioCodec },
    { label: '视频码率', value: fmtKbps(c.videoBitrateKbps) },
    { label: '音频码率', value: fmtKbps(c.audioBitrateKbps) },
    { label: '采样率', value: fmtHz(c.audioSampleRate) },
    { label: '声道数', value: c.audioChannels != null ? String(c.audioChannels) : '' },
    { label: '时长', value: !c.isLive ? fmtDur(c.durationSec) : '' },
    { label: '可选画质档数', value: c.levelCount != null ? String(c.levelCount) : '' },
    { label: '下载速度', value: c.speedKBps != null ? `${c.speedKBps} KB/s` : '' },
    { label: '已解码/丢帧', value: c.totalFrames != null ? `${c.totalFrames} / ${c.droppedFrames ?? 0}` : '' },
    { label: '播放地址', value: c.url }
  ]
})
const hint = ref('')
const playFlash = ref('')
const skipShow = ref('')
// 画面比例（默认值见下方 scaleFit 处的说明）
const skipLeft = ref(0)
const introSkipSec = ref(0)
const outroSkipSec = ref(0)
// 片头片尾跳过按「剧」分别记：不同剧的片头长度差很多，
// 一套全局值套所有剧必然错位（A 剧跳过 90 秒，套到片头只有 30 秒的 B 剧
// 会直接吞掉正片开头）。
// skipOwn 表示当前这部剧是否有自己的设置；没有时用全局默认值。
const skipOwn = ref(false)
const globalIntroSkip = ref(0)
const globalOutroSkip = ref(0)
// 按剧读写用的 key 两段（点播 site+vodId，网盘 srcId+vodName）
const skipScopeA = computed(() => (siteKey === 'webdav' ? player.wdSrcId : siteKey))
const skipScopeB = computed(() => (siteKey === 'webdav' ? player.wdVod : vodId))
// ===== 连续播放 =====
const autoPlayNext = ref(true)  // 自动连播开关，默认开启
// ===== 字幕支持（usePlayerSubtitles 组合式函数，见 composables/usePlayerSubtitles.ts）=====
const {
  subtitles,
  currentSubIdx,
  subDelay,
  subFontSize,
  subLabel,
  externalSubActive,
  loadSubtitlesFromHls,
  switchSubtitle,
  applySubStyle,
  setSubFontSize,
  removeExternalSub,
  detachExternalSub,
  reattachExternalSub,
  setExternalSub,
  setTimelineBase,
  onSubFile,
  promptSubUrl,
  setSubDelay,
  disposeSubtitles
} = usePlayerSubtitles({
  videoEl,
  getHls: () => hls,
  tip: (m: string) => app.showTip(m),
  proxy: (u: string) => proxied(u)
})

// ===== 倍速播放 =====
const playbackRate = ref(1.0)
const PLAYBACK_RATES = [0.5, 0.75, 1, 1.25, 1.5, 2]
// 1 → "1"，1.25 → "1.25"（不要显示成 1.0x 那种多余小数）
const rateLabel = computed(() => String(playbackRate.value))

let introDone = false
let outroFired = false
let skipTimer = 0
let osdTimer = 0
let flashTimer = 0
let volTimer = 0
let leave = false
let lastSaveMs = 0
/**
 * 续播时要用作起点的位置（毫秒）。initPlayer 消费一次后清零。
 * 必须在第一次 proxiedFinal() 之前就位，转封装流才能一次从目标位置起转。
 */
let pendingResumeMs = 0
let saveTimer = 0 // savePos 防抖 timer

// ===== Seek 防抖优化（三层防抖）=====
const SEEK_DEBOUNCE = 300      // 拖拽时防抖 300ms，给用户反馈
const SEEK_THROTTLE = 5000     // 最终落盘节流 5s，避免频繁网络请求
let lastSeekMs = 0             // 上次成功 seek 的时间戳
let seekPendingTimer = 0       // 等待中的 seek timer

const playPauseText = computed(() => (playing.value ? '暂停' : '播放'))
const muteText = computed(() => (muted.value ? '取消静音' : '静音'))
const volPct = computed(() => (muted.value ? '静音' : Math.round(volume.value * 100) + '%'))
const progressPct = computed(() => (durMs.value > 0 ? (curMs.value / durMs.value) * 100 : 0))

// ===== 移动端检测 =====
const isMobile = ref(false)
function checkMobile() { isMobile.value = window.innerWidth <= 640 }

// ===== 画中画 =====
const isPip = ref(false)
function supportsPip(): boolean {
  return !!(document as any).pictureInPictureEnabled
}
async function togglePip() {
  const vid = videoEl.value
  if (!vid) return
  try {
    const doc = document as any
    if (doc.pictureInPictureElement) {
      await doc.exitPictureInPicture()
    } else {
      await (vid as any).requestPictureInPicture()
    }
  } catch (e) {
    app.showTip('此设备不支持画中画')
  }
}

// ===== 投屏（Remote Playback API）=====
const castConnected = ref(false)
function remoteApi(): any {
  return (videoEl.value as any)?.remote
}
function supportsCast(): boolean {
  return !!remoteApi()
}
async function toggleCast() {
  const remote = remoteApi()
  if (!remote) {
    app.showTip('当前浏览器不支持投屏，可以用 Chrome / Edge 连接 Chromecast 设备')
    return
  }
  try {
    if (remote.state === 'connected') {
      await remote.disconnect()
      app.showTip('已断开投屏')
    } else {
      app.showTip('正在搜索投屏设备…')
      await remote.prompt()
    }
  } catch {
    app.showTip('投屏已取消，或没有可用设备')
  }
}

// ===== 全屏 =====
const playerEl = ref<HTMLElement>()
const isFullscreen = ref(false)
// 鼠标移到这么靠近左右边缘（像素）就当成"要收起界面"的手势——跟系统/
// 浏览器自己的边缘触发区（比如边缘唤出任务栏/手势返回）离得稍微远一点，
// 避免手滑碰到边就把界面收了。
const EDGE_HIDE_PX = 24
// 全屏时如果界面被隐藏，鼠标指针也跟着隐藏（见 hideUI）；非全屏时这个
// 标记不生效（有系统标题栏/任务栏，隐藏指针意义不大，模板里额外判断了
// isFullscreen 才应用对应 CSS 类）。
const cursorHidden = ref(false)

function tryLockLandscape() {
  try {
    const p = (screen as any)?.orientation?.lock?.('landscape')
    if (p?.catch) p.catch(() => {})
  } catch { /* 部分浏览器不支持，忽略 */ }
}

function tryUnlockOrientation() {
  try { (screen as any)?.orientation?.unlock?.() } catch { /* ignore */ }
}

function toggleFullscreen() {
  // 移动端统一走 CSS 全屏类，避免原生 Fullscreen API 与 isFullscreen 状态不同步
  if (isMobile.value) {
    isFullscreen.value = !isFullscreen.value
    if (isFullscreen.value) {
      tryLockLandscape()
      // 进全屏要重新计时 OSD：否则会接着竖屏那条 12s 定时器倒数，
      // 可能刚铺满屏、OSD 连进度条一起消失，用户想拖都找不到地方下手。
      popOsd()
    } else {
      tryUnlockOrientation()
    }
    return
  }
  const el = playerEl.value
  if (!el) return
  const doc = document as any
  const isFs =
    doc.fullscreenElement ||
    doc.webkitFullscreenElement ||
    doc.mozFullScreenElement ||
    doc.msFullscreenElement

  if (isFs) {
    ;(doc.exitFullscreen || doc.webkitExitFullscreen || doc.mozCancelFullScreen || doc.msExitFullscreen)?.call(doc)
  } else {
    const req = (el as any).requestFullscreen ||
      (el as any).webkitRequestFullscreen ||
      (el as any).mozRequestFullScreen ||
      (el as any).msRequestFullscreen
    req?.call(el)
    tryLockLandscape()
  }
}

function onFullscreenChange() {
  // 移动端全屏走 CSS 类（.m-fullscreen），与原生 Fullscreen API 无关。
  // 这里如果也去读 document.fullscreenElement，某些浏览器（尤其是从原生全屏
  // 退回、或外部触发的 fullscreenchange）会把 isFullscreen 改写成 false，
  // 结果全屏 OSD 和底部面板同时渲染 —— 界面上就出现上下两条进度条。
  // 所以移动端直接跳过，状态只由 toggleFullscreen 自己维护。
  if (isMobile.value) return
  const doc = document as any
  isFullscreen.value = !!(
    doc.fullscreenElement ||
    doc.webkitFullscreenElement ||
    doc.mozFullScreenElement ||
    doc.msFullscreenElement
  )
}

function syncEpLabel() {
  const n = player.count
  if (isLive) {
    epLabel.value = n > 1 ? `第 ${player.idx + 1}/${n} 台` : ''
    return
  }
  epLabel.value = n > 1 ? `第 ${player.idx + 1}/${n} 集` : ''
}

onMounted(async () => {
  // 片头片尾按剧读写：点播用 site+vodId，网盘用 srcId+vodName，
  // 与服务端 skipKey() 的约定一致。取不到就不带，读写全局默认值。
  const prefs = await api
    .prefs(skipScopeA.value, skipScopeB.value)
    .catch(() => ({
      qPref: 0, speedPref: 1, volumePref: 1,
      introSkip: 0, outroSkip: 0,
      curIntroSkip: 0, curOutroSkip: 0,
      skipOwn: false, skipScoped: false, skipCfgCount: 0,
      tapSound: 1, tapVibrate: 1
    }))
  qCap = prefs.qPref || 0
  // 优先用「本剧」的值；没单独设置过时服务端已经把全局默认填进 cur* 了
  introSkipSec.value = prefs.curIntroSkip ?? prefs.introSkip ?? 0
  outroSkipSec.value = prefs.curOutroSkip ?? prefs.outroSkip ?? 0
  skipOwn.value = !!prefs.skipOwn
  globalIntroSkip.value = prefs.introSkip || 0
  globalOutroSkip.value = prefs.outroSkip || 0
  // 应用倍速偏好（持久化）
  const sp = prefs.speedPref || 1
  if (PLAYBACK_RATES.includes(sp)) {
    playbackRate.value = sp
  }
  // 应用音量偏好（持久化，0=未设置用 1.0）
  volume.value = Math.min(1, Math.max(0, prefs.volumePref || 1))
  syncEpLabel()
  await safeResolve()
  initPlayer()
  startLineQualityWatch()
  if (videoEl.value) videoEl.value.volume = volume.value
  // 监听投屏连接状态（Remote Playback API）
  const remote = remoteApi()
  if (remote) {
    const onConnect = () => { castConnected.value = true; app.showTip('已连接投屏设备') }
    const onDisconnect = () => { castConnected.value = false; app.showTip('投屏已断开') }
    remote.addEventListener?.('connect', onConnect)
    remote.addEventListener?.('disconnect', onDisconnect)
    castHandlers = { onConnect, onDisconnect }
    castConnected.value = remote.state === 'connected'
  }
})

// 画中画跨路由续播：组件真正卸载（DOM 即将被移除）前，如果正处于系统
// 画中画，把 <video> 搬到 App 级常驻容器里，而不是让下面 onUnmounted 的
// destroyPlayer() 把它销毁。必须用 onBeforeUnmount（DOM 移除前）而不是
// onUnmounted（DOM 已经被移除后）——搬家要赶在 Vue 把 .player 这棵子树
// 摘掉之前完成，<video> 一旦先被摘掉就会触发浏览器自动退出画中画。
onBeforeUnmount(() => {
  const vid = videoEl.value
  if (!isPip.value || !vid) return
  ghosted = true
  document.getElementById(PIP_HOST_ID)?.appendChild(vid)
  const handle: GhostHandle = {
    stop: (opts) => {
      ghosted = false
      ghostHandle = null
      // 浏览器原生画中画没法区分用户点的是「关闭 ×」还是「返回标签页」——
      // 两者都只触发同一个 leavepictureinpicture 事件，没有任何字段能分辨。
      // 只能都当成「要回去」处理：真想彻底不看了，回到播放页后再点一次
      // 「返回」就是原来的正常退出流程。
      const restore = !!opts?.restore
      const resumeMs = !isLive ? realPosMs() : 0
      savePos()
      // 这个 stop 也会被「开始播放新内容」那条路径（stopCurrentGhost）直接
      // 调用，不一定是画中画自己先关掉触发的——这种情况下这里仍然是系统
      // 画中画正在显示的那个元素，必须显式退出，否则清空/摘除 <video> 后
      // 画中画窗口可能停留在最后一帧，变成一个点了没反应的「僵尸」悬浮窗。
      const doc = document as any
      if (doc.pictureInPictureElement === vid) {
        doc.exitPictureInPicture().catch(() => {})
      }
      destroyPlayer()
      vid.remove()
      if (restore) {
        // 点播才有「续播位置」的概念；直播回去就是重新进这个频道，
        // 从当前直播时刻接上即可，不需要（也没法）定位到某个时间点。
        if (!isLive && resumeMs > 0) player.seek = resumeMs
        // 带上 pipRestore=1 标记：新挂载的播放页实例要凭它知道「这次的
        // 返回按钮不能走 router.back()」（见 cameFromPipRestore/goBack）。
        const sep = ownFullPath.includes('?') ? '&' : '?'
        router.push(ownFullPath + sep + 'pipRestore=1').catch(() => {})
      }
    }
  }
  ghostHandle = handle
  setCurrentGhost(handle)
})

onUnmounted(() => {
  leave = true
  stopLineQualityWatch()
  // 清理所有 timers。原来只清了 4 个，漏掉 osdTimer / skipTimer /
  // flashTimer / volTimer —— 离开页面后 skipTimer 仍在跑，几秒后会调
  // doSkip()，对一个已卸载的组件调 switchEpisode，发出无意义的解析请求。
  if (saveTimer) clearTimeout(saveTimer)
  if (seekPendingTimer) clearTimeout(seekPendingTimer)
  if (volSaveTimer) clearTimeout(volSaveTimer)
  if (osdTimer) { clearTimeout(osdTimer); osdTimer = 0 }
  if (skipTimer) { clearInterval(skipTimer); skipTimer = 0 }
  if (flashTimer) { clearTimeout(flashTimer); flashTimer = 0 }
  if (volTimer) { clearTimeout(volTimer); volTimer = 0 }
  // 音量拖动把 move/up 绑到了 window，卸载时必须解绑，否则离开播放页后
  // 任何一次 pointermove 都会调到已销毁组件的 setVolumeFromEvent。
  onVolUp()
  detachProgressListeners()
  removeExternalSub()
  disposeSubtitles()
  if (castHandlers) {
    const remote = remoteApi()
    remote?.removeEventListener?.('connect', castHandlers.onConnect)
    remote?.removeEventListener?.('disconnect', castHandlers.onDisconnect)
    castHandlers = null
  }
  tryUnlockOrientation()
  // 画中画续播：上面 onBeforeUnmount 已经把 <video> 搬去常驻容器了，
  // 「保存进度 + 彻底销毁」交给 ghostHandle.stop()（画中画被关掉，或者
  // 下一次挂载 PlayerView 调 stopCurrentGhost() 时触发），这里不能提前做。
  if (ghosted) return
  cancelAutoplay()
  // 页面卸载时确保最终进度被保存
  savePos()
  destroyPlayer()
})

// ---------- 解析 ----------

function fixPlayable(u: string): string {
  if (!u || isDirectUrl(u) || !u.includes('://')) return u
  const noQuery = u.split('?')[0].replace(/\/+$/, '')
  const last = noQuery.slice(noQuery.lastIndexOf('/') + 1)
  if (last.length > 0 && !last.includes('.')) return noQuery + '/index.m3u8'
  return u
}

function isDirectUrl(u: string): boolean {
  const lu = u.toLowerCase()
  return ['.m3u8', '.mp4', '.flv', '.mkv', '.ts', '.m4v'].some((e) => lu.includes(e))
}

async function resolveUrl(skipLiveProbe = false) {
  finalUrl = ''
  via.value = ''
  hint.value = ''
  levels.value = []
  levelIdx = -1
  // 换源/切集后上一路的实际档位不再有效，等新的 LEVEL_SWITCHED 回写
  liveLevelIdx = -1
  // 新的源可以重新享受一次转封装兜底
  remuxFallbackTried = false
  errMsg.value = ''

  if (isLive) {
    // skipLiveProbe：switchLine() 换线路时，目标 url 已经是调用方特意挑出来
    // 的（用户手动选的，或者 HLS 播放失败后从 ep.urls 里选的下一条没试过的）。
    // 这种情况不能再跑一遍 pickPlayableLive() 的 /api/peek 探活——它只是个
    // 轻量连通性检查，跟 HLS 层面「这条线到底能不能真正播出内容」是两回事：
    // 曾经出现过某条线连通性检查能过、但 manifest 解析一直失败的情况，
    // pickPlayableLive 探活一通过就会把它当成「可用」原样塞回来，而它其实
    // 已经在 HLS 层面被判定成死线路（记在 triedLines 里）——两层互相不知道
    // 对方的判断，来回把同一条已确认播不通的线路送回去，变成死循环，永远
    // 卡在「缓冲中」又不断尝试、却怎么都播不出来。
    if (skipLiveProbe) {
      finalUrl = rawUrl
      via.value = '直播直连'
      return
    }
    const url = await pickPlayableLive()
    if (!url) {
      // 这个频道自己的线路（含备用线路）一条都连不上：不自动跳到下一个
      // 频道（那样用户会被悄悄带到一个自己没选的频道），退回频道选择页，
      // 用一条三秒的提示说清楚原因。
      reportLiveStatus(false)
      app.showTip('该频道暂时无法播放', 3000)
      goBack()
      return
    }
    finalUrl = url
    via.value = '直播直连'
    return
  }

  if (directMode) {
    finalUrl = rawUrl
    via.value = '直连'
    return
  }

  // 补全 m3u8
  const guess = fixPlayable(rawUrl)
  if (guess !== rawUrl) {
    try {
      const ok = await api.peek(guess)
      if (ok.alive) {
        finalUrl = guess
        via.value = '补全 m3u8'
        return
      }
    } catch {
      /* ignore */
    }
  }
  try {
    const r = await api.resolve(rawUrl)
    via.value = r.via
    finalUrl = fixPlayable(r.url)
    if (!r.ok) hint.value = '未能解析出真实播放地址（' + r.via + '），仍尝试直接播放'
  } catch {
    finalUrl = rawUrl
    via.value = '直链'
  }
}

/**
 * 解析地址的统一包装：保证 resolving 一定被复位。
 *
 * 之前三处调用点都写成 `resolving = true; await resolveUrl(); resolving = false`。
 * resolveUrl 内部虽然大部分分支自己 catch 了，但 pickPlayableLive 里对
 * player.current 的访问、以及个别 await 仍可能抛出去 —— 一旦抛出，
 * resolving 永远停在 true，界面就卡在「正在解析播放地址…」不动，
 * 连重试按钮都出不来。
 */
async function safeResolve(skipLiveProbe = false) {
  try {
    await resolveUrl(skipLiveProbe)
  } catch (e) {
    if (!finalUrl) {
      errMsg.value = '解析播放地址失败，请点击重试或返回换一个源'
      showOsd.value = true
    }
  } finally {
    resolving.value = false
  }
}

async function probeLive(u: string): Promise<boolean> {
  try {
    const r = await api.peek(u)
    return r.alive
  } catch {
    return false
  }
}

// 只探测「这一个频道」自己的线路（rawUrl + 它的备用线路），探完还是没有一条
// 能用就返回空串——不再自动跳去下一个频道。频道完全播不通时，用户想知道的
// 是「这个频道不行」，而不是被悄悄带到一个自己没选的频道上，所以这里交给
// 调用方（resolveUrl）统一走「退回频道列表 + 提示」。
async function pickPlayableLive(): Promise<string> {
  if (await probeLive(rawUrl)) return rawUrl
  const ep = player.current
  if (ep) {
    for (const alt of ep.urls) {
      if (alt === rawUrl) continue
      if (await probeLive(alt)) {
        rawUrl = alt
        app.showTip('已切到该频道的备用线路')
        return alt
      }
    }
  }
  return ''
}

// ---------- 播放器 ----------

// 浏览器 <video>/MSE 不支持这些容器：即使里面的 H.264/AAC 能解也播不了，
// 必须由服务端转封装成 fMP4（/api/remux）。
// 注意：.flv/.ts/.m2ts 不在这里——这三种容器 mpegts.js 能在浏览器端直接
// 解封装（见 initMpegtsPlayer），不需要服务端 ffmpeg 转码，优先走那条路，
// 省掉树莓派本就吃紧的 CPU；真的播不动（比如编码不是 H.264/AAC）再落到
// 下面 onPlayError 的转封装兜底。
const REMUX_EXTS = ['.mkv', '.avi', '.wmv', '.rmvb', '.rm', '.mov', '.mpg', '.mpeg', '.vob']

/** 从 URL 取容器扩展名（忽略 query/hash） */
function containerExt(u: string): string {
  const clean = u.split('?')[0].split('#')[0].toLowerCase()
  const dot = clean.lastIndexOf('.')
  return dot >= 0 ? clean.slice(dot) : ''
}

function needsRemux(u: string, force = false): boolean {
  if (force) return true
  const ext = containerExt(u)
  // 无扩展名（网盘常见，如「异星战境 Atlas」）无法从名字判断，先交给浏览器试；
  // 真失败时 onPlayError 会带 force 再走一次转封装兜底。
  return ext !== '' && REMUX_EXTS.includes(ext)
}

/** 点播场景下是否该走 mpegts.js（FLV/裸 TS 容器），而不是原生 <video src>。 */
function isMpegtsContainer(u: string): boolean {
  const ext = containerExt(u)
  return ext === '.flv' || ext === '.ts' || ext === '.m2ts'
}

function proxiedFinal(forceRemux = false): string {
  const auth = player.header?.auth || undefined
  // MKV 之类：服务端转封装。输出是分片 fMP4 流、不支持 Range，
  // 拖进度条时会用 seek 参数重新起转（见 reloadAt）。
  if (needsRemux(finalUrl, forceRemux) && app.status?.remux) {
    remuxMode.value = true
    return remuxed(finalUrl, auth, remuxSeek.value)
  }
  remuxMode.value = false
  // 认证信息与「是否直连」无关：只要上游需要 Basic Auth（WebDAV 网盘就是），
  // 无论直连还是走 /api/proxy 都必须带上。
  // 之前写成 directMode ? auth : undefined —— 网盘走的是普通代理模式，
  // 代理拿不到认证 → 上游 401 → <video> 触发 error → 被误报成「视频解码失败」。
  return proxied(finalUrl, auth)
}

async function initPlayer() {
  if (!finalUrl || !videoEl.value) return
  const vid = videoEl.value
  // 直播「卡死不报错」兜底：见 armLiveStallWatchdog 定义处注释。
  // 每条线路开始尝试时都重新起这个看门狗（对点播无效果，函数内部自己判断）。
  armLiveStallWatchdog()
  // 续播：必须在第一次 proxiedFinal() 之前把起点塞进 remuxSeek，
  // 这样转封装那条流一次就从目标位置起转，不用「先拉 0 秒再跳」。
  // 之前是等 loadedmetadata 之后再 applySeek，等于白白多请求一次，
  // 而且那次跳转常常被 seekingRemux 的防重入窗口吃掉 → 续播失效。
  if (player.seek > 0 && !isLive) {
    pendingResumeMs = player.seek
    player.seek = 0
    remuxSeek.value = Math.max(0, Math.floor(pendingResumeMs / 1000))
  }
  // 兜底状态下（无扩展名文件靠强制转封装在播）重新 init 也要保持强制
  const src = proxiedFinal(remuxFallbackTried)
  // 编码信息面板：每换一条线路/一次重试都从空白状态重新累积，不保留上一
  // 条线路的旧数据——旧数据会让人误以为新线路也是那个容器/编码。
  codecInfo.value = blankCodecInfo()
  codecInfo.value.via = via.value
  codecInfo.value.url = finalUrl
  codecInfo.value.isLive = isLive
  if (codecInfoTimer) clearInterval(codecInfoTimer)
  codecInfoTimer = window.setInterval(refreshLiveCodecStats, 1000)
  // 直播频道不等于一定是 m3u8：之前这里拿 via.value === '直播直连' 把所有
  // 直播播放统统塞进 hls.js，碰到实际是 FLV/裸 TS 容器的直播线路（URL 里
  // 看不出扩展名，常见于虎牙/斗鱼这类转播镜像）时，hls.js 会把这段二进制
  // 数据硬当成 m3u8 manifest 解析，必然失败——现在把「是不是直播」和
  // 「是不是 m3u8」拆成两个独立判断，直播但不是 m3u8 的交给 initMpegtsPlayer。
  if (src.includes('.m3u8') || via.value === '补全 m3u8') {
    const HlsCls = await loadHls()
    if (HlsCls.isSupported()) {
      // 缓冲策略：直播追求低延迟用小缓冲 + 限制最大缓冲防内存膨胀；
      // 点播用较大缓冲保证网络抖动时的流畅度。
      // maxBufferLength 默认 30s（1080p 会占 100MB+ 内存），这里收紧到 10s。
      //
      // 直播额外收紧了 manifest/分片的重试次数和退避延迟：hls.js 默认对每个
      // 清单/分片请求会在内部自己重试好几次（分片默认 6 次，退避延迟从 1s
      // 翻倍），这些重试完全在 hls.js 内部完成，不会触发我们监听的
      // Events.ERROR（那个事件只在 hls.js 自己的重试预算耗尽、判定 fatal
      // 之后才会发出）——也就是说下面「软重试几次就换线路」那套逻辑压根
      // 没机会介入，真正卡住时间的是 hls.js 自己这层还没认输。实测过一条
      // 反复 503 的广告片花分片，仅这一层内部重试就能吃掉 10+ 秒。直播
      // 线路多，没必要给 hls.js 这么宽裕的自愈机会，收紧成最多重试 1 次、
      // 0.5s 退避，让它更快把 fatal 错误交还给我们去换线路；点播没有备用
      // 线可换，保留 hls.js 的默认重试策略，优先保流畅不中断。
      const hlsCfg: any = isLive
        ? {
            maxBufferLength: 10,
            maxMaxBufferLength: 30,
            manifestLoadingMaxRetry: 1,
            manifestLoadingRetryDelay: 500,
            levelLoadingMaxRetry: 1,
            levelLoadingRetryDelay: 500,
            fragLoadingMaxRetry: 1,
            fragLoadingRetryDelay: 500
          } // 直播：最多缓冲 30s，重试收紧到「快速认输」
        : { maxBufferLength: 30 }                                      // 点播：缓冲 30s 保流畅
      const h = new HlsCls(hlsCfg)
      hls = h
      h.loadSource(src)
      h.attachMedia(vid)
      h.on(HlsCls.Events.MANIFEST_PARSED, () => {
        codecInfo.value.container = 'HLS (m3u8)'
        codecInfo.value.levelCount = h.levels?.length || null
        applyHlsLevelInfo(h.levels?.[h.currentLevel >= 0 ? h.currentLevel : 0])
        if (h.levels && h.levels.length > 1) {
          levels.value = fromHlsLevels(h.levels)
          applyInitialLevel()
        }
        // 尝试从 m3u8 中加载字幕轨道
        loadSubtitlesFromHls(h)
      })
      // 自动模式下 hls.js 会按带宽自己升降档。
      // 不监听这个事件的话 levelIdx 永远是 -1，按钮一直显示「自动」，
      // 用户不知道现在实际跑的是 720P 还是 1080P。
      h.on(HlsCls.Events.LEVEL_SWITCHED, (_e: any, data: any) => {
        const i = typeof data?.level === 'number' ? data.level : h.currentLevel
        liveLevelIdx = i >= 0 ? i : -1
        syncQLabel()
        applyHlsLevelInfo(h.levels?.[i])
      })
      // 自动模式下也把 ABR 起始档位记下来，避免首个分片前一片空白
      h.on(HlsCls.Events.LEVEL_LOADED, (_e: any, data: any) => {
        if (liveLevelIdx >= 0) return
        const i = typeof data?.level === 'number' ? data.level : -1
        if (i >= 0) {
          liveLevelIdx = i
          syncQLabel()
        }
      })
      h.on(HlsCls.Events.ERROR, (_e: any, data: any) => {
        if (!data.fatal) return
        const isNetOrMedia = data.type === HlsCls.ErrorTypes.NETWORK_ERROR || data.type === HlsCls.ErrorTypes.MEDIA_ERROR
        if (isNetOrMedia) lineFatalTotal++
        // manifest 级别的致命错误（地址返回的根本不是 m3u8，比如重定向到了一个
        // 普通视频文件/错误页，或者整份清单直接加载失败/超时）：h.startLoad()
        // 对这种错误没用——它是「续」已经建立起来的加载状态用的，这条线从一
        // 开始就没能解析出任何 level，调多少次 startLoad() 都只是原地空等，
        // 不会产生任何新的网络请求。表现出来就是「网络抖动，正在恢复…」提示
        // 一闪而过之后，画面永远停在「缓冲中」，什么都不会再发生。这类错误
        // 直接跳过软重试，走下面的换线路逻辑。
        const isManifestFailure =
          data.details === HlsCls.ErrorDetails.MANIFEST_LOAD_ERROR ||
          data.details === HlsCls.ErrorDetails.MANIFEST_LOAD_TIMEOUT ||
          data.details === HlsCls.ErrorDetails.MANIFEST_PARSING_ERROR ||
          data.details === HlsCls.ErrorDetails.MANIFEST_INCOMPATIBLE_CODECS_ERROR
        if (isLive && data.details === HlsCls.ErrorDetails.MANIFEST_INCOMPATIBLE_CODECS_ERROR) {
          codecIncompatibleSeen = true
        }
        // lineFatalTotal 没到阈值、也不是 manifest 级别错误时才走「原地软
        // 重试」：到了阈值说明这条线反复横跳（播一下断一下，hlsRecover 被
        // onplaying 清零好几轮了），原地重试已经没意义，直接跳去下面的换
        // 线路逻辑。
        // 直播频道通常有十几到几十条备用线路，原地软重试的价值远低于「尽快
        // 换一条好的线」——实测过不少「历史评分很高但这会儿正好抽风」的线
        // （广告片花段 502/503、分片指向只有运营商内网能连通的私网地址），
        // 原来给 3 次软重试机会，每次都要等 hls.js 重新拉一遍清单+分片，
        // 一条坏线路就能拖十几秒，好几条连着挂就是几十秒。点播没有「还有
        // 别的线」这个选项，软重试次数维持原来更宽松的 3 次，避免偶发抖动
        // 就直接放弃唯一这条源、白白触发转封装兜底。
        const recoverBudget = isLive ? 1 : 3
        // 实测发现有些"坏线路"会一惊一乍地"播一下、断"反复横跳（广告片花
        // 轮播到真正节目之间那一小段，偶尔能渲染出一两帧）——这类线路每
        // "播一下"都会把 hlsRecover 清零（见上面注释），hlsRecover 的软
        // 重试预算形同虚设，真正兜底的是 lineFatalTotal 这个不会被清零的
        // 计数器。原来给到 5 次才判死，换一条十几秒就的线路，等于白白
        // 多等好几次 hls.js 自己的清单重载周期。直播线路多，没必要给这么
        // 宽松的容忍度；点播没有备用线可换，维持原来更宽松的阈值。
        const fatalLimit = isLive ? 2 : LINE_FATAL_LIMIT
        // 自动选线关掉：用户明确表示「就守着这条线，别自作主张换别的」，
        // 软重试预算/阈值这些「攒够次数就放弃原地重试」的判断全部失效——
        // 不限次数地原地重试，不去碰 triedLines/换线路/放弃退出那一整套。
        const unlimitedRetry = isLive && !autoLineSwitch.value
        if (unlimitedRetry || (!isManifestFailure && lineFatalTotal < fatalLimit && hlsRecover < recoverBudget)) {
          if (!unlimitedRetry) hlsRecover++
          if (data.type === HlsCls.ErrorTypes.NETWORK_ERROR) {
            app.showTip('网络抖动，正在恢复…')
            h.startLoad()
            return
          }
          if (data.type === HlsCls.ErrorTypes.MEDIA_ERROR) {
            app.showTip('解码异常，正在恢复…')
            h.recoverMediaError()
            return
          }
        }
        // 到这还没 return：要么是非 network/media 的 fatal 错误，要么这条
        // 线路的 fatal 错误已经攒够阈值——原地再怎么重试也是白等，这条线
        // 大概率真挂了。直播频道通常有好几条线路（Channel.urls，合并自多个
        // 直播源），先试同一个频道里还没试过的其它线路，而不是直接报错
        // 或者跳去完全不同的频道。
        failLineOrGiveUp(data.details || data.type)
      })
    } else if (vid.canPlayType('application/vnd.apple.mpegurl')) {
      codecInfo.value.container = 'HLS (m3u8, 浏览器原生)'
      vid.src = src
    }
  } else if (isLive) {
    mpegtsTypeFallbackTried = false
    await initMpegtsPlayer(src, vid, undefined, true)
  } else if (!remuxFallbackTried && isMpegtsContainer(finalUrl)) {
    // 点播里的 FLV/裸 TS 文件（网盘/抓取站常见）：同样原生 <video> 解不了
    // 容器，优先交给 mpegts.js 在浏览器端解封装，而不是直接上服务端 ffmpeg
    // 转码——真要是编码本身不支持（非 H.264/AAC），mpegtsPlayer 会报
    // MEDIA_ERROR，走 onMpegtsFatal → onPlayError 的转封装兜底收尾。
    // remuxFallbackTried 为 true 说明已经被强制走过转封装（src 这时候已经
    // 是服务端吐出来的 fMP4 流，不再是原始 FLV/TS），不能再送进 mpegts.js。
    mpegtsTypeFallbackTried = false
    await initMpegtsPlayer(src, vid, undefined, false)
  } else {
    codecInfo.value.container = remuxMode.value ? '转封装 (服务端 ffmpeg → fMP4)' : '原生播放（浏览器直接解封装，读不到编码详情）'
    vid.src = src
  }
  bindVideoEvents(vid)
  // 新流就绪后把之前用过的外挂字幕重新挂回来（切集时 detach 过）。
  // 必须在这里做：<track> 要挂到 video 元素上，而 video 是同一个节点，
  // 换源时其子节点会被清掉，所以每次 initPlayer 都要重新 append。
  reattachExternalSub()
  // 应用倍速偏好（已从 prefs 读取）
  if (playbackRate.value !== 1) {
    vid.playbackRate = playbackRate.value
    if (hls) hls.config.playbackRate = playbackRate.value
  }
  vid.play().catch(() => {})
}

// URL 字符串里能看出 .ts/.m2ts 后缀的走 mpegts(TS) 解封装，其余（包括完全
// 没有后缀的——真实容器要等实际连上才知道，常见于经过跳转的直播间地址）
// 先试 flv：国内直播转播最常见的封装格式（虎牙/斗鱼之类基本都是 flv），
// 猜错了会在下面 ERROR 回调里翻一次类型重试，不会直接判死整条线路。
function guessMpegtsType(u: string): 'flv' | 'mpegts' {
  const clean = u.split('?')[0].split('#')[0].toLowerCase()
  return clean.endsWith('.ts') || clean.endsWith('.m2ts') ? 'mpegts' : 'flv'
}

async function initMpegtsPlayer(src: string, vid: HTMLVideoElement, type?: 'flv' | 'mpegts', live = true) {
  const MpegtsCls = await loadMpegts()
  if (!MpegtsCls.isSupported()) {
    // 当前浏览器不支持 MSE：没有更好的兜底了，走跟 hls.js fatal 错误一样的
    // 「换这个频道的下一条线路，都没有就交给 onPlayError 收尾」流程。
    onMpegtsFatal()
    return
  }
  const t = type || guessMpegtsType(src)
  const p = MpegtsCls.createPlayer(
    { type: t, isLive: live, url: src },
    // 直播追求低延迟：关掉 IO 暂存缓冲，开启「追帧」避免 HTMLMediaElement
    // 内部缓冲越攒越多导致画面逐渐滞后（同样的低延迟取向见上面 hls 的
    // maxBufferLength 配置）。点播（live=false）则用默认配置——需要正常的
    // 预加载缓冲和 seek 支持，不能关 stash buffer。
    live ? { enableStashBuffer: false, liveBufferLatencyChasing: true } : {}
  )
  mpegtsPlayer = p
  p.attachMediaElement(vid)
  // 编码信息面板：mpegts.js 解析出媒体信息（分辨率/帧率/编码/码率……）就
  // 一次性全给了，不需要像 hls 的 Level 那样分好几个事件拼。
  p.on(MpegtsCls.Events.MEDIA_INFO, (mi: any) => {
    codecInfo.value.container = t === 'mpegts' ? 'MPEG-TS (裸流)' : 'FLV'
    if (mi.width) codecInfo.value.width = mi.width
    if (mi.height) codecInfo.value.height = mi.height
    if (mi.fps) codecInfo.value.fps = mi.fps
    if (mi.videoCodec) codecInfo.value.videoCodec = mi.videoCodec
    if (mi.audioCodec) codecInfo.value.audioCodec = mi.audioCodec
    // mpegts.js 的 videoDataRate/audioDataRate 延续 flv.js 的约定，单位是
    // kbps，跟 hls 那边算出来的 videoBitrateKbps 刻度一致，可以直接复用
    // 同一个字段、面板上不用分两套单位展示。
    if (mi.videoDataRate) codecInfo.value.videoBitrateKbps = Math.round(mi.videoDataRate)
    if (mi.audioDataRate) codecInfo.value.audioBitrateKbps = Math.round(mi.audioDataRate)
    if (mi.audioSampleRate) codecInfo.value.audioSampleRate = mi.audioSampleRate
    if (mi.audioChannelCount) codecInfo.value.audioChannels = mi.audioChannelCount
    // mpegts.js 的 duration 单位是毫秒，跟 codecInfo.durationSec（秒）对齐。
    if (!live && typeof mi.duration === 'number') codecInfo.value.durationSec = mi.duration / 1000
  })
  p.on(MpegtsCls.Events.ERROR, (errType: string) => {
    const isMediaErr = errType === MpegtsCls.ErrorTypes.MEDIA_ERROR
    // 容器/编码猜错了：翻一次类型重试（flv⇄mpegts），只翻一次——两种都
    // 播不了的坏线路不能在这两个类型之间来回切换变成死循环。
    if (isMediaErr && !mpegtsTypeFallbackTried) {
      mpegtsTypeFallbackTried = true
      const altType = t === 'flv' ? 'mpegts' : 'flv'
      app.showTip('尝试切换解封装格式…')
      p.destroy()
      if (mpegtsPlayer === p) mpegtsPlayer = null
      void initMpegtsPlayer(src, vid, altType, live)
      return
    }
    onMpegtsFatal()
  })
  p.load()
}

// mpegts 播放彻底失败（含当前浏览器不支持 MSE、翻过类型还是不行）：跟
// hls.js 的 fatal 错误走同一套「这个频道还有没试过的线路就换一条，没有
// 就交给 onPlayError 收尾」逻辑，两条解封装路径的收尾行为保持一致。
function onMpegtsFatal() {
  if (isLive) lineFatalTotal++
  failLineOrGiveUp('mediaError')
}

function bindVideoEvents(vid: HTMLVideoElement) {
  vid.onloadedmetadata = () => {
    if (isLive) {
      durMs.value = 0
    } else if (remuxMode.value) {
      // 转封装流从 seek 点起转，服务端只知剩余时长，补回起点才是整片时长
      durMs.value = (remuxSeek.value + vid.duration) * 1000
    } else {
      durMs.value = vid.duration * 1000
    }
    prepared.value = true
    buffering.value = false
    playing.value = true
    // 转封装流的时间轴从 seek 点重新计数，而外挂字幕的 cue 是整片绝对时间。
    // 不告诉字幕模块这个偏移，字幕会整体提前 remuxSeek 秒出现
    // （从第 10 分钟续播，本该 10:30 的台词会在 0:30 就冒出来）。
    setTimelineBase(remuxMode.value ? remuxSeek.value * 1000 : 0)
    popOsd()
    // 网盘续播。
    // 转封装模式：initPlayer 已经把起点塞给 remuxSeek，这条流本身就是
    //   从目标位置起转的，vid.currentTime 从 0 开始算，不需要再跳，
    //   这里只把进度条和提示补上。
    // 普通流：直接改 currentTime 即可。
    if (pendingResumeMs > 0 && !isLive) {
      const ms = pendingResumeMs
      pendingResumeMs = 0
      curMs.value = ms
      lastSaveMs = ms
      app.showTip('已从上次位置继续播放')
      // mpegts.js（点播 FLV/裸 TS）场景下，直接改 currentTime 会把刚起播的
      // <video> 内部置成暂停（不像原生 progressive 文件那样自动续播），
      // 补一次 play() 续上；对原生 <video>/hls.js 路径无害。
      if (!remuxMode.value) {
        vid.currentTime = ms / 1000
        vid.play().catch(() => {})
      }
    }
    if (introSkipSec.value > 0 && !introDone && !isLive) {
      const skipMs = introSkipSec.value * 1000
      if (durMs.value <= 0 || skipMs < durMs.value - 30000) showSkipBtn('intro', 5)
    }
  }
  // 裸 TS 容器（mpegts.js 点播）常常在 loadedmetadata 触发的那一刻
  // vid.duration 还是 Infinity——TS 本身没有 FLV onMetaData 那样的时长字段，
  // mpegts.js 要等把整个流/索引摸完才知道真实时长，之后才用
  // MediaSource.duration 改写，改写只会触发 durationchange，不会重新触发
  // loadedmetadata。只在 onloadedmetadata 里读一次的话，进度条时长会一直
  // 卡在 Infinity（显示成「Infinity:NaN:NaN」）。
  vid.ondurationchange = () => {
    if (isLive || remuxMode.value) return
    if (Number.isFinite(vid.duration) && vid.duration > 0) {
      durMs.value = vid.duration * 1000
    }
  }
  vid.ontimeupdate = () => {
    // 转封装模式下服务端从 seek 点起转，视频时间戳从 0 开始，
    // 这里把跳转起点加回去，进度条与时间显示才是真实位置。
    curMs.value = (remuxMode.value ? remuxSeek.value : 0) * 1000 + vid.currentTime * 1000
    // 进度保存：节流（5 秒）+ 防抖（300ms），保证最后一次 seek 后一定保存。
    // 用绝对差值判断，不能写成 curMs - lastSaveMs > 5000：
    // 往回拖进度时 curMs 会突然变小，差值变负，条件永远不成立，
    // 于是「看了十分钟再往回拖几分钟」这段完全不落盘，退出后就从头开始。
    if (Math.abs(curMs.value - lastSaveMs) > 5000) {
      lastSaveMs = curMs.value
      if (saveTimer) clearTimeout(saveTimer)
      saveTimer = window.setTimeout(() => { savePos(); saveTimer = 0 }, 300)
    }
    if (!isLive && outroSkipSec.value > 0 && durMs.value > 0 && !outroFired && curMs.value >= durMs.value - outroSkipSec.value * 1000) {
      outroFired = true
      showSkipBtn('outro', 8)
    }
  }
  vid.onwaiting = () => {
    buffering.value = true
    onBufferingStart()
  }
  vid.onplaying = () => {
    buffering.value = false
    playing.value = true
    hlsRecover = 0
    // 真正出过画面了：不是直接判这条线「没问题」，换成一个更短的宽限期
    // 看门狗，确认是不是真的站稳了（见 armLiveEarlyStallGrace 注释）。
    armLiveEarlyStallGrace()
    reportLiveStatus(true)
    onBufferingEnd()
  }
  vid.onpause = () => {
    playing.value = false
    buffering.value = false
    // 用户主动暂停不算网络卡顿，清掉计时但不计入卡顿累计
    waitingSince = 0
  }
  vid.onended = () => {
    playing.value = false
    if (player.hasNext && !isLive && autoPlayNext.value) {
      // 自动连播：3 秒倒计时后切下一集
      let countdown = 3
      hint.value = `即将播放下一集（${countdown}s）`
      if (autoplayTimer) clearInterval(autoplayTimer)
      autoplayTimer = window.setInterval(() => {
        countdown--
        if (countdown > 0) {
          hint.value = `即将播放下一集（${countdown}s）`
        } else {
          // 必须先 clearInterval 再置 0：只把 id 清掉而不停表的话，
          // 一旦下面的 switchEpisode 走不到 destroyPlayer（比如
          // player.count 为 0 时提前 return），这个 interval 会一直
          // 每秒跑下去，反复调 switchEpisode 并重复弹提示。
          clearInterval(autoplayTimer)
          autoplayTimer = 0
          hint.value = ''
          switchEpisode(1)
        }
      }, 1000)
    } else {
      showOsd.value = true
      if (!player.hasNext && !isLive) {
        app.showTip('播放列表已全部完成')
      }
    }
  }
  vid.onerror = () => {
    // 区分真正的解码失败与「上游返回了错误页」：
    // <video> 对 401/403/404 的响应体同样会抛 MEDIA_ERR_SRC_NOT_SUPPORTED，
    // 一律说成「解码失败」会把人往编码格式上带偏（实际是认证或地址问题）。
    const code = vid.error?.code
    const msg = vid.error?.message || ''
    // 直播下这是原生 <video> 自己报的错（跟 hls.js/mpegts.js 各自的 ERROR
    // 回调是两条独立路径）：地址里没有 .m3u8 字样、被猜成 FLV/TS 容器交给
    // mpegts.js 的直播线路（常见于「xxx.php?id=yyy」这类 IPTV 网关地址），
    // mpegts.js 把非法内容解析失败后关闭 MediaSource，<video> 元素自己会
    // 报一个原生 error——以前这里直接 onPlayError，跳过了「同一频道还有
    // 没试过的线路就先换一条」这一层，表现成「只试了一条线路就瞬间判定
    // 全部线路都不能播」，41 条备用线路里剩下的全部没机会被尝试。
    // 统一走 failLineOrGiveUp，和 hls fatal/mpegts fatal 两条路径保持一致。
    if (code === MediaError.MEDIA_ERR_NETWORK) {
      failLineOrGiveUp('networkError')
      return
    }
    if (code === MediaError.MEDIA_ERR_DECODE) {
      failLineOrGiveUp('mediaError')
      return
    }
    // MEDIA_ERR_SRC_NOT_SUPPORTED：地址不通/需要认证/格式不支持，三种都可能
    failLineOrGiveUp(/401|403/.test(msg) ? 'authError' : 'srcNotSupported')
  }
  vid.onenterpictureinpicture = () => {
    isPip.value = true
    app.showTip('已进入画中画，切换到其它页面也会继续播放', 2500)
  }
  vid.onleavepictureinpicture = () => {
    isPip.value = false
    // 已经离开播放页、靠画中画续播的会话：画中画一关就没有任何画面还在
    // 展示它了，收尾（保存进度、销毁 hls/mpegts、从常驻容器摘除），同时
    // 带用户跳回这部剧/频道的播放页（restore: true）——否则浏览器原生
    // 画中画窗口的「返回标签页」就只是把人带回当前随便哪个页面，视频却
    // 已经没了，表现跟误触关闭没有区别。
    // 仍在播放页内关画中画（组件还挂载着）只是切回页面内正常播放，
    // ghosted 为 false，不触发这里。
    if (ghosted && ghostHandle) {
      clearCurrentGhost(ghostHandle)
      ghostHandle.stop({ restore: true })
    }
  }
}

function onPlayError(errKind?: string) {
  if (isLive) {
    // 走到这说明这个频道能试的线路（HLS 播放时在 trySwitchToUntriedLine
    // 里逐条试过 ep.urls）都已经失败：不自动跳到下一个频道——那样用户会
    // 被悄悄带到一个自己没选的频道，还以为换台是自己手滑点到的。退回
    // 频道选择页，用一条提示说清楚原因。
    reportLiveStatus(false)
    // 不少"央视类"频道的几十条备用线路其实都是同一路原始信号的转发/
    // 镜像，编码都一样——如果源本身是 HEVC/H.265，所有线路会在清单解析阶段
    // 就判定编码不兼容，不需要真的等网络超时，表现成「瞬间全部线路报废」。
    // 跟真正的源失效区分开说清楚，省得用户以为软件坏了、每条线路都没真的试。
    const msg = codecIncompatibleSeen
      ? '该频道所有线路均播放失败，常见原因：源是 HEVC/H.265 编码，当前浏览器/设备不支持硬解码'
      : '该频道所有线路均无法播放'
    app.showTip(msg, codecIncompatibleSeen ? 5000 : 3000)
    goBack()
    return
  }
  // 转封装兜底：网盘里很多文件没有扩展名（如「异星战境 Atlas」），
  // needsRemux() 只能判「有扩展名且在不支持列表里」，无扩展名时一律交给
  // 浏览器先试。试了播不了如果再直接报错，就等于把服务端 ffmpeg 白放着 ——
  // 这类文件多半是 MKV/AVI，转封装就能播。
  // 注意只兜底一次（remuxFallbackTried），避免「转封装也失败 → 再兜底」死循环。
  // 网络/认证类错误不兜底：那不是容器问题，转封装也救不回来。
  const isContainerish = errKind === 'srcNotSupported' || errKind === 'mediaError' || errKind === undefined
  if (!remuxMode.value && !remuxFallbackTried && app.status?.remux && isContainerish && finalUrl) {
    remuxFallbackTried = true
    app.showTip('尝试转封装后播放…')
    // 走一次真正的转封装加载：remuxSeek 归零，从 0 起转
    remuxSeek.value = 0
    errMsg.value = ''
    prepared.value = false
    buffering.value = true
    const vid = videoEl.value
    if (vid) {
      // FLV/TS 文件走到这说明 mpegts.js 也解不动（多半编码不是 H.264/AAC），
      // 旧的 mpegtsPlayer 实例还挂在这个 video 元素上，必须先销毁再直接改
      // src——否则它会继续跟下面新设的 src 抢同一个 <video>/MSE。
      if (mpegtsPlayer) {
        mpegtsPlayer.destroy()
        mpegtsPlayer = null
      }
      // 关键：传 true。无扩展名文件 needsRemux() 本来就返回 false，
      // 不强制的话这里又会拿回原来的直连地址，等于原地重试一遍同样的失败。
      const src = proxiedFinal(true)
      codecInfo.value = blankCodecInfo()
      codecInfo.value.via = via.value
      codecInfo.value.url = finalUrl
      codecInfo.value.isLive = isLive
      codecInfo.value.container = '转封装 (服务端 ffmpeg → fMP4)'
      vid.src = src
      vid.play().catch(() => {})
    }
    return
  }
  // 按错误类型给出友好提示
  // 容器不支持（MKV 等）且服务端没有 ffmpeg：给出可操作的指引，
  // 而不是让人以为文件坏了。这是网盘播放最常见的失败原因。
  // 注意不含 flv/ts/m2ts：这三种走 mpegts.js 客户端解封装，不依赖服务端
  // ffmpeg，播不了大概率是编码本身的问题，走下面按 errKind 的提示即可。
  const isContainerIssue = needsRemux(finalUrl) || /\.(mkv|avi|wmv|rmvb|rm|mov|mpg|mpeg|vob)\b/i.test(finalUrl)
  if (isContainerIssue && !app.status?.remux) {
    errMsg.value =
      (app.status?.remuxReason || '服务端缺少 ffmpeg，无法转换该视频容器') +
      '。装好后重启服务即可播放，画质不受影响（只换容器不重新编码）。'
    // buffering 的模板优先级高于 errMsg（v-else-if 链），不清掉的话
    // 清单/分片从没加载成功过、buffering 从没被 canplay 复位过，错误提示
    // 会被「缓冲中…」永远挡住，表现成卡死不动。
    buffering.value = false
    showOsd.value = true
    return
  }

  const tips: Record<string, string> = {
    networkError: '网络连接失败，请检查网络后点击重试',
    mediaError: '视频解码失败，可能是编码格式不支持',
    manifestLoadError: '视频地址失效或已下线',
    levelLoadError: '视频分片加载失败，源站可能不稳定',
    fragLoadError: '视频分片加载中断，可能是网络波动',
    manifestIncompatibleCodecsError: '编码格式不支持当前浏览器',
    authError: '资源需要认证或没有访问权限，请检查网盘账号密码',
    srcNotSupported: needsRemux(finalUrl) || remuxMode.value
      ? '该容器转换失败。可能是源不是 H.264/AAC（如 HEVC/AC3），无法无损转封装'
      : '这个文件无法直接播放。可能是编码格式浏览器不支持，或地址已失效',
  }
  errMsg.value = tips[errKind || ''] || '播放失败，可能是链接失效或编码不支持'
  // 已经走过转封装兜底还失败：说明问题不在容器，而在编码本身。
  // 这时候再说「无法直接播放」会误导 —— 用户会以为文件坏了或者播放器没试，
  // 实际是 HEVC/AC3 这类转封装救不了的编码。
  if (remuxFallbackTried && remuxMode.value) {
    errMsg.value =
      '这个文件的编码浏览器不支持（常见于 HEVC/H.265、AC3 音轨）。' +
      '转封装只换容器不转码，救不了这种情况。换一个 H.264/AAC 的片源试试。'
  }
  // 同上：buffering 不清掉的话 errMsg 会被模板里优先级更高的「缓冲中…」挡住。
  buffering.value = false
  showOsd.value = true
}

function applyInitialLevel() {
  if (!hls || !levels.value.length) return
  if (qCap > 0) {
    const i = pickByCap(qCap, levels.value)
    levelIdx = i
    // 用户明确指定了档位：用 currentLevel 锁定，从第一个分片就按它加载
    hls.currentLevel = i
  } else {
    // 自动：只设置「起始档位」，不能写 currentLevel ——
    // currentLevel >= 0 等于手动锁档，hls.js 的 ABR 会彻底停摆，
    // 网速再好也不会自动升到 1080P，用户选了「自动」却永远停在 720P。
    // startLevel 只在起播时用一次，之后由 ABR 按带宽自由升降。
    let best = -1
    levels.value.forEach((l, i) => {
      if (l.height > 0 && l.height <= 720) best = i
    })
    if (best >= 0) hls.startLevel = best
    else hls.startLevel = -1
    // 实际档位交给 LEVEL_SWITCHED 回写，这里先按起始档位显示
    liveLevelIdx = best
  }
  syncQLabel()
}

function pickByCap(cap: number, lv: QLevel[]): number {
  if (!lv.length) return -1
  if (cap >= TOP) return lv.length - 1
  let best = -1
  lv.forEach((l, i) => {
    if (l.height <= 0 || l.height <= cap) best = i
  })
  return best < 0 ? 0 : best
}

/**
 * 画质按钮文案。
 *
 * 手动选了档位 → 直接显示那一档（原画 / 1080P / 720P…）。
 * 自动模式 → 显示 hls.js 此刻实际在播的档位（来自 LEVEL_SWITCHED），
 *   因为自动档位会随带宽上下浮动，用户想知道「我现在到底在看几 P」。
 *   一个档位都没有（单码率源/非 HLS）时才退回「自动」。
 * 不拼「自动 1080P」这种长串 —— 移动端按钮宽度有限会被挤变形，
 * 「自动」这层语义放到 title 里。
 */
function syncQLabel() {
  // 手动选了档位：优先按真实档位表取名，取不到（单码率源/非 HLS）
  // 就按用户选的分辨率直接显示。这条要在「只有一个档位」判断之前 ——
  // 否则单码率源上用户选了「原画」，按钮还固执地显示「自动」。
  if (qCap > 0) {
    const i = levelIdx >= 0 && levelIdx < levels.value.length
      ? levelIdx
      : pickByCap(qCap, levels.value)
    // TOP 是个哨兵值（代表「原画/最高」），不能直接喂给 labelOf ——
    // labelOf(100000) 会命中「高度 >= 2160 → 4K」，显示成错误的 4K。
    let name: string
    if (i >= 0 && i < levels.value.length) name = labelOfLevel(levels.value[i])
    else if (qCap >= TOP) name = '原画'
    else name = labelOf(qCap, 0)
    qLabel.value = name
    qTitle.value = `当前画质：${name}（手动）`
    return
  }
  if (levels.value.length <= 1) {
    qLabel.value = '自动'
    qTitle.value = '当前画质：自动'
    return
  }
  // 自动：显示 hls.js 此刻实际在播的档位（LEVEL_SWITCHED 回写），
  // 因为自动档位会随带宽上下浮动，用户想知道「我现在到底在看几 P」。
  if (liveLevelIdx >= 0 && liveLevelIdx < levels.value.length) {
    const name = labelOfLevel(levels.value[liveLevelIdx])
    qLabel.value = name
    qTitle.value = `当前画质：自动 · 正在播 ${name}`
    return
  }
  qLabel.value = '自动'
  qTitle.value = '当前画质：自动'
}

function chooseLevel(cap: number) {
  qCap = cap
  api.setPrefs({ qPref: cap }).catch(() => {})
  showQ.value = false
  if (hls && levels.value.length > 1) {
    if (cap === 0) {
      // 回到自动跟随
      hls.currentLevel = -1
      levelIdx = -1
    } else {
      const next = pickByCap(cap, levels.value)
      levelIdx = next
      // 用 nextLevel 而不是 currentLevel：后者会立即丢弃当前缓冲区、
      // 从头重新加载，画面会黑一下并停顿好几秒；前者在下一个分片边界
      // 平滑切换，观看几乎无感。已经在目标档位就不用动。
      if (next !== hls.currentLevel) hls.nextLevel = next
    }
  }
  syncQLabel()
}

// 画面比例
// 默认用「等比适应（留黑边）」而不是「拉伸铺满」：
// fill 会把 16:9 的视频硬压进手机竖屏容器（390×844），画面严重变形；
// 部分移动端 GPU 在这种拉伸 + 硬件解码的组合下还会出现色块/花屏，
// 看起来就是「画面闪成一块块纯色」。等比留黑边是最不容易出问题的默认值。
const scaleMode = ref(2)
const scaleFit = computed(() => {
  if (scaleMode.value === 0) return 'fill'
  if (scaleMode.value === 1) return 'cover'
  return 'contain'
})

const scaleNames = ['拉伸铺满', '裁剪铺满（等比不变形）', '等比适应（留黑边）']

function setScale(mode: number) {
  scaleMode.value = mode
  app.showTip('画面比例：' + scaleNames[mode])
}

// 跳过片头/片尾档位设置
const SKIP_PRESETS = [0, 15, 30, 60, 90]

/**
 * 保存当前这部剧的片头片尾设置。
 *
 * 只写本剧，不动全局默认值 —— 这正是这次要修的核心：
 * 原来写 api.setPrefs({introSkip}) 会落到全局，等于改一部剧影响所有剧。
 * skipGlobal=true 时才额外把这套值设为全局默认（「应用到所有剧」）。
 */
function saveSkipCfg(opts: { intro?: number; outro?: number; global?: boolean } = {}) {
  const intro = opts.intro ?? introSkipSec.value
  const outro = opts.outro ?? outroSkipSec.value
  introSkipSec.value = intro
  outroSkipSec.value = outro
  const a = skipScopeA.value
  const b = skipScopeB.value
  if (!a || !b) {
    // 没有剧标识（直链播放）：只能落到全局默认
    api.setPrefs({ introSkip: intro, outroSkip: outro }).catch(() => {})
    return
  }
  const payload: Record<string, number | string> = { introSkip: intro, outroSkip: outro }
  if (opts.global) payload.skipGlobal = 1
  api
    .setPrefs(payload, { skipA: a, skipB: b })
    .then(() => {
      // 两个都归零时服务端会删掉本剧记录、回落到全局默认
      skipOwn.value = !(intro === 0 && outro === 0)
      if (opts.global) {
        globalIntroSkip.value = intro
        globalOutroSkip.value = outro
      }
    })
    .catch(() => {})
}

function setIntroSkip(sec: number) {
  saveSkipCfg({ intro: sec })
  if (sec > 0) app.showTip(`本剧片头跳过 ${fmtSec(sec)}（只对这部剧生效）`)
  else app.showTip('已关闭本剧的片头跳过')
}

function setOutroSkip(sec: number) {
  saveSkipCfg({ outro: sec })
  if (sec > 0) app.showTip(`本剧片尾跳过 ${fmtSec(sec)}（只对这部剧生效）`)
  else app.showTip('已关闭本剧的片尾跳过')
}

/** 把当前这部剧的设置设为「所有剧的默认值」 */
function applySkipToAll() {
  if (introSkipSec.value <= 0 && outroSkipSec.value <= 0) {
    app.showTip('本剧还没设置片头片尾，先标记或选一个时长')
    return
  }
  saveSkipCfg({ global: true })
  app.showTip('已设为所有剧的默认值（已单独设置过的剧不受影响）')
}

/** 清掉本剧的单独设置，回落到全局默认 */
function resetSkipCfg() {
  const a = skipScopeA.value
  const b = skipScopeB.value
  if (a && b) api.clearSkipCfg(a, b).catch(() => {})
  introSkipSec.value = globalIntroSkip.value
  outroSkipSec.value = globalOutroSkip.value
  skipOwn.value = false
  app.showTip(
    globalIntroSkip.value > 0 || globalOutroSkip.value > 0
      ? `已恢复默认（片头 ${fmtSec(globalIntroSkip.value)} / 片尾 ${fmtSec(globalOutroSkip.value)}）`
      : '已恢复默认（不跳过）'
  )
}

// 标当前：片头=当前播放位置，片尾=距结尾距离
function markSkip(kind: 'intro' | 'outro') {
  const vid = videoEl.value
  if (!vid || !prepared.value || isLive) return
  if (kind === 'intro') {
    const t = Math.floor(vid.currentTime)
    if (t <= 0) {
      app.showTip('当前位置是片头起点，无法标记')
      return
    }
    saveSkipCfg({ intro: t })
    app.showTip(`已标记本剧片头结束点 ${fmtSec(t)}，下次起播跳到这里`)
  } else {
    if (durMs.value <= 0) return
    const remain = Math.floor((durMs.value - vid.currentTime * 1000) / 1000)
    if (remain <= 0) {
      app.showTip('已到片尾，无法标记')
      return
    }
    saveSkipCfg({ outro: remain })
    app.showTip(`已标记本剧片尾起点（距结尾 ${fmtSec(remain)}），播到这里自动切`)
  }
}

// ---------- 播放控制 ----------

function togglePlay() {
  const vid = videoEl.value
  if (!vid || !prepared.value) return
  // 用户主动操作播放，先把连播倒计时撤掉
  cancelAutoplay()
  if (vid.paused) {
    vid.play()
    flashPlay('play')
    popOsd()
  } else {
    vid.pause()
    flashPlay('pause')
    showOsd.value = true
  }
}

// 点画面：控制条收着时先唤出，再点才切播放/暂停。
function onSurfaceClick() {
  if (!showOsd.value) {
    popOsd()
    return
  }
  togglePlay()
}

function onMouseMove(e: MouseEvent) {
  // 鼠标贴到屏幕最左/最右边缘：主动收起界面（常见于想单纯看画面、
  // 或者鼠标要移去碰系统任务栏/其它窗口时不想一直被控制条挡着）。
  // 优先判断边缘，命中就直接收起，不执行下面"挪了鼠标就唤出"那套。
  const el = playerEl.value
  if (el && prepared.value) {
    const rect = el.getBoundingClientRect()
    const x = e.clientX - rect.left
    if (x <= EDGE_HIDE_PX || x >= rect.width - EDGE_HIDE_PX) {
      hideUI()
      return
    }
  }
  cursorHidden.value = false
  if (!showOsd.value && prepared.value) popOsd()
}

// ---------- 进度条拖动定位 ----------

const dragging = ref(false)
const previewMs = ref(-1)
// 进度条 DOM 引用：拖拽时用它算比例（window 监听拿不到 currentTarget）。
//
// 注意：移动端模板里进度条可能出现两处（全屏浮动 OSD 一条 + 视频下方面板一条），
// 桌面端也有一处。所有兄弟元素共用一个 ref 时，后挂载的会覆盖先挂载的，
// pointerdown 时如果取到的是另一条（可能宽高/位置完全不同，甚至已被 v-if 移除），
// 比例就算歪了 —— 表现为「拖着没反应」或「下面那条不动」。
// 因此这里不在渲染期收集元素，而是在 pointerdown 时用 currentTarget 就地锁定，
// 拖动全程只认这一个基准，彻底摆脱「谁最后挂载」的不确定性。
const dragBarEl = ref<HTMLElement | null>(null)
// ?debug=1 打开拖拽诊断日志
const DEBUG = route.query.debug === '1'

function ratioFromEvent(e: PointerEvent): number {
  const el = e.currentTarget as HTMLElement
  const rect = el.getBoundingClientRect()
  return Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width))
}

function onProgressDown(e: PointerEvent) {
  if (DEBUG) console.log('[seek] down', { prepared: prepared.value, dur: durMs.value, live: isLive })
  if (!prepared.value || durMs.value <= 0 || isLive) return
  e.preventDefault()
  dragging.value = true
  // 锁定本次拖动所用的进度条（currentTarget 一定是用户真正按下的那一条）
  dragBarEl.value = e.currentTarget as HTMLElement
  previewMs.value = durMs.value * ratioFromEvent(e)

  // move/up 绑到 window 而不是进度条自己：
  // 进度条只有 ~26px 高，拖快一点指针就跑出去了。虽然 setPointerCapture
  // 理论上能兜住，但元素被重渲染、或在某些浏览器里捕获失败时，
  // 一旦移出元素就再也收不到 pointermove/pointerup，表现为「拖不动」。
  // 绑 window 后无论指针移到哪里都能跟住，这是最稳的做法。
  window.addEventListener('pointermove', onProgressMove, { passive: true })
  window.addEventListener('pointerup', onProgressUp, { passive: true })
  window.addEventListener('pointercancel', onProgressCancel, { passive: true })
}

function ratioFromPoint(clientX: number, el: HTMLElement): number {
  const rect = el.getBoundingClientRect()
  if (rect.width <= 0) return 0
  return Math.min(1, Math.max(0, (clientX - rect.left) / rect.width))
}

function onProgressMove(e: PointerEvent) {
  if (!dragging.value) return
  const el = dragBarEl.value
  if (!el) return
  previewMs.value = durMs.value * ratioFromPoint(e.clientX, el)
}

function onProgressCancel() {
  // 被系统手势/来电等打断：撤销拖动，不改播放位置
  if (!dragging.value) return
  dragging.value = false
  previewMs.value = -1
  detachProgressListeners()
}

function detachProgressListeners() {
  window.removeEventListener('pointermove', onProgressMove)
  window.removeEventListener('pointerup', onProgressUp)
  window.removeEventListener('pointercancel', onProgressCancel)
}

function onProgressUp(e: PointerEvent) {
  if (DEBUG) console.log('[seek] up', { dragging: dragging.value, preview: previewMs.value })
  if (!dragging.value) return
  dragging.value = false
  detachProgressListeners()

  const t = previewMs.value / 1000
  if (videoEl.value && t >= 0 && durMs.value > 0) {
    // 统一走 applySeek：普通流直接改 currentTime，转封装流要带 seek 重新起转。
    applySeek(t)
    curMs.value = previewMs.value

    // 三层防抖：只在 5s 间隔后才真正落盘进度记录
    clearTimeout(seekPendingTimer)
    const now = Date.now()
    const timeSinceLastSeek = now - lastSeekMs

    if (timeSinceLastSeek >= SEEK_THROTTLE) {
      // 距上次 seek 已超过 5s，直接落盘
      lastSeekMs = now
      savePos()
    } else {
      // 在 5s 范围内，等待 300ms 后若无新 seek 才落盘
      seekPendingTimer = window.setTimeout(() => {
        const elapsed = Date.now() - lastSeekMs
        if (elapsed >= SEEK_THROTTLE) {
          lastSeekMs = Date.now()
          savePos()
        }
      }, SEEK_DEBOUNCE)
    }
  }
  const target = fmtMs(previewMs.value)
  previewMs.value = -1
  app.showTip('已定位到 ' + target)
  popOsd()
}

// ---------- 音量滑块 ----------

const volDragging = ref(false)
// 音量条 DOM：拖动中要拿它算比例。move 事件现在绑在 window 上，
// event.currentTarget 是 window 而不是音量条，所以必须单独记住元素。
let volBarEl: HTMLElement | null = null
let volSaveTimer = 0

// 音量变化后防抖落盘，避免拖动时每帧都请求后端
function persistVolume() {
  if (volSaveTimer) clearTimeout(volSaveTimer)
  volSaveTimer = window.setTimeout(() => {
    api.setPrefs({ volumePref: Math.round(volume.value * 100) / 100 }).catch(() => {})
  }, 500)
}

function setVolumeFromEvent(e: PointerEvent) {
  // 优先用拖动时锁定的音量条；没有（非拖动的单次点击）再退回 currentTarget
  const el = volBarEl || (e.currentTarget as HTMLElement | null)
  if (!el || typeof el.getBoundingClientRect !== 'function') return
  const rect = el.getBoundingClientRect()
  if (rect.width <= 0) return
  let r = (e.clientX - rect.left) / rect.width
  r = Math.min(1, Math.max(0, r))
  volume.value = r
  if (r > 0) muted.value = false
  if (videoEl.value) videoEl.value.volume = muted.value ? 0 : r
  persistVolume()
  showVol.value = true
  if (volTimer) clearTimeout(volTimer)
  volTimer = window.setTimeout(() => (showVol.value = false), 1600)
}

function onVolDown(e: PointerEvent) {
  e.preventDefault()
  volDragging.value = true
  volBarEl = e.currentTarget as HTMLElement
  setVolumeFromEvent(e)
  // 与进度条一致：move/up 绑 window 而不是元素自己。
  // 只靠 setPointerCapture 时，元素一被重渲染/Vue 重排就会丢事件，
  // 表现为「拖着拖着音量就不跟手了」，松手也收不到 pointerup。
  window.addEventListener('pointermove', onVolMove, { passive: true })
  window.addEventListener('pointerup', onVolUp, { passive: true })
  window.addEventListener('pointercancel', onVolUp, { passive: true })
}

function onVolMove(e: PointerEvent) {
  if (!volDragging.value || !volBarEl) return
  setVolumeFromEvent(e)
}

function onVolUp() {
  volDragging.value = false
  volBarEl = null
  window.removeEventListener('pointermove', onVolMove)
  window.removeEventListener('pointerup', onVolUp)
  window.removeEventListener('pointercancel', onVolUp)
}

function seekBy(dir: number) {
  const vid = videoEl.value
  if (!vid || !prepared.value || isLive) return
  // 转封装模式下 vid.currentTime 是从 seek 起点算起的相对时间，
  // 必须加上 remuxSeek 偏移才是整片里的绝对位置，否则前后跳会越跳越偏。
  const base = remuxMode.value ? remuxSeek.value : 0
  let t = base + vid.currentTime + (dir * SEEK_STEP) / 1000
  if (t < 0) t = 0
  if (durMs.value > 0 && t > durMs.value / 1000 - 1) t = durMs.value / 1000 - 1
  applySeek(t)
}

/**
 * 执行一次跳转。
 *
 * 转封装流（/api/remux）是服务端线性吐出来的分片 MP4，体积索引不完整，
 * 直接改 video.currentTime 浏览器会原地重试甚至报错。这里改为带 seek 参数
 * 重新请求，让 ffmpeg 从目标位置重新转封装。
 */
let seekingRemux = false

function applySeek(t: number) {
  const vid = videoEl.value
  if (!vid) return
  if (remuxMode.value) {
    // 防重入：重新设置 src 会再触发 loadedmetadata，
    // 若此时还有未消化的 player.seek 就会反复重载。
    if (seekingRemux) return
    seekingRemux = true
    window.setTimeout(() => (seekingRemux = false), 1500)
    remuxSeek.value = Math.max(0, Math.floor(t))
    const wasPlaying = playing.value
    // 把兜底状态带上：无扩展名文件已经靠强制转封装在播了，
    // 这里如果不传就会拿回直连地址，一拖进度条又变回「播不了」。
    const src = proxiedFinal(remuxFallbackTried)
    vid.src = src
    curMs.value = t * 1000
    vid.addEventListener('loadedmetadata', () => {
      if (wasPlaying) vid.play().catch(() => {})
    }, { once: true })
    popOsd()
    app.showTip('正在跳转…')
    return
  }
  const wasPlaying = playing.value
  vid.currentTime = t
  curMs.value = t * 1000
  popOsd()
  // mpegts.js（点播 FLV/裸 TS）seek 时会把 <video> 内部置成暂停，不会像
  // 原生 progressive 文件那样自动续播在原地卡住；这里补一次 play() 续上。
  // 对原生 <video>/hls.js 路径无害——本来就在播，play() 是空操作。
  if (wasPlaying) vid.play().catch(() => {})
}

// ===== 倍速播放 =====

function setPlaybackRate(rate: number) {
  const vid = videoEl.value
  if (!vid || !prepared.value) return
  if (!PLAYBACK_RATES.includes(rate)) return
  playbackRate.value = rate
  vid.playbackRate = rate
  // 同步到 HLS 配置（防止切换线路时重置）
  if (hls) hls.config.playbackRate = rate
  // 持久化倍速偏好
  api.setPrefs({ speedPref: rate }).catch(() => {})
  app.showTip(`${rate}x 倍速播放`)
}

// 循环切换倍速（0.5x → 0.75x → 1x → 1.25x → 1.5x → 2x → 0.5x）
function togglePlaySpeed() {
  const idx = PLAYBACK_RATES.indexOf(playbackRate.value)
  const nextIdx = (idx + 1) % PLAYBACK_RATES.length
  setPlaybackRate(PLAYBACK_RATES[nextIdx])
}

// ===== 移动端手势（usePlayerGestures 组合式函数）=====
const { gestureHint, brightness, resetBrightness, onTouchStart, onTouchMove, onTouchEnd, disposeGestures } =
  usePlayerGestures({
    videoEl,
    volume,
    muted,
    curMs,
    durMs,
    showOsd,
    isLive: () => isLive,
    seekBy,
    applySeek,
    togglePlay,
    flashPlay,
    popOsd,
    persistVolume,
    tip: (m: string) => app.showTip(m)
  })

function seekToRatio(ratio: number) {
  const vid = videoEl.value
  if (!vid || !prepared.value || durMs.value <= 0) return
  const r = Math.min(1, Math.max(0, ratio))
  applySeek((durMs.value * r) / 1000)
}

function changeVolume(delta: number) {
  let v = volume.value + delta
  v = Math.min(1, Math.max(0, v))
  volume.value = v
  if (v > 0) muted.value = false
  if (videoEl.value) videoEl.value.volume = muted.value ? 0 : v
  persistVolume()
  showVol.value = true
  if (volTimer) clearTimeout(volTimer)
  volTimer = window.setTimeout(() => (showVol.value = false), 1600)
}

function toggleMute() {
  muted.value = !muted.value
  if (videoEl.value) videoEl.value.volume = muted.value ? 0 : volume.value
  showVol.value = true
  if (volTimer) clearTimeout(volTimer)
  volTimer = window.setTimeout(() => (showVol.value = false), 1600)
}

// ===== 选集抽屉 =====
const showEps = ref(false)
let autoplayTimer = 0
let castHandlers: { onConnect: () => void; onDisconnect: () => void } | null = null

/**
 * 取消「即将播放下一集」的倒计时。
 *
 * 播完后会起一个 3 秒倒计时，但原来没有任何地方能取消它：
 * 用户在倒计时期间点了暂停、切了别的集、或关掉自动连播，
 * 3 秒到了照样会被切走。所有「用户主动干预播放」的入口都要调这个。
 */
function cancelAutoplay() {
  if (autoplayTimer) {
    clearInterval(autoplayTimer)
    autoplayTimer = 0
  }
  // 提示也要一起清，否则倒计时残留的「（1s）」会跟着切集留到下一集
  if (hint.value.includes('即将播放下一集')) hint.value = ''
}

/**
 * 切换自动连播。关掉的时候要把正在跑的倒计时立刻撤掉 ——
 * 否则用户点了「手动切集」，3 秒后还是被切走，等于开关没生效。
 */
function toggleAutoPlayNext() {
  autoPlayNext.value = !autoPlayNext.value
  if (!autoPlayNext.value) {
    cancelAutoplay()
    app.showTip('已改为手动切集')
  } else {
    app.showTip('播完自动播放下一集')
  }
}

async function jumpToEpisode(i: number) {
  // 先关抽屉再切集：switchEpisode 里要解析地址、重新建流，可能卡几秒。
  // 原来放在 await 之后，抽屉会一直开着，用户以为没点中，容易连点好几下。
  showEps.value = false
  if (i === player.idx) return
  await switchEpisode(i - player.idx)
}

async function switchEpisode(delta: number) {
  if (!player.count) {
    app.showTip(isLive ? '当前分组没有其他频道' : '当前没有可切换的剧集')
    return
  }
  savePos()
  // 清掉连播倒计时与它的提示，避免切集后还残留「即将播放下一集（1s）」
  cancelAutoplay()
  // 清除进度条拖动状态，避免切集/切台后进度条显示混乱
  dragging.value = false
  previewMs.value = -1
  const ep = player.step(delta)
  if (!ep) {
    app.showTip(delta > 0 ? '已经是最后一个了' : '已经是第一个了')
    return
  }
  destroyPlayer()
  // 上一集的外挂 <track> 必须从 video 上摘掉：destroyPlayer 只是改了 src，
  // <track> 是挂在 video 元素上的子节点，不会跟着换源消失。
  // 不摘的话下一集会继续显示上一集的字幕（外挂字幕常见跨集通用，
  // 所以内容要留着，等新流就绪后 reattachExternalSub 再挂回去）。
  detachExternalSub()
  curMs.value = 0
  durMs.value = 0
  prepared.value = false
  playing.value = false
  // 切集/换源必须把转封装偏移和保存节流基线一起归零：
  // 否则上一集如果拖到 10 分钟再切下一集，下一集会从第 10 分钟起播；
  // lastSaveMs 留着旧值也会让新一集的前 5 秒不落盘。
  remuxSeek.value = 0
  pendingResumeMs = 0
  lastSaveMs = 0
  // 亮度遮罩也要复位：上一集被手势压暗后，下一集会带着同一层黑开场，
  // 用户会以为新一集画面发灰/变色。
  resetBrightness()
  introDone = false
  outroFired = false
  skipShow.value = ''
  rawUrl = ep.url
  triedLines = new Set()
  codecIncompatibleSeen = false
  hlsRecover = 0
  lineFatalTotal = 0
  mpegtsTypeFallbackTried = false
  liveStatusReported = false
  // 换台是「新频道」：自动选线开关若在上一个频道里被关掉排查问题，
  // 不该悄悄带到下一个频道继续生效——那样会表现成「这个频道也瞬间全部
  // 线路失败」，看起来像是新 bug，实际只是开关忘了开回去。
  autoLineSwitch.value = true
  if (isLive) {
    title.value = ep.name
    app.showTip('换台：' + ep.name)
  } else {
    title.value = player.vodTitle ? `${player.vodTitle} ${ep.name}` : ep.name
    app.showTip(`第 ${player.idx + 1} 集  ${ep.name}`)
  }
  syncEpLabel()
  resolving.value = true
  await safeResolve()
  initPlayer()
  startLineQualityWatch()
}

/**
 * 这部剧/这个频道「本该」返回去的那个页面——点播是详情页，直播是频道
 * 列表，网盘是对应源的续播入口。只在 cameFromPipRestore 时使用（见那里
 * 的注释），平时「返回」按钮走的是更贴合用户真实来源的 router.back()。
 */
function buildParentPath(): string {
  if (isLive) return '/live'
  if (siteKey === 'webdav') {
    if (player.wdSrcId) {
      const q = new URLSearchParams({ continue: `${player.wdSrcId}:${player.wdVod}` })
      return `/webdav?${q.toString()}`
    }
    return '/webdav'
  }
  if (siteKey && vodId) {
    const q = new URLSearchParams({ site: siteKey, id: vodId })
    if (vodSiteName) q.set('siteName', vodSiteName)
    return `/detail?${q.toString()}`
  }
  return ''
}

function goBack() {
  savePos()
  if (cameFromPipRestore) {
    const target = buildParentPath()
    if (target) {
      router.replace(target)
      return
    }
  }
  if (window.history.length > 1) {
    router.back()
  } else {
    router.replace('/')
  }
}

// 手动切换直播线路（同一频道的多线路）
async function switchLine(url: string, auto = false) {
  if (url === rawUrl) {
    if (!auto) app.showTip('已经是当前线路')
    return
  }
  destroyPlayer()
  // 上一集的外挂 <track> 必须从 video 上摘掉：destroyPlayer 只是改了 src，
  // <track> 是挂在 video 元素上的子节点，不会跟着换源消失。
  // 不摘的话下一集会继续显示上一集的字幕（外挂字幕常见跨集通用，
  // 所以内容要留着，等新流就绪后 reattachExternalSub 再挂回去）。
  detachExternalSub()
  curMs.value = 0
  durMs.value = 0
  prepared.value = false
  playing.value = false
  // 切集/换源必须把转封装偏移和保存节流基线一起归零：
  // 否则上一集如果拖到 10 分钟再切下一集，下一集会从第 10 分钟起播；
  // lastSaveMs 留着旧值也会让新一集的前 5 秒不落盘。
  remuxSeek.value = 0
  pendingResumeMs = 0
  lastSaveMs = 0
  // 亮度遮罩也要复位：上一集被手势压暗后，下一集会带着同一层黑开场，
  // 用户会以为新一集画面发灰/变色。
  resetBrightness()
  introDone = false
  outroFired = false
  skipShow.value = ''
  rawUrl = url
  // 换了线路：不管是手动选的还是自动失败切过来的，fatal 错误计数都要
  // 从 0 开始——这是「新的一条线」，不该背着上一条线攒的失败次数。
  hlsRecover = 0
  lineFatalTotal = 0
  // 用户手动选的线路：给它一次干净的机会，不被之前的自动失败记录卡住。
  // 自动失败切换（auto=true）则保留 triedLines，避免在几条坏线路间来回打转。
  if (!auto) {
    triedLines = new Set()
    codecIncompatibleSeen = false
  }
  if (!auto) app.showTip('切换线路中…')
  resolving.value = true
  // url 已经是明确选定的目标线路（用户选的，或者从 ep.urls 里专门挑的未试过
  // 的那条），跳过 pickPlayableLive() 的 /api/peek 探活，见 resolveUrl 里的
  // 注释——否则探活可能把这条已经在 HLS 层确认播不通的线路又判成「能用」
  // 原样放回来，和换线路逻辑来回拉扯成死循环。
  await safeResolve(true)
  initPlayer()
  startLineQualityWatch()
}

function retry() {
  destroyPlayer()
  hlsRecover = 0
  lineFatalTotal = 0
  triedLines = new Set()
  codecIncompatibleSeen = false
  liveStatusReported = false
  // 用户主动重试 = 重新给一次机会，把兜底标记、自动选线开关都一起恢复默认
  remuxFallbackTried = false
  autoLineSwitch.value = true
  prepared.value = false
  playing.value = false
  curMs.value = 0
  durMs.value = 0
  errMsg.value = ''
  resolving.value = true
  safeResolve().then(() => initPlayer())
}

function destroyPlayer() {
  clearLiveStallWatchdog()
  clearRetryForeverTimer()
  if (codecInfoTimer) { clearInterval(codecInfoTimer); codecInfoTimer = 0 }
  if (hls) {
    hls.destroy()
    hls = null
  }
  if (mpegtsPlayer) {
    mpegtsPlayer.destroy()
    mpegtsPlayer = null
  }
  if (videoEl.value) {
    videoEl.value.pause()
    videoEl.value.removeAttribute('src')
    videoEl.value.load()
  }
  if (skipTimer) { clearInterval(skipTimer); skipTimer = 0 }
  if (flashTimer) { clearTimeout(flashTimer); flashTimer = 0 }
  if (osdTimer) { clearTimeout(osdTimer); osdTimer = 0 }
  if (volTimer) { clearTimeout(volTimer); volTimer = 0 }
  cancelAutoplay()
  playFlash.value = ''
}

/**
 * 当前真实播放位置（毫秒）。
 *
 * 不能直接用 curMs：它只在 ontimeupdate 里更新，而 timeupdate 是浏览器
 * 按 ~250ms 节流触发的，离开页面那一刻的 curMs 往往落后实际进度好几百毫秒；
 * 更关键的是转封装模式下视频时间戳从 seek 点重新从 0 开始，
 * 必须把 remuxSeek 偏移加回去才是整片里的位置。
 * 这里以 video.currentTime 为准实时换算，取不到时退回 curMs。
 */
function realPosMs(): number {
  const vid = videoEl.value
  if (!vid || !isFinite(vid.currentTime)) return curMs.value
  const base = remuxMode.value ? remuxSeek.value : 0
  return Math.max(0, (base + vid.currentTime) * 1000)
}

function savePos() {
  if (isLive) return
  // 用实时位置而不是 curMs，避免存进去的是几秒前的旧值
  const pos = realPosMs()
  if (pos <= 0) return
  curMs.value = pos
  // 网盘播放：保存续播记录（网盘页「继续观看」）。
  // 但 wdVod 为空时（老数据、或从直链进来的网盘资源）不能直接 return ——
  // 那样连点播历史也不会写，结果是「看过但哪都没有记录」。让它落到下面的
  // 点播历史分支，至少首页「继续观看」还能看到。
  if (siteKey === 'webdav' && player.wdVod) {
    api
      .updateWdHistory({
        srcId: player.wdSrcId,
        srcName: player.wdSrcName,
        vodName: player.wdVod,
        dir: player.wdDir,
        epIndex: player.idx,
        epName: player.current?.name || '',
        url: player.current?.url || '',
        pos: Math.floor(pos),
        dur: durMs.value > 0 ? Math.floor(durMs.value) : 0,
        time: Date.now()
      })
      .catch(() => {})
    return
  }
  if (!siteKey || !vodId) return
  api
    .updateHistoryPos({
      siteKey,
      siteName: vodSiteName,
      vodId,
      name: vodName,
      pic: vodPic,
      epName: player.current?.name || '',
      epIndex: player.idx,
      epCount: player.count,
      from: '',
      pos: Math.floor(pos),
      dur: durMs.value > 0 ? Math.floor(durMs.value) : 0,
      time: Date.now()
    })
    .catch(() => {})
}

/**
 * 页面就要被销毁时的应急保存（关标签页、切后台被回收、手机按 Home）。
 *
 * 这些场景下 Vue 的 onUnmounted 根本不会触发 —— 浏览器直接丢弃整个页面，
 * 于是「看了半小时、直接关掉、下次打开又从头发」。
 * 这时候普通 fetch 也可能被中断，所以用 sendBeacon：浏览器保证把它发出去
 * 才会真正销毁页面。
 */
function savePosBeacon() {
  if (isLive) return
  const pos = realPosMs()
  if (pos <= 0) return
  const isWd = siteKey === 'webdav' && player.wdVod
  let path = ''
  let body: Record<string, unknown> = {}
  const dur = durMs.value > 0 ? Math.floor(durMs.value) : 0
  if (isWd) {
    path = '/api/webdav/history/update'
    body = {
      srcId: player.wdSrcId,
      srcName: player.wdSrcName,
      vodName: player.wdVod,
      dir: player.wdDir,
      epIndex: player.idx,
      epName: player.current?.name || '',
      url: player.current?.url || '',
      pos: Math.floor(pos),
      dur,
      time: Date.now()
    }
  } else {
    if (!siteKey || !vodId) return
    path = '/api/history/pos'
    body = {
      siteKey,
      siteName: vodSiteName,
      vodId,
      name: vodName,
      pic: vodPic,
      epName: player.current?.name || '',
      epIndex: player.idx,
      epCount: player.count,
      from: '',
      pos: Math.floor(pos),
      dur,
      time: Date.now()
    }
  }
  try {
    const blob = new Blob([JSON.stringify(body)], { type: 'application/json' })
    navigator.sendBeacon(path, blob)
  } catch {
    // sendBeacon 不可用（极老浏览器/被策略禁用）就退回同步 XHR，
    // 至少比什么都不做好 —— 这点数据量不至于卡住关页。
    try {
      const xhr = new XMLHttpRequest()
      xhr.open('POST', path, false)
      xhr.setRequestHeader('Content-Type', 'application/json')
      xhr.send(JSON.stringify(body))
    } catch {
      /* 尽力而为 */
    }
  }
}

function onPageHide() {
  savePosBeacon()
}

function onVisibilityChange() {
  // 切到后台：移动端很可能不会再回来（被系统回收），先存一份。
  if (document.visibilityState === 'hidden') savePosBeacon()
}

// ---------- 提示 ----------

function popOsd() {
  // 用 realPosMs() 而不是裸的 currentTime：转封装模式下视频时间戳从
  // seek 点重新计数，直接取会让进度条在每次唤出 OSD 时瞬间往回跳。
  curMs.value = realPosMs()
  showOsd.value = true
  cursorHidden.value = false // 唤出界面时鼠标指针一定要跟着可见
  if (osdTimer) clearTimeout(osdTimer)
  osdTimer = window.setTimeout(() => {
    if (playing.value) hideUI()
  }, 12000)
}

/**
 * 主动收起界面：桌面端「隐藏界面」按钮、鼠标移到屏幕左右边缘、以及上面
 * 12 秒无操作自动隐藏，三个入口共用这一个函数，保证"隐藏"这个动作本身
 * （包括全屏时顺带隐藏鼠标指针）只有一处实现。
 */
function hideUI() {
  if (osdTimer) { clearTimeout(osdTimer); osdTimer = 0 }
  showOsd.value = false
  // 不连带关掉播放设置/选线路面板：面板本身是独立的浮层/弹层，有自己的
  // 关闭按钮和点遮罩关闭，用户在里面看线路列表、犹豫选哪条时不该被这个
  // 12 秒无操作计时器（或鼠标贴边）半路把面板强制收起。
  if (isFullscreen.value) cursorHidden.value = true
}

function flashPlay(kind: string) {
  playFlash.value = kind
  if (flashTimer) clearTimeout(flashTimer)
  flashTimer = window.setTimeout(() => (playFlash.value = ''), 700)
}

// ---------- 跳过片头片尾 ----------

function showSkipBtn(kind: string, totalSec: number) {
  skipShow.value = kind
  skipLeft.value = totalSec
  if (skipTimer) clearInterval(skipTimer)
  skipTimer = window.setInterval(() => {
    skipLeft.value -= 0.1
    if (skipLeft.value <= 0) {
      // 先停表再执行：doSkip 在 skipShow 已变化时会提前 return，
      // 那样 skipTimer 永远停不下来，之后每秒都会再调一次 doSkip。
      clearInterval(skipTimer)
      skipTimer = 0
      doSkip(kind)
    }
  }, 100)
}

function doSkip(kind: string) {
  if (skipShow.value !== kind) return
  skipShow.value = ''
  if (kind === 'intro') {
    introDone = true
    const skipMs = introSkipSec.value * 1000
    if (videoEl.value && prepared.value && skipMs > 0) {
      // 同样要走 applySeek：转封装流直接改 currentTime 无效
      applySeek(skipMs / 1000)
      curMs.value = skipMs
    }
    app.showTip('已跳过片头 ' + fmtSec(introSkipSec.value))
  } else {
    outroFired = true
    if (player.hasNext) {
      app.showTip('片尾，跳下一集')
      switchEpisode(1)
    } else {
      app.showTip('已跳过片尾')
    }
  }
}

function fmtSec(sec: number): string {
  const m = Math.floor(sec / 60)
  const s = sec % 60
  if (m > 0 && s > 0) return m + '分' + s + '秒'
  if (m > 0) return m + '分钟'
  return s + '秒'
}

// 键盘
/**
 * 当前焦点是否在可编辑元素上。
 *
 * 播放页里有输入框（画质面板的字幕地址、选集搜索等），在这些地方打字时
 * 不能把按键当成播放器快捷键 —— 否则输入 "m" 会静音、敲空格会暂停、
 * 按方向键会跳进度，输入框基本没法用。
 */
function isTypingTarget(el: EventTarget | null): boolean {
  const node = el as HTMLElement | null
  if (!node || !node.tagName) return false
  const tag = node.tagName.toUpperCase()
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return true
  return !!node.isContentEditable
}

function onKey(e: KeyboardEvent) {
  // 输入态下完全放行，连 preventDefault 都不做
  if (isTypingTarget(e.target)) return
  // 带修饰键的（Ctrl+C 等）也不劫持
  if (e.ctrlKey || e.metaKey || e.altKey) return
  const k = e.key.toLowerCase()
  if (e.key === ' ' || e.key === 'Enter') {
    e.preventDefault()
    if (!showOsd.value) popOsd()
    else togglePlay()
  } else if (e.key === 'ArrowLeft') {
    e.preventDefault()
    if (isLive) switchEpisode(-1)
    else seekBy(-1)
  } else if (e.key === 'ArrowRight') {
    e.preventDefault()
    if (isLive) switchEpisode(1)
    else seekBy(1)
  } else if (e.key === 'ArrowUp') {
    // 不 preventDefault 的话方向键会连带滚动背景页面（播放页本身不滚动，
    // 但浏览器会把滚动传给文档，桌面端表现为画面轻微位移）
    e.preventDefault()
    changeVolume(0.05)
  } else if (e.key === 'ArrowDown') {
    e.preventDefault()
    changeVolume(-0.05)
  } else if (k === 'm') toggleMute()
  else if (k === 'f') toggleFullscreen()
  else if (k === 'p') {
    // 不支持画中画的环境直接忽略，不要静默失败
    if (supportsPip()) togglePip()
    else app.showTip('当前浏览器不支持画中画')
  }
}

onMounted(() => {
  window.addEventListener('keydown', onKey)
  document.addEventListener('fullscreenchange', onFullscreenChange)
  document.addEventListener('webkitfullscreenchange', onFullscreenChange)
  document.addEventListener('mozfullscreenchange', onFullscreenChange)
  document.addEventListener('MSFullscreenChange', onFullscreenChange)
  checkMobile()
  window.addEventListener('resize', checkMobile)
  // 关标签页/切后台时兜底保存进度：这两种情况下 onUnmounted 不会执行
  window.addEventListener('pagehide', onPageHide)
  document.addEventListener('visibilitychange', onVisibilityChange)
})
onUnmounted(() => {
  window.removeEventListener('keydown', onKey)
  document.removeEventListener('fullscreenchange', onFullscreenChange)
  document.removeEventListener('webkitfullscreenchange', onFullscreenChange)
  document.removeEventListener('mozfullscreenchange', onFullscreenChange)
  document.removeEventListener('MSFullscreenChange', onFullscreenChange)
  window.removeEventListener('resize', checkMobile)
  window.removeEventListener('pagehide', onPageHide)
  document.removeEventListener('visibilitychange', onVisibilityChange)
})
</script>

<template>
  <!-- 桌面端：全屏固定播放器 -->
  <div
    v-if="!isMobile"
    ref="playerEl"
    class="player"
    :class="{ 'cursor-hidden': cursorHidden && isFullscreen }"
    @mousemove="onMouseMove"
    @click="onSurfaceClick"
    @dblclick="toggleFullscreen"
  >
    <video ref="videoEl" class="video" :style="{ objectFit: scaleFit }"></video>

    <!-- 中央播放/暂停图标 -->
    <transition name="flash">
      <div v-if="playFlash" class="play-flash">{{ playFlash === 'play' ? '▶' : '❚❚' }}</div>
    </transition>

    <!-- 常驻提示（连播倒计时、解析降级说明）。
         这个 ref 原来只有赋值没有渲染，「即将播放下一集（3s）」用户根本看不到，
         3 秒后画面突然就跳了，像是播放器自己乱跑。 -->
    <div v-if="hint" class="player-hint">{{ hint }}</div>

    <!-- 加载/缓冲 -->
    <div v-if="resolving" class="center-tip">正在解析播放地址…</div>
    <div v-else-if="buffering" class="center-tip">缓冲中…</div>
    <div v-else-if="errMsg" class="center-tip err">
      <p>{{ errMsg }}</p>
      <div class="err-actions">
        <el-button @click="retry">重试</el-button>
        <el-button @click="goBack">返回</el-button>
        <!-- 播不出来的时候更需要看一眼到底拿到了多少信息（容器/分辨率/
             编码……），不受 prepared 限制，这个按钮始终在。 -->
        <el-button @click="toggleCodecInfo">编码信息</el-button>
      </div>
    </div>

    <!-- 跳过片头/片尾按钮 -->
    <div v-if="skipShow" class="skip-btn" @click="doSkip(skipShow)">
      <span class="ring" :style="{ '--p': skipLeft }"></span>
      {{ skipShow === 'intro' ? '跳过片头' : player.hasNext ? '下一集' : '跳过片尾' }}
    </div>

    <!-- 编码信息面板：故意不受 prepared 限制——播不出来的时候更需要看一眼
         到底拿到了多少信息，入口见上面错误提示里的「编码信息」按钮，以及
         下面控制条里的 ⓘ 按钮。 -->
    <div v-if="showCodecInfo" class="codec-panel" @click.stop @click.self="showCodecInfo = false">
      <div class="codec-content">
        <div class="codec-title">编码信息<button class="codec-close" @click="showCodecInfo = false">✕</button></div>
        <div class="codec-grid">
          <template v-for="row in codecInfoRows" :key="row.label">
            <div v-if="row.value !== ''" class="codec-row">
              <span class="codec-label">{{ row.label }}</span>
              <span class="codec-value">{{ row.value }}</span>
            </div>
          </template>
        </div>
      </div>
    </div>

    <!-- 桌面端左上角画中画按钮。
         必须跟 OSD 一起显隐：原来只判 prepared，控制条 12 秒自动隐藏后
         它还常驻在角上，看片时一直挡着一块画面。鼠标移上去会唤出 OSD。 -->
    <button
      v-if="prepared && showOsd && supportsPip()"
      class="corner-btn pip-btn"
      :title="isPip ? '退出画中画 (P)' : '画中画 (P)'"
      @click.stop="togglePip"
    >
    <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <rect x="2" y="3" width="20" height="14" rx="2"/>
      <rect x="12" y="10" width="9" height="7" rx="1"/>
    </svg>
  </button>

    <!-- 桌面端右上角全屏按钮：固定在角上常驻，不跟底部 .controls 一起走——
         以前跟选集/自动连播/投屏这些按钮混在同一行里，点播和直播下这些
         按钮的个数不一样，全屏按钮在行里的位置会跟着来回挪，这里改成和
         画中画按钮一样的角标按钮，位置永远固定。 -->
    <button
      v-if="isLive || showOsd"
      class="corner-btn fullscreen-corner-btn"
      :title="isFullscreen ? '退出全屏 (F)' : '全屏 (F)'"
      @click.stop="toggleFullscreen"
    >{{ isFullscreen ? '⤓' : '⛶' }}</button>

  <!-- 控制条：不等 prepared，起播前也常驻显示——点播/直播都一样，只是
       直播额外不受 showOsd 自动隐藏影响（见 popOsd 调用处）。 -->
    <div v-if="isLive || showOsd" class="osd" @click.stop>
      <div class="osd-top">
        <button class="back-btn" @click="goBack">
          <svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M15 18l-6-6 6-6" /></svg>
          <span>返回</span>
        </button>
        <span class="osd-title">{{ title }}</span>
        <span class="osd-meta">{{ epLabel }} {{ via ? '· ' + via : '' }}</span>
      </div>
      <div class="osd-progress" @pointerdown="onProgressDown">
        <div class="progress-track">
          <div class="progress-fill" :style="{ width: (dragging ? (previewMs / durMs) * 100 : progressPct) + '%' }"></div>
        </div>
      </div>
      <div v-if="dragging" class="seek-hint">{{ fmtMs(previewMs) }} / {{ fmtMs(durMs) }}</div>
      <div class="osd-bottom">
        <span class="time">{{ dragging ? fmtMs(previewMs) : fmtMs(curMs) }}</span>
        <div class="controls">
          <button class="ctl" @click="switchEpisode(-1)">{{ isLive ? '上一台' : '上一集' }}</button>
          <button class="ctl" @click="seekBy(-1)">−10 秒</button>
          <button class="ctl primary" @click="togglePlay">{{ playPauseText }}</button>
          <button class="ctl" @click="seekBy(1)">+10 秒</button>
          <button class="ctl" @click="switchEpisode(1)">{{ isLive ? '下一台' : '下一集' }}</button>
          <button class="ctl" @click="toggleMute">{{ muteText }}</button>
          <div class="vol-slider" @pointerdown="onVolDown">
            <div class="vol-track"><div class="vol-fill" :style="{ width: (muted ? 0 : volume * 100) + '%' }"></div></div>
          </div>
          <button class="ctl" @click="togglePlaySpeed" :title="`当前 ${rateLabel}x，点击切换倍速`">{{ rateLabel }}x</button>
          <button class="ctl" @click="showQ = !showQ" :title="qTitle">{{ isLive ? '选择线路' : qLabel }}</button>
          <button v-if="!isLive && player.count > 1" class="ctl" @click="showEps = true">选集</button>
          <!-- 自动连播开关：原来只有移动端有，桌面端播完只好被动连播，
               想逐集看完再停下也没法设置。 -->
          <button
            v-if="!isLive && player.hasNext"
            class="ctl"
            :title="autoPlayNext ? '播完自动下一集，点击改为手动' : '播完停在当集，点击改为自动'"
            @click="toggleAutoPlayNext"
          >{{ autoPlayNext ? '自动连播' : '手动切集' }}</button>
          <button v-if="supportsCast()" class="ctl" :class="{ primary: castConnected }" @click="toggleCast">{{ castConnected ? '断开投屏' : '投屏' }}</button>
          <!-- 编码信息：分辨率/帧率/编码/码率这些排障才会用到的细节，不用
               常驻占地方，点一下才展开。 -->
          <button class="ctl" title="编码信息" @click="toggleCodecInfo">ⓘ</button>
          <!-- 主动隐藏界面：以前只能等 12 秒无操作自动收起，看片想单纯看
               画面时没法立刻把控制条弄掉。直播控制条已经改成常驻显示，
               点这个按钮不会有任何可见效果，所以直播下不显示。 -->
          <button v-if="!isLive" class="ctl" title="隐藏界面" @click="hideUI">隐藏界面</button>
        </div>
        <span class="time">{{ durMs > 0 ? fmtMs(durMs) : (isLive ? '直播' : '') }}</span>
        <span v-if="showVol" class="vol">{{ volPct }}</span>
      </div>
    </div>

    <!-- 画质面板（桌面浮层，结构见 PlayerSettingsPanel） -->
    <PlayerSettingsPanel
      v-model="showQ"
      variant="desktop"
      :is-live="isLive"
      :levels="levels"
      :q-cap="qCap"
      :level-idx="levelIdx"
      :lines="player.current?.urls || []"
      :line-quality="lineQuality"
      :raw-url="rawUrl"
      :sub-delay="subDelay"
      :sub-font-size="subFontSize"
      :external-sub-active="externalSubActive"
      :sub-label="subLabel"
      :sub-tracks="subtitles"
      :sub-track-idx="currentSubIdx"
      :scale-names="scaleNames"
      :scale-mode="scaleMode"
      :intro-skip-sec="introSkipSec"
      :outro-skip-sec="outroSkipSec"
      :skip-own="skipOwn"
      :global-intro-skip="globalIntroSkip"
      :global-outro-skip="globalOutroSkip"
      :auto-switch="autoLineSwitch"
      @choose-level="chooseLevel"
      @switch-line="switchLine"
      @update:auto-switch="autoLineSwitch = $event"
      @sub-file="onSubFile"
      @sub-url="promptSubUrl"
      @remove-sub="removeExternalSub"
      @choose-sub-track="switchSubtitle"
      @sub-delay="setSubDelay"
      @sub-size="setSubFontSize"
      @scale="setScale"
      @intro-skip="setIntroSkip"
      @outro-skip="setOutroSkip"
      @mark-skip="markSkip"
      @skip-apply-all="applySkipToAll"
      @skip-reset="resetSkipCfg"
    />

    <!-- 选集抽屉：桌面端也要挂。
         之前只挂在移动端模板里，桌面端点「选集」只把 showEps 置 true，
         没有任何组件去渲染它 —— 按钮等于点了没反应。 -->
    <EpisodeDrawer
      v-model="showEps"
      :title="`选集 · ${player.vodTitle || ''}（${player.count} 集）`"
      :episodes="player.eps"
      :current-idx="player.idx"
      @select="jumpToEpisode"
    />

    <!-- 桌面端原生全屏时，只有 .player 这棵子树还会被渲染到屏幕上——
         App.vue 里挂的那个全局 TipToast 会被挡在全屏背后看不见，这里在
         .player 内部再挂一份，内容完全一致，只是多了个「全屏时也可见」。 -->
    <TipToast />
  </div>
  <div v-else ref="playerEl" class="m-player" :class="{ 'm-fullscreen': isFullscreen }">
    <!-- 视频区域 -->
    <div
      class="m-video-wrap"
      @click="onSurfaceClick"
      @dblclick="toggleFullscreen"
      @touchstart="onTouchStart"
      @touchmove="onTouchMove"
      @touchend="onTouchEnd"
    >
      <video ref="videoEl" class="video" :style="{ objectFit: scaleFit }"></video>

      <!-- 模拟亮度遮罩（左滑调节，覆盖在视频上方） -->
      <div class="brightness-mask" :style="{ opacity: brightness }"></div>

      <!-- 手势提示 -->
      <div v-if="gestureHint" class="m-gesture-hint">{{ gestureHint }}</div>

      <!-- 加载/缓冲（视频区内）-->
      <div v-if="resolving" class="m-center-tip">正在解析…</div>
      <div v-else-if="buffering" class="m-center-tip">缓冲中…</div>
      <div v-else-if="errMsg" class="m-center-tip m-err">
        <p>{{ errMsg }}</p>
        <div class="err-actions">
          <el-button size="small" @click="retry">重试</el-button>
          <el-button size="small" @click="goBack">返回</el-button>
          <el-button size="small" @click="toggleCodecInfo">编码信息</el-button>
        </div>
      </div>

      <!-- 播放/暂停动画 -->
      <transition name="flash">
        <div v-if="playFlash" class="play-flash" style="font-size:56px">{{ playFlash === 'play' ? '▶' : '❚❚' }}</div>
      </transition>

      <!-- 常驻提示（连播倒计时等），与桌面端同一个 hint -->
      <div v-if="hint" class="player-hint m-player-hint">{{ hint }}</div>

      <!-- 跳过按钮 -->
      <div v-if="skipShow" class="skip-btn" @click.stop="doSkip(skipShow)">
        <span class="ring"></span>
        {{ skipShow === 'intro' ? '跳过片头' : player.hasNext ? '下一集' : '跳过片尾' }}
      </div>

      <!-- 全屏时的浮动 OSD（覆盖在视频上方）：不等 prepared，起播前也常驻
           显示；直播额外不受 showOsd 自动隐藏影响。 -->
      <div v-if="isFullscreen && (isLive || showOsd)" class="m-osd-fullscreen" @click.stop>
        <div class="m-fs-top">
          <button class="m-back-btn" @click="goBack">
            <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="M15 18l-6-6 6-6" /></svg>
          </button>
          <span class="m-fs-title">{{ title }}</span>
          <button v-if="!isLive" class="m-icon-btn" title="隐藏界面" @click="hideUI">⌄</button>
          <button class="m-icon-btn" @click="toggleFullscreen">⤓</button>
        </div>
        <div class="m-fs-bottom">
          <!-- 直播没有可拖动的时长，不渲染进度条（否则会多出一条永远不动的空条） -->
          <div v-if="!isLive" class="m-progress-wrap" @pointerdown="onProgressDown">
            <div class="m-progress-track">
              <div class="m-progress-fill" :style="{ width: (dragging ? (previewMs / durMs) * 100 : progressPct) + '%' }"></div>
            </div>
          </div>
          <div v-if="dragging" class="m-seek-hint">{{ fmtMs(previewMs) }}</div>
          <div class="m-fs-controls">
            <!-- 全屏下也要能切集，否则每换一集都得先退出全屏 -->
            <button v-if="player.count > 1" class="m-ctl-icon" :title="isLive ? '上一台' : '上一集'" @click="switchEpisode(-1)">
              <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><polyline points="11 17 2 12 11 7"/><line x1="22" y1="7" x2="22" y2="17"/><polyline points="22 17 13 12 22 7"/></svg>
            </button>
            <span class="m-time">{{ fmtMs(curMs) }}</span>
            <!-- 这两个是「快退/快进 10 秒」，图标要用回转箭头，
                 原来画的是「上/下一集」的双三角，和实际功能不符，容易误导。 -->
            <button class="m-ctl-icon" title="快退 10 秒" @click="seekBy(-1)">
              <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><polyline points="1 4 1 10 7 10"/><path d="M3.51 15a9 9 0 1 0 .49-3.96"/></svg>
            </button>
            <button class="m-play-btn" @click="togglePlay">
              <svg v-if="!playing" viewBox="0 0 24 24" width="32" height="32" fill="currentColor"><polygon points="5 3 19 12 5 21"/></svg>
              <svg v-else viewBox="0 0 24 24" width="32" height="32" fill="currentColor"><rect x="6" y="4" width="4" height="16"/><rect x="14" y="4" width="4" height="16"/></svg>
            </button>
            <button class="m-ctl-icon" title="快进 10 秒" @click="seekBy(1)">
              <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><polyline points="23 4 23 10 17 10"/><path d="M20.49 15a9 9 0 1 1-.49-3.96"/></svg>
            </button>
            <span class="m-time">{{ durMs > 0 ? fmtMs(durMs) : (isLive ? '直播' : '--:--') }}</span>
            <button v-if="player.count > 1" class="m-ctl-icon" :title="isLive ? '下一台' : '下一集'" @click="switchEpisode(1)">
              <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><polyline points="13 17 22 12 13 7"/><line x1="2" y1="7" x2="2" y2="17"/><polyline points="2 17 11 12 2 7"/></svg>
            </button>
            <button class="m-icon-btn" style="margin-left:auto" title="编码信息" @click="toggleCodecInfo">ⓘ</button>
            <button class="m-icon-btn" @click="showQ = !showQ">⚙</button>
          </div>
        </div>
      </div>
    </div>

    <!-- 非全屏时：视频下方控制面板 -->
    <div v-if="!isFullscreen" class="m-panel">
      <!-- 标题栏 -->
      <div class="m-panel-title">
        <button class="m-back-btn" @click="goBack">
          <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="M15 18l-6-6 6-6" /></svg>
        </button>
        <div class="m-title-info">
          <div class="m-title-name">{{ title }}</div>
          <div v-if="epLabel" class="m-title-meta">{{ epLabel }}<span v-if="via"> · {{ via }}</span></div>
        </div>
        <button class="m-icon-btn" @click="toggleFullscreen">⛶</button>
      </div>

      <!-- 进度条 -->
      <div v-if="!isLive" class="m-progress-row">
        <span class="m-time-sm">{{ fmtMs(curMs) }}</span>
        <div class="m-progress-wrap" @pointerdown="onProgressDown">
          <div class="m-progress-track">
            <div class="m-progress-fill" :style="{ width: (dragging ? (previewMs / durMs) * 100 : progressPct) + '%' }"></div>
          </div>
        </div>
        <span class="m-time-sm">{{ durMs > 0 ? fmtMs(durMs) : '--:--' }}</span>
      </div>
      <div v-if="dragging" class="m-seek-hint">{{ fmtMs(previewMs) }}</div>

      <!-- 主控制按钮行 -->
      <div class="m-main-controls">
        <button class="m-ctl-icon" @click="switchEpisode(-1)">
          <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><polyline points="11 17 2 12 11 7"/><line x1="22" y1="7" x2="22" y2="17"/><polyline points="22 17 13 12 22 7"/></svg>
        </button>
        <button class="m-ctl-icon" @click="seekBy(-1)">
          <svg viewBox="0 0 24 24" width="24" height="24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><polyline points="1 4 1 10 7 10"/><path d="M3.51 15a9 9 0 1 0 .49-3.96"/></svg>
        </button>
        <button class="m-play-btn" @click="togglePlay">
          <svg v-if="!playing" viewBox="0 0 24 24" width="36" height="36" fill="currentColor"><polygon points="5 3 19 12 5 21"/></svg>
          <svg v-else viewBox="0 0 24 24" width="36" height="36" fill="currentColor"><rect x="6" y="4" width="4" height="16"/><rect x="14" y="4" width="4" height="16"/></svg>
        </button>
        <button class="m-ctl-icon" @click="seekBy(1)">
          <svg viewBox="0 0 24 24" width="24" height="24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><polyline points="23 4 23 10 17 10"/><path d="M20.49 15a9 9 0 1 1-.49-3.96"/></svg>
        </button>
        <button class="m-ctl-icon" @click="switchEpisode(1)">
          <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><polyline points="13 17 22 12 13 7"/><line x1="2" y1="7" x2="2" y2="17"/><polyline points="2 17 11 12 2 7"/></svg>
        </button>
      </div>

      <!-- 辅助按钮行 -->
      <div class="m-aux-controls">
        <button class="m-aux-btn" @click="toggleMute">
          <span class="m-aux-icon">{{ muted ? '🔇' : '🔊' }}</span>
          <span>{{ muted ? '已静音' : Math.round(volume * 100) + '%' }}</span>
        </button>
        <button v-if="!isLive && player.hasNext" class="m-aux-btn" @click="toggleAutoPlayNext">
          <span class="m-aux-icon">{{ autoPlayNext ? '▶' : '⏸' }}</span>
          <span>{{ autoPlayNext ? '自动连播' : '手动切集' }}</span>
        </button>
        <button class="m-aux-btn" @click="togglePlaySpeed">
          <span class="m-aux-icon">{{ rateLabel }}x</span>
          <span>倍速</span>
        </button>
        <button class="m-aux-btn" @click="showQ = !showQ" :title="qTitle">
          <span class="m-aux-icon">⚙</span>
          <span>{{ isLive ? '选择线路' : qLabel }}</span>
        </button>
        <button v-if="!isLive && player.count > 1" class="m-aux-btn" @click="showEps = true">
          <span class="m-aux-icon">☷</span>
          <span>选集</span>
        </button>
        <button v-if="supportsCast()" class="m-aux-btn" @click="toggleCast">
          <span class="m-aux-icon">{{ castConnected ? '⇄' : '⤴' }}</span>
          <span>{{ castConnected ? '断开投屏' : '投屏' }}</span>
        </button>
        <button v-if="supportsPip()" class="m-aux-btn" @click="togglePip">
          <span class="m-aux-icon">{{ isPip ? '▣' : '⧉' }}</span>
          <span>{{ isPip ? '退出PiP' : '画中画' }}</span>
        </button>
        <button class="m-aux-btn" @click="toggleFullscreen">
          <span class="m-aux-icon">⛶</span>
          <span>全屏</span>
        </button>
        <button class="m-aux-btn" @click="toggleCodecInfo">
          <span class="m-aux-icon">ⓘ</span>
          <span>编码信息</span>
        </button>
      </div>
    </div>

    <!-- 画质面板（移动端底部 sheet，结构见 PlayerSettingsPanel） -->
    <PlayerSettingsPanel
      v-model="showQ"
      variant="mobile"
      :is-live="isLive"
      :levels="levels"
      :q-cap="qCap"
      :level-idx="levelIdx"
      :lines="player.current?.urls || []"
      :line-quality="lineQuality"
      :raw-url="rawUrl"
      :sub-delay="subDelay"
      :sub-font-size="subFontSize"
      :external-sub-active="externalSubActive"
      :sub-label="subLabel"
      :sub-tracks="subtitles"
      :sub-track-idx="currentSubIdx"
      :scale-names="scaleNames"
      :scale-mode="scaleMode"
      :intro-skip-sec="introSkipSec"
      :outro-skip-sec="outroSkipSec"
      :skip-own="skipOwn"
      :global-intro-skip="globalIntroSkip"
      :global-outro-skip="globalOutroSkip"
      :auto-switch="autoLineSwitch"
      @choose-level="chooseLevel"
      @switch-line="switchLine"
      @update:auto-switch="autoLineSwitch = $event"
      @sub-file="onSubFile"
      @sub-url="promptSubUrl"
      @remove-sub="removeExternalSub"
      @choose-sub-track="switchSubtitle"
      @sub-delay="setSubDelay"
      @sub-size="setSubFontSize"
      @scale="setScale"
      @intro-skip="setIntroSkip"
      @outro-skip="setOutroSkip"
      @mark-skip="markSkip"
      @skip-apply-all="applySkipToAll"
      @skip-reset="resetSkipCfg"
    />

    <!-- 选集抽屉 -->
    <EpisodeDrawer
      v-model="showEps"
      :title="`选集 · ${player.vodTitle || ''}（${player.count} 集）`"
      :episodes="player.eps"
      :current-idx="player.idx"
      @select="jumpToEpisode"
    />

    <!-- 编码信息面板：不受 prepared 限制，入口见上面 m-err 里的「编码信息」
         按钮，以及下面控制条里的 ⓘ 按钮。 -->
    <div v-if="showCodecInfo" class="codec-panel" @click.stop @click.self="showCodecInfo = false">
      <div class="codec-content">
        <div class="codec-title">编码信息<button class="codec-close" @click="showCodecInfo = false">✕</button></div>
        <div class="codec-grid">
          <template v-for="row in codecInfoRows" :key="row.label">
            <div v-if="row.value !== ''" class="codec-row">
              <span class="codec-label">{{ row.label }}</span>
              <span class="codec-value">{{ row.value }}</span>
            </div>
          </template>
        </div>
      </div>
    </div>

    <TipToast />
  </div>
</template>

<style scoped>
/* ===== 桌面端播放器 ===== */
.player {
  position: fixed;
  inset: 0;
  background: #000;
  overflow: hidden;
}
.player.cursor-hidden,
.player.cursor-hidden * {
  cursor: none;
}
.video {
  width: 100%;
  height: 100%;
  object-fit: contain;
  display: block;
}
.play-flash {
  position: absolute;
  top: 50%;
  left: 50%;
  transform: translate(-50%, -50%);
  font-size: 90px;
  color: rgba(255, 255, 255, 0.9);
  pointer-events: none;
}
.flash-enter-active, .flash-leave-active { transition: opacity 0.3s; }
.flash-enter-from, .flash-leave-to { opacity: 0; }

.center-tip {
  position: absolute;
  top: 50%;
  left: 50%;
  transform: translate(-50%, -50%);
  color: #fff;
  font-size: 18px;
  text-align: center;
}
.center-tip.err p { margin-bottom: 12px; }
.err-actions { display: flex; justify-content: center; gap: 12px; flex-wrap: wrap; }

/* 编码信息面板：跟 PlayerSettingsPanel 的 q-panel/q-content 同一套视觉
   语言（半透明背景居中卡片），不复用那个组件——这里只需要一份只读的
   标签/值列表，没必要为了这点展示逻辑多引入一个带一堆 props 的组件。 */
.codec-panel {
  position: absolute;
  inset: 0;
  background: rgba(0, 0, 0, 0.5);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 50;
}
.codec-content {
  background: var(--surface);
  border-radius: 16px;
  padding: 20px 24px;
  max-width: 560px;
  width: 86%;
  max-height: 80vh;
  overflow-y: auto;
}
.codec-title {
  font-size: 16px;
  font-weight: 600;
  margin-bottom: 12px;
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.codec-close {
  background: transparent;
  border: none;
  color: var(--sub);
  font-size: 16px;
  cursor: pointer;
  padding: 2px 6px;
}
.codec-close:hover { color: var(--txt); }
.codec-grid { display: flex; flex-direction: column; gap: 2px; }
.codec-row {
  display: flex;
  gap: 12px;
  padding: 7px 0;
  border-bottom: 1px solid var(--border);
  font-size: 13px;
}
.codec-row:last-child { border-bottom: none; }
.codec-label { flex: 0 0 110px; color: var(--sub); }
.codec-value {
  flex: 1;
  color: var(--txt);
  word-break: break-all;
}

/* 常驻提示：连播倒计时 / 解析降级说明。
   放底部靠上一点，避开控制条与进度条，不挡字幕。 */
.player-hint {
  position: absolute;
  left: 50%;
  bottom: 22%;
  transform: translateX(-50%);
  max-width: min(76%, 640px);
  padding: 8px 16px;
  border-radius: 8px;
  background: rgba(0, 0, 0, 0.62);
  color: #fff;
  font-size: 14px;
  line-height: 1.4;
  text-align: center;
  pointer-events: none;
  z-index: 6;
}

.skip-btn {
  position: absolute;
  right: 48px;
  bottom: 96px;
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 26px 12px 20px;
  background: rgba(24, 24, 24, 0.8);
  border: 1.5px solid rgba(255, 255, 255, 0.5);
  border-radius: 40px;
  color: #fff;
  font-size: 18px;
  cursor: pointer;
}
.ring {
  width: 22px;
  height: 22px;
  border-radius: 50%;
  border: 2.5px solid rgba(255, 255, 255, 0.3);
  border-top-color: #fff;
  animation: spin 1s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }

.osd {
  position: absolute;
  left: 0;
  right: 0;
  bottom: 0;
  padding: 24px 24px 16px;
  background: linear-gradient(transparent, rgba(0, 0, 0, 0.85));
}
.osd-top {
  display: flex;
  align-items: center;
  gap: 16px;
  margin-bottom: 10px;
}
.back-btn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 8px 16px 8px 12px;
  border-radius: 8px;
  background: rgba(255, 255, 255, 0.12);
  border: 1px solid rgba(255, 255, 255, 0.35);
  color: #fff;
  font-size: 15px;
  font-weight: 500;
  cursor: pointer;
  flex-shrink: 0;
  transition: background 0.15s, border-color 0.15s;
}
.back-btn:hover {
  background: rgba(255, 255, 255, 0.24);
  border-color: #fff;
}
.osd-title { font-size: 18px; font-weight: 600; }
.osd-meta { font-size: 13px; color: var(--sub); }

.osd-progress {
  padding: 10px 0;
  cursor: pointer;
  touch-action: none;
}
.progress-track {
  height: 6px;
  border-radius: 3px;
  background: rgba(255, 255, 255, 0.25);
  position: relative;
}
.progress-fill {
  height: 100%;
  border-radius: 3px;
  background: var(--live);
}
.osd-progress:hover .progress-track { height: 8px; }
.seek-hint {
  text-align: center;
  font-size: 14px;
  color: #ffeb3b;
  margin-bottom: 4px;
}

.osd-bottom {
  display: flex;
  align-items: center;
  gap: 12px;
}
.time {
  font-size: 13px;
  color: #fff;
  min-width: 52px;
}
.controls {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  flex: 1;
}
.ctl {
  padding: 8px 12px;
  border-radius: 4px;
  background: transparent;
  border: 1px solid transparent;
  color: #fff;
  font-size: 14px;
  cursor: pointer;
  white-space: nowrap;
}
.ctl:hover {
  background: var(--card-hi);
  border-color: #fff;
}
.ctl.primary {
  background: #fff;
  color: #141218;
  font-weight: 600;
}
.vol-slider {
  width: 80px;
  padding: 10px 0;
  cursor: pointer;
  touch-action: none;
}
.vol-track {
  height: 5px;
  border-radius: 3px;
  background: rgba(255, 255, 255, 0.25);
}
.vol-fill {
  height: 100%;
  border-radius: 3px;
  background: #fff;
}
.vol { font-size: 13px; color: #fff; }

/* 桌面端角标按钮（画中画 / 全屏）：固定在左上/右上角，跟 OSD 一起显隐，
   不随底部 .controls 里按钮数量变化而挪位——直播和点播按钮数量不一样
   （选集/自动连播等只有点播才有），之前全屏按钮混在那一行里时位置会
   跟着来回跳。 */
.corner-btn {
  position: absolute;
  top: 16px;
  width: 36px;
  height: 36px;
  border-radius: 8px;
  background: rgba(255, 255, 255, 0.12);
  border: 1px solid rgba(255, 255, 255, 0.35);
  color: #fff;
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  z-index: 20;
  transition: background 0.15s, border-color 0.15s;
}
.corner-btn:hover {
  background: rgba(255, 255, 255, 0.24);
  border-color: #fff;
}
.pip-btn { left: 16px; }
.fullscreen-corner-btn { right: 16px; font-size: 18px; }

:fullscreen .osd,
:-webkit-full-screen .osd,
:-moz-full-screen .osd { position: fixed; }

/* ===== 移动端播放器 ===== */
.m-player {
  position: fixed;
  inset: 0;
  background: #000;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}
.m-player.m-fullscreen {
  /* 全屏时回归单一视频容器 */
}

.m-video-wrap {
  position: relative;
  width: 100%;
  flex-shrink: 0;
  background: #000;
}
/* 竖屏非全屏时，视频区原来锁死 16:9 —— 390 宽的屏只有 219px 高（占 26%），
   而下面控制面板内容才 266px 却靠 flex:1 撑到 625px，底部空出 359px 纯白。
   现在视频区吃掉面板之外的全部剩余高度（视频用 contain 居中，多出的
   上下留黑边而不是白色空白），面板按内容高度贴着底部。 */
.m-player:not(.m-fullscreen) .m-video-wrap {
  flex: 1 1 auto;
  min-height: 0;
  display: flex;
  align-items: center;
  justify-content: center;
}
.m-player:not(.m-fullscreen) .m-video-wrap > .video {
  width: 100%;
  height: 100%;
}
.m-player.m-fullscreen .m-video-wrap {
  flex: 1;
  height: 100%;
}
.m-center-tip {
  position: absolute;
  top: 50%;
  left: 50%;
  transform: translate(-50%, -50%);
  color: #fff;
  font-size: 15px;
  text-align: center;
  z-index: 10;
}
.m-center-tip.m-err p { margin-bottom: 10px; font-size: 14px; }

/* 移动端提示：竖屏时视频区只有 16:9 高，b 22% 会压到画面中下部，
   改成贴着视频区底部；全屏时再往上抬一点避开控制条。 */
.m-player-hint {
  position: absolute;
  bottom: 12px;
  font-size: 13px;
  max-width: 88%;
}
.m-player.m-fullscreen .m-player-hint {
  bottom: 96px;
}

.m-panel {
  /* 原来是 flex:1 —— 会把面板拉伸到占满剩余空间，内容只有 266px
     却在 844 高的屏上占 625px，底部留出 359px 空白，视频被压到 219px。
     改成按内容自然高度（不放大），富余高度留给上面的视频区。 */
  flex: 0 0 auto;
  background: var(--surface);
  padding: 12px 16px 16px;
  overflow-y: auto;
  /* 横屏或小屏时面板内容可能比可用空间高，给个上限并允许滚动，
     否则会顶出视口（body 是 overflow:hidden，溢出部分直接看不到）。 */
  max-height: 62dvh;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.m-panel-title {
  display: flex;
  align-items: center;
  gap: 10px;
}
.m-back-btn {
  width: 36px;
  height: 36px;
  border-radius: 8px;
  background: var(--card);
  border: none;
  color: var(--text);
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  flex-shrink: 0;
}
.m-title-info {
  flex: 1;
  min-width: 0;
}
.m-title-name {
  font-size: 15px;
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.m-title-meta {
  font-size: 12px;
  color: var(--sub);
  margin-top: 2px;
}
.m-icon-btn {
  width: 36px;
  height: 36px;
  border-radius: 8px;
  background: var(--card);
  border: none;
  color: var(--text);
  font-size: 18px;
  cursor: pointer;
  flex-shrink: 0;
}

.m-progress-row {
  display: flex;
  align-items: center;
  gap: 8px;
}
.m-time-sm {
  font-size: 11px;
  color: var(--sub);
  min-width: 36px;
  text-align: center;
}
.m-progress-wrap {
  flex: 1;
  padding: 8px 0;
  cursor: pointer;
  touch-action: none;
}
.m-progress-track {
  height: 4px;
  border-radius: 2px;
  background: var(--card-hi);
  position: relative;
}
.m-progress-fill {
  height: 100%;
  border-radius: 2px;
  background: var(--live);
}
.m-seek-hint {
  text-align: center;
  font-size: 13px;
  color: #ffeb3b;
  margin: -6px 0 6px;
}

.m-main-controls {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 16px;
  padding: 8px 0;
}
.m-ctl-icon {
  width: 44px;
  height: 44px;
  border-radius: 8px;
  background: var(--card);
  border: none;
  color: var(--text);
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  flex-shrink: 0;
}
.m-play-btn {
  width: 64px;
  height: 64px;
  border-radius: 50%;
  background: var(--live);
  border: none;
  color: #fff;
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  flex-shrink: 0;
  box-shadow: 0 4px 12px rgba(0,0,0,0.15);
}

.m-aux-controls {
  display: flex;
  gap: 8px;
}
.m-aux-btn {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 4px;
  padding: 10px 8px;
  border-radius: 8px;
  background: var(--card);
  border: none;
  color: var(--text);
  font-size: 12px;
  cursor: pointer;
}
.m-aux-icon {
  font-size: 20px;
}

/* 全屏时浮动 OSD */
.m-osd-fullscreen {
  position: absolute;
  inset: 0;
  display: flex;
  flex-direction: column;
  justify-content: space-between;
  padding: 12px;
  background: linear-gradient(rgba(0,0,0,0.5) 0%, transparent 20%, transparent 80%, rgba(0,0,0,0.7) 100%);
  pointer-events: none;
}
.m-osd-fullscreen > * { pointer-events: auto; }

.m-fs-top {
  display: flex;
  align-items: center;
  gap: 10px;
}
.m-fs-title {
  flex: 1;
  color: #fff;
  font-size: 15px;
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.m-fs-bottom {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.m-fs-controls {
  display: flex;
  align-items: center;
  gap: 12px;
}
.m-time {
  font-size: 12px;
  color: #fff;
  min-width: 40px;
  text-align: center;
}

/* ===== 手势提示覆盖层 ===== */
.m-gesture-hint {
  position: absolute;
  top: 50%;
  left: 50%;
  transform: translate(-50%, -50%);
  background: rgba(0, 0, 0, 0.65);
  color: #fff;
  font-size: 16px;
  font-weight: 600;
  padding: 10px 18px;
  border-radius: 8px;
  pointer-events: none;
  z-index: 20;
  white-space: nowrap;
}

/* ===== 模拟亮度遮罩 ===== */
.brightness-mask {
  position: absolute;
  inset: 0;
  background: #000;
  pointer-events: none;
  z-index: 5;
  transition: opacity 0.1s;
}
</style>
