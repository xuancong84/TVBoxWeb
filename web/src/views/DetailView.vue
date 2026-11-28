<script setup lang="ts">
import { ref, computed, watch, onMounted, onUnmounted, nextTick } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessageBox } from 'element-plus'
import { api, humanizeApiError, imgUrl } from '@/api'
import { useAppStore } from '@/stores/app'
import { usePlayerStore } from '@/stores/player'
import { splitLines } from '@/utils/lines'
import { getCachedVod } from '@/vodCache'
import type { Vod, Line, SubRef, DownloadJob } from '@/types'
import { siteInfoMap } from '@/siteInfo'
import TopBar from '@/components/TopBar.vue'
import ErrorState from '@/components/ErrorState.vue'
import SkeletonList from '@/components/SkeletonList.vue'

const route = useRoute()
const router = useRouter()
const app = useAppStore()
const player = usePlayerStore()

const siteKey = route.query.site as string
const siteName = (route.query.siteName as string) || ''
const vodId = route.query.id as string

const vod = ref<Vod | null>(null)
const lines = ref<Line[]>([])
const curLine = ref(0)
const expanded = ref(false)
// 简介默认夹 3 行，但不少片子的简介本来就没三行长——这种时候「展开/收起」
// 按钮点了也没任何变化，纯粹给用户一种「还有更多内容」的错觉。只有真的
// 被 3 行截断了（scrollHeight 超过 clientHeight）才显示这个按钮。
const contentEl = ref<HTMLElement | null>(null)
const canExpand = ref(false)
function checkOverflow() {
  const el = contentEl.value
  if (!el || expanded.value) return
  canExpand.value = el.scrollHeight > el.clientHeight + 1
}
const isFav = ref(false)
const reverse = ref(false)
const loading = ref(true)
const loadError = ref('')

// 上次看到的集（用于定位）
const watchedEp = ref(-1)
// 已看过的所有集（分集管理）
const watchedEps = ref<Set<number>>(new Set())
// 上次播放进度（毫秒），用于续播
const histPos = ref(0)
const posterFailed = ref(false)

// 悬停「点播源」名字可看它的「单仓源」列表——跟 VodCard 上的徽标同一套查法
const subs = ref<SubRef[]>([])
const subsTitle = computed(() => {
  if (!subs.value.length) return '单仓源：未知'
  return '单仓源：\n' + subs.value.map((s) => s.name || s.source).join('\n')
})

async function loadDetail() {
  loading.value = true
  loadError.value = ''
  try {
    // 从点播/搜索/首页最近更新的卡片点进来的话，那边很可能已经拿到过一份
    // 带 playUrl 的完整 Vod 了（见 vodCache.ts）——这种情况下这份数据跟
    // 详情接口会返回的东西是同一回事，没必要再打一次网络请求重新要一遍。
    // 缓存没命中（比如直接刷新这个页面、从没有 playUrl 的「继续观看」
    // 点进来、或者新标签页打开）才现查。
    const detail = getCachedVod(siteKey, vodId) || (await api.vodDetail(siteKey, vodId))
    vod.value = detail
    lines.value = splitLines(detail.from, detail.playUrl)
    siteInfoMap().then((map) => {
      subs.value = map.get(siteKey)?.subs || []
    })
    // 默认线路：优先选第一条「直链」线路（网页地址播不出，真流在后面）
    if (lines.value.length) {
      const dIdx = lines.value.findIndex((l) => l.direct)
      curLine.value = dIdx >= 0 ? dIdx : 0
    }
  } catch (e: any) {
    loadError.value = humanizeApiError(e)
    app.showTip('详情加载失败：' + humanizeApiError(e))
  } finally {
    loading.value = false
    // loading 翻成 false 之后 .content 才会真正渲染出来（骨架屏切真实
    // 内容），必须等这一轮 DOM 更新完才能量得到准确的 scrollHeight。
    nextTick(checkOverflow)
    nextTick(fitEps)
  }
}

// 剧集区最小高度 220px；视口还有更多空间时一直撑到页面底部，
// 这样集数很多（几百集）时不用先滚页面再滚剧集区。
const epsEl = ref<HTMLElement | null>(null)
const epsMaxH = ref(0)
const EPS_MIN_H = 220
function fitEps() {
  const el = epsEl.value
  if (!el) return
  const docTop = el.getBoundingClientRect().top + window.scrollY
  // 剧集区下面还有 .lines 和 .page 各自的 padding-bottom，一并留出来，
  // 否则整页会比视口多出这几十像素，外层窗口也跟着出现滚动条。
  let below = 0
  for (let n: HTMLElement | null = el.parentElement; n && n !== document.body; n = n.parentElement) {
    below += parseFloat(getComputedStyle(n).paddingBottom) || 0
  }
  epsMaxH.value = Math.max(EPS_MIN_H, Math.floor(window.innerHeight - docTop - below))
}

onMounted(async () => {
  await loadDetail()
  try {
    const favs = await api.favorites()
    isFav.value = favs.some((f) => f.siteKey === siteKey && f.vodId === vodId)
  } catch {
    /* ignore */
  }
  try {
    const hist = await api.history()
    const h = hist.find((x) => x.siteKey === siteKey && x.vodId === vodId)
    if (h) {
      watchedEp.value = h.epIndex
      watchedEps.value = new Set(h.watchedEps?.length ? h.watchedEps : [h.epIndex])
      histPos.value = h.pos || 0
      // 预选上次播放的线路（按线路名匹配），不用再手动切线路
      const li = h.from ? lines.value.findIndex((l) => l.name === h.from) : -1
      if (li >= 0) curLine.value = li
    }
  } catch {
    /* ignore */
  }
  window.addEventListener('resize', checkOverflow)
  window.addEventListener('resize', fitEps)
  nextTick(fitEps)
})
// 下载进度：轮询任务列表，按「片名 + 集名」对应到每一集
const dlJobs = ref<DownloadJob[]>([])
let dlTimer = 0
async function loadDl() {
  try {
    dlJobs.value = (await api.downloads()).jobs
  } catch {
    /* ignore */
  }
}
onMounted(() => {
  loadDl()
  setTimeout(loadLocal, 800)
  dlTimer = window.setInterval(loadDl, 1500)
})
// 各集已有的本地文件类型：file = 普通 mp4 等；hls = .ts + .jm3u8（可转换为 mp4）
const localKinds = ref<Record<string, 'file' | 'hls'>>({})
const hasFfmpeg = ref(false)
async function loadLocal() {
  if (!vod.value || !lines.value.length) return
  try {
    const names = lines.value[curLine.value].eps.map((e) => e.name)
    const r = await api.downloadLocalStatus(vod.value.name, names)
    localKinds.value = r.kinds
    hasFfmpeg.value = r.ffmpeg
  } catch {
    /* ignore */
  }
}
// 任务状态变化（完成/失败/取消）后刷新本地文件情况
watch(
  () => dlJobs.value.map((j) => j.id + j.status).join(','),
  () => loadLocal()
)
watch([curLine, () => lines.value.length], () => loadLocal())
// 各线路已缓存的点播 m3u8：续播时不用再解析/拉取。key=线路下标
const m3u8Cache = ref<Record<number, { n: number; height: number }>>({})
// 已缓存 m3u8 的单集播放地址（按集显示「已获取」）
const cachedUrls = ref<Set<string>>(new Set())
async function loadM3u8Cache() {
  if (!lines.value.length) return
  const urls = new Set<string>()
  for (const l of lines.value) for (const e of l.eps) if (e.url) urls.add(e.url)
  try {
    const { status } = await api.m3u8CacheStatus([...urls])
    const out: Record<number, { n: number; height: number }> = {}
    lines.value.forEach((l, i) => {
      let n = 0
      let height = 0
      for (const e of l.eps) {
        const st = status[e.url]
        if (st?.cached) {
          n++
          height = Math.max(height, st.height || 0)
        }
      }
      if (n) out[i] = { n, height }
    })
    m3u8Cache.value = out
    cachedUrls.value = new Set(Object.keys(status).filter((u) => status[u]?.cached))
  } catch {
    /* ignore */
  }
}
function lineLabel(l: Line, i: number): string {
  const c = m3u8Cache.value[i]
  return l.name + (l.direct ? '' : ' ·解析') + (c ? ' (缓存' + (c.height ? ' ' + c.height + 'p' : '') + ')' : '')
}
async function dropLineCache(l: Line, i: number) {
  try {
    await api.m3u8CacheDropMany(l.eps.map((e) => e.url).filter(Boolean))
    delete m3u8Cache.value[i]
    m3u8Cache.value = { ...m3u8Cache.value }
    const gone = new Set(cachedUrls.value)
    for (const e of l.eps) gone.delete(e.url)
    cachedUrls.value = gone
    app.showTip('已删除「' + l.name + '」的 m3u8 缓存')
  } catch (e: any) {
    app.showTip('删除失败：' + humanizeApiError(e))
  }
  loadM3u8Cache()
}
function lineTitle(l: Line, i: number): string {
  const c = m3u8Cache.value[i]
  return c ? `已缓存 ${c.n}/${l.eps.length} 集的 m3u8，续播更快` : ''
}
let m3u8Timer = 0
watch(() => lines.value.length, () => loadM3u8Cache())
onMounted(() => {
  m3u8Timer = window.setInterval(loadM3u8Cache, 20000)
})
onUnmounted(() => clearInterval(m3u8Timer))
// 非 mp4 的已下载集
const convertibleEps = computed(() =>
  (lines.value[curLine.value]?.eps || []).filter((e) => localKinds.value[e.name] === 'hls').map((e) => e.name)
)
const converting = computed(() =>
  dlJobs.value.some((j) => j.kind === 'convert' && j.vod === vod.value?.name && (j.status === 'queued' || j.status === 'running'))
)
const convDlg = ref(false)
const convNames = ref<string[]>([]) // 本次要转换的集（全部或单集）
function openConvert(names: string[]) {
  convNames.value = names
  convDlg.value = true
}
async function doConvert(keep: boolean) {
  convDlg.value = false
  if (!vod.value || !convNames.value.length) return
  try {
    const r = await api.downloadConvert(vod.value.name, convNames.value, keep)
    app.showTip(r.added ? `已开始转换 ${r.added} 集` : '没有需要转换的集')
    loadDl()
  } catch (e) {
    app.showTip(humanizeApiError(e))
  }
}

function jobPct(j: DownloadJob): number {
  if (j.status === 'done') return 100
  if (j.total > 0) return Math.floor((j.done * 100) / j.total)
  if (j.size > 0) return Math.min(99, Math.floor((j.bytes * 100) / j.size))
  return 0
}
const dlByEp = computed(() => {
  const m = new Map<string, DownloadJob>()
  if (!vod.value) return m
  for (const j of dlJobs.value) if (j.kind !== 'convert' && j.vod === vod.value.name) m.set(j.name, j) // 同名取最新
  return m
})
// 显示下标 → 该集下载状态文字（无任务返回空）
function epDl(displayIdx: number): string {
  const line = lines.value[curLine.value]
  if (!line) return ''
  const realIdx = reverse.value ? line.eps.length - 1 - displayIdx : displayIdx
  const nm = line.eps[realIdx]?.name
  const cached = cachedUrls.value.has(line.eps[realIdx]?.url) ? '已获取' : ''
  if (dlJobs.value.some((x) => x.kind === 'convert' && x.vod === vod.value?.name && x.name === nm && (x.status === 'queued' || x.status === 'running'))) return '转换中'
  const j = dlByEp.value.get(nm)
  if (!j) return localKinds.value[nm] ? '已下载' : cached
  if (j.status === 'done') return '已下载'
  if (j.status === 'error') return '下载失败'
  if (j.status === 'canceled') return localKinds.value[nm] ? '已下载' : cached
  if (j.status === 'queued') return '排队中'
  return jobPct(j) + '%'
}
// 当前线路整体进度（按集平均），没有进行中的任务时为 null
const dlOverall = computed<number | null>(() => {
  const line = lines.value[curLine.value]
  if (!line) return null
  const js = line.eps.map((e) => dlByEp.value.get(e.name)).filter((j): j is DownloadJob => !!j && j.status !== 'canceled')
  if (!js.length || !js.some((j) => j.status === 'queued' || j.status === 'running')) return null
  return Math.floor(js.reduce((a, j) => a + (j.status === 'error' ? 0 : jobPct(j)), 0) / js.length)
})
onUnmounted(() => {
  clearInterval(dlTimer)
  window.removeEventListener('resize', checkOverflow)
  window.removeEventListener('resize', fitEps)
})
watch([curLine, () => lines.value.length], () => nextTick(fitEps))

const eps = computed(() => {
  if (!lines.value.length) return []
  const list = lines.value[curLine.value].eps
  return reverse.value ? [...list].reverse() : list
})

// 倒序时显示下标要换算回真实集下标
function isWatched(displayIdx: number): boolean {
  const line = lines.value[curLine.value]
  if (!line) return false
  const realIdx = reverse.value ? line.eps.length - 1 - displayIdx : displayIdx
  return watchedEps.value.has(realIdx)
}

const meta = computed(() => {
  if (!vod.value) return ''
  const parts = [vod.value.typeName, vod.value.year, vod.value.area, vod.value.remark].filter(Boolean)
  const line = parts.join(' · ')
  const actor = vod.value.actor ? '主演：' + vod.value.actor : ''
  return [line, actor].filter(Boolean).join(' · ') || '暂无资料'
})

function toggleFav() {
  if (!vod.value) return
  const f = {
    siteKey,
    siteName: vod.value.siteName || siteName,
    vodId,
    name: vod.value.name,
    pic: vod.value.pic
  }
  api.toggleFavorite(f).then((r) => {
    isFav.value = r.added
    app.showTip(r.added ? '已加入收藏' : '已取消收藏')
  })
}

function play(epIndex: number, resume = false) {
  if (!vod.value || !lines.value.length) return
  const line = lines.value[curLine.value]
  const realIndex = reverse.value ? line.eps.length - 1 - epIndex : epIndex
  const ep = line.eps[realIndex]
  if (!ep) return
  // 把整条线路的剧集写入播放列表，播放页才能「上一集/下一集」
  player.set(
    line.eps.map((e) => ({ name: e.name, url: e.url, urls: [...e.urls] })),
    realIndex,
    vod.value.name,
    false,
    false
  )
  player.setLines(lines.value, curLine.value)
  // 续播：播放上次看到的集时，从上次进度继续
  if (resume && realIndex === watchedEp.value && histPos.value > 0) {
    player.seek = histPos.value
  } else {
    player.seek = 0
  }
  router.push({
    name: 'play',
    query: {
      url: ep.url,
      title: `${vod.value.name} ${ep.name}`,
      site: siteKey,
      id: vodId,
      name: vod.value.name,
      pic: vod.value.pic || '',
      siteName: vod.value.siteName || '',
      from: '',
      epIndex: String(realIndex),
      vodTitle: vod.value.name,
      lineIdx: String(curLine.value),
      direct: line.direct ? '1' : '0'
    }
  })
}

// 提交下载。后端发现同名集已下载/正在下载时先返回 conflicts，这里弹确认框，
// 确认后带 force 重新提交（删除旧文件并重下）。
async function submitDownload(items: { name: string; urls: string[] }[]) {
  if (!vod.value || !items.length) return
  try {
    let r = await api.download(vod.value.name, items)
    if (r.conflicts?.length) {
      const c = r.conflicts
      const what = c.length === 1 ? `「${c[0]}」` : `${c.length} 集（${c.slice(0, 3).join('、')}${c.length > 3 ? '…' : ''}）`
      try {
        await ElMessageBox.confirm(
          `${what}已下载或正在下载。是否删除已有文件并重新下载？`,
          '重新下载',
          { confirmButtonText: '重新下载', cancelButtonText: '取消', type: 'warning' }
        )
      } catch {
        return // 取消
      }
      r = await api.download(vod.value.name, items, true)
    }
    app.showTip(`已加入下载队列：${r.added} 集 → ${r.dir}`)
    loadDl()
  } catch (e) {
    app.showTip(humanizeApiError(e))
  }
}

// 下载：当前线路的全部剧集（单集电影就是这一集）。每集带上它的备用地址，下载时逐个尝试。
function downloadAll() {
  if (!lines.value.length) return
  submitDownload(lines.value[curLine.value].eps.map((e) => ({ name: e.name, urls: [e.url, ...e.urls.filter((u) => u !== e.url)] })))
}

// ---------- 单集右键菜单：该集所有可用操作（不可用的置灰） ----------
interface CtxItem {
  label: string
  disabled: boolean
  run: () => void
}
const ctx = ref<{ x: number; y: number; idx: number } | null>(null)
function realIdxOf(displayIdx: number): number {
  const line = lines.value[curLine.value]
  return line && reverse.value ? line.eps.length - 1 - displayIdx : displayIdx
}
function epPayload(e: { name: string; url: string; urls: string[] }) {
  return { name: e.name, urls: [e.url, ...e.urls.filter((u) => u !== e.url)] }
}
function openMenu(ev: MouseEvent, displayIdx: number) {
  const W = 168
  const H = 220
  ctx.value = {
    x: Math.max(4, Math.min(ev.clientX, window.innerWidth - W - 4)),
    y: Math.max(4, Math.min(ev.clientY, window.innerHeight - H - 4)),
    idx: displayIdx
  }
}
function closeMenu() {
  ctx.value = null
}
async function redownloadEp(e: { name: string; url: string; urls: string[] }) {
  if (!vod.value) return
  try {
    await ElMessageBox.confirm(`「${e.name}」将删除已有文件并重新下载，是否继续？`, '重新下载', {
      confirmButtonText: '重新下载',
      cancelButtonText: '取消',
      type: 'warning'
    })
  } catch {
    return
  }
  try {
    const r = await api.download(vod.value.name, [epPayload(e)], true)
    app.showTip(`已加入下载队列：${r.added} 集 → ${r.dir}`)
    loadDl()
  } catch (err) {
    app.showTip(humanizeApiError(err))
  }
}
async function cancelEpDownload(name: string) {
  const j = dlByEp.value.get(name)
  if (!j) return
  try {
    await api.downloadCancel(j.id)
    loadDl()
  } catch (err) {
    app.showTip(humanizeApiError(err))
  }
}
async function dropEpCache(e: { name: string; url: string }) {
  try {
    await api.m3u8CacheDropMany([e.url])
    const gone = new Set(cachedUrls.value)
    gone.delete(e.url)
    cachedUrls.value = gone
    app.showTip(`已删除「${e.name}」的 m3u8 缓存`)
  } catch (err) {
    app.showTip('删除失败：' + humanizeApiError(err))
  }
  loadM3u8Cache()
}
const ctxItems = computed<CtxItem[]>(() => {
  const c = ctx.value
  const line = lines.value[curLine.value]
  if (!c || !line) return []
  const e = line.eps[realIdxOf(c.idx)]
  if (!e) return []
  const j = dlByEp.value.get(e.name)
  const active = !!j && (j.status === 'queued' || j.status === 'running')
  const local = !!localKinds.value[e.name]
  const convBusy = dlJobs.value.some(
    (x) => x.kind === 'convert' && x.vod === vod.value?.name && x.name === e.name && (x.status === 'queued' || x.status === 'running')
  )
  return [
    { label: '播放', disabled: false, run: () => play(c.idx) },
    { label: '下载', disabled: active || local, run: () => submitDownload([epPayload(e)]) },
    { label: '重新下载', disabled: !(active || local), run: () => redownloadEp(e) },
    { label: '取消下载', disabled: !active, run: () => cancelEpDownload(e.name) },
    {
      label: '转换为 MP4',
      disabled: !(hasFfmpeg.value && localKinds.value[e.name] === 'hls') || convBusy,
      run: () => openConvert([e.name])
    },
    { label: '删除 m3u8 缓存', disabled: !cachedUrls.value.has(e.url), run: () => dropEpCache(e) }
  ]
})
function runCtx(it: CtxItem) {
  if (it.disabled) return
  closeMenu()
  it.run()
}
function onKey(ev: KeyboardEvent) {
  if (ev.key === 'Escape') closeMenu()
}
onMounted(() => {
  window.addEventListener('click', closeMenu)
  window.addEventListener('scroll', closeMenu, true)
  window.addEventListener('resize', closeMenu)
  window.addEventListener('keydown', onKey)
})
onUnmounted(() => {
  window.removeEventListener('click', closeMenu)
  window.removeEventListener('scroll', closeMenu, true)
  window.removeEventListener('resize', closeMenu)
  window.removeEventListener('keydown', onKey)
})

// 续播上次进度
function resumePlay() {
  if (watchedEp.value < 0) return
  // 找到上次看的集在显示列表中的位置
  const line = lines.value[curLine.value]
  if (!line) return
  const displayIdx = reverse.value ? line.eps.length - 1 - watchedEp.value : watchedEp.value
  play(displayIdx, true)
}
</script>

<template>
  <div class="page">
    <TopBar />
    <div class="back" @click="router.back()">‹ 返回</div>

    <!-- 加载中：骨架屏 -->
    <div v-if="loading" class="detail-loading">
      <div class="left">
        <div class="skeleton-poster"></div>
      </div>
      <div class="right">
        <div class="skeleton-title"></div>
        <div class="skeleton-meta"></div>
        <div class="skeleton-meta"></div>
        <div class="skeleton-content"></div>
        <div class="skeleton-content"></div>
        <div class="skeleton-content short"></div>
      </div>
    </div>

    <!-- 详情内容 -->
    <ErrorState v-else-if="loadError" :message="loadError" @retry="loadDetail" />

    <div v-else-if="vod" class="detail">
      <div class="left">
        <div class="poster">
          <img v-if="!posterFailed && vod.pic" :src="imgUrl(vod.pic)" :alt="vod.name" @error="posterFailed = true" />
          <div v-else class="ph">
            <span class="ph-char">{{ vod.name ? vod.name[0] : '影' }}</span>
            <span class="ph-name">{{ vod.name }}</span>
          </div>
        </div>
      </div>
      <div class="right">
        <h1 class="title">{{ vod.name }}</h1>
        <div v-if="vod.siteName" class="src-line">
          点播源：<span class="src-name" :title="subsTitle">{{ vod.siteName }}</span>
        </div>
        <div v-if="meta" class="meta">{{ meta }}</div>
        <div v-if="vod.actor" class="meta">主演：{{ vod.actor }}</div>
        <div v-if="vod.director" class="meta">导演：{{ vod.director }}</div>

        <div v-if="vod.content" ref="contentEl" class="content" :class="{ expanded }">
          {{ vod.content }}
        </div>
        <el-button v-if="canExpand" link class="toggle" @click="expanded = !expanded">
          {{ expanded ? '收起' : '展开' }}
        </el-button>

        <div class="actions">
          <el-button v-if="histPos > 0" type="primary" @click="resumePlay">
            继续播放{{ watchedEp >= 0 ? ' · 第' + (watchedEp + 1) + '集' : '' }}
          </el-button>
          <el-button :type="histPos > 0 ? 'default' : 'primary'" @click="toggleFav">{{ isFav ? '已收藏' : '收藏' }}</el-button>
          <el-button v-if="hasFfmpeg && convertibleEps.length" :disabled="converting" @click="openConvert(convertibleEps)">
            {{ converting ? '转换中…' : '转换为 MP4（' + convertibleEps.length + ' 集）' }}
          </el-button>
          <el-button v-if="lines.length" @click="downloadAll">
            {{ dlOverall !== null ? '下载中 ' + dlOverall + '%' : lines[curLine].eps.length > 1 ? '下载全部 ' + lines[curLine].eps.length + ' 集' : '下载' }}
          </el-button>
        </div>
      </div>
    </div>

    <!-- 线路 -->
    <div v-if="lines.length" class="lines">
      <div class="lines-head">
        <span class="lines-label">线路</span>
        <div class="lines-tabs">
          <span
            v-for="(l, i) in lines"
            :key="i"
            class="chip"
            :class="{ active: curLine === i }"
            :title="lineTitle(l, i)"
            @click="curLine = i"
          >
            {{ lineLabel(l, i) }}
            <span
              v-if="m3u8Cache[i]"
              class="chip-x"
              title="删除这条线路已缓存的 m3u8"
              @click.stop="dropLineCache(l, i)"
            >×</span>
          </span>
        </div>
      </div>

      <!-- 剧集 -->
      <div class="eps-head">
        <span class="eps-title">剧集</span>
        <span class="eps-count">共 {{ lines[curLine].eps.length }} 集</span>
        <span v-if="lines[curLine].eps.length > 32" class="eps-hint">· 可上下滚动查看更多</span>
        <span v-if="watchedEp >= 0" class="eps-seen">上次看到第 {{ watchedEp + 1 }} 集</span>
        <span v-if="watchedEps.size" class="eps-seen">· 已看 {{ watchedEps.size }} 集</span>
        <span v-if="lines[curLine].eps.length > 1" class="eps-hint">· 右键/长按某集弹出操作菜单</span>
        <span class="eps-order" @click="reverse = !reverse">{{ reverse ? '正序' : '倒序' }}</span>
      </div>
      <div ref="epsEl" class="eps" :style="epsMaxH ? { maxHeight: epsMaxH + 'px' } : undefined">
        <span
          v-for="(ep, i) in eps"
          :key="i"
          class="ep-chip"
          :class="{ watched: isWatched(i) }"
          @click="play(i)"
          @contextmenu.prevent.stop="openMenu($event, i)"
        >
          {{ ep.name }}<em v-if="epDl(i)" class="ep-dl" :class="{ 'ep-dl-cache': epDl(i) === '已获取' }">{{ epDl(i) }}</em>
        </span>
      </div>
    </div>
    <div v-if="ctx" class="ctx-menu" :style="{ left: ctx.x + 'px', top: ctx.y + 'px' }" @click.stop @contextmenu.prevent.stop>
      <div
        v-for="it in ctxItems"
        :key="it.label"
        class="ctx-item"
        :class="{ disabled: it.disabled }"
        @click="runCtx(it)"
      >{{ it.label }}</div>
    </div>
    <el-dialog v-model="convDlg" title="转换为 MP4" width="420px" append-to-body>
      <p style="margin: 0 0 6px">将 {{ convNames.length }} 集已下载的视频（.ts + .jm3u8）无损转换为 MP4，转换后可直接用浏览器播放。</p>
      <p style="margin: 0">是否保留原始文件？</p>
      <template #footer>
        <el-button @click="convDlg = false">取消</el-button>
        <el-button @click="doConvert(false)">不保留（删除原文件）</el-button>
        <el-button type="primary" @click="doConvert(true)">保留原文件</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.back {
  padding: 8px 24px;
  color: var(--sub);
  cursor: pointer;
  font-size: 19px;
  font-weight: 600;
}
.detail {
  display: flex;
  gap: 24px;
  padding: 8px 24px 24px;
}
.poster {
  width: 220px;
  aspect-ratio: 2 / 3;
  border-radius: var(--radius);
  overflow: hidden;
  background: var(--card);
  flex-shrink: 0;
}
.poster img {
  width: 100%;
  height: 100%;
  object-fit: cover;
}
.poster .ph {
  width: 100%;
  height: 100%;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  background: linear-gradient(160deg, #26242b 0%, #1c1b1f 100%);
  padding: 12px;
  text-align: center;
}
.poster .ph .ph-char {
  font-size: 56px;
  font-weight: 700;
  color: rgba(255, 255, 255, 0.18);
  line-height: 1;
}
.poster .ph .ph-name {
  font-size: 13px;
  color: rgba(255, 255, 255, 0.5);
  word-break: break-all;
}
.right {
  flex: 1;
  min-width: 0;
}
.title {
  margin: 0 0 12px;
  font-size: 26px;
}
.meta {
  color: var(--sub);
  font-size: 14px;
  margin-bottom: 6px;
}
.src-line {
  color: var(--sub);
  font-size: 13px;
  margin-bottom: 8px;
}
.src-name {
  cursor: help;
  border-bottom: 1px dashed var(--sub);
}
.src-name:hover {
  color: #fff;
  border-bottom-color: #fff;
}
.content {
  color: var(--sub);
  font-size: 14px;
  line-height: 1.6;
  margin-top: 12px;
  display: -webkit-box;
  -webkit-line-clamp: 3;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
.content.expanded {
  -webkit-line-clamp: unset;
}
.toggle {
  padding: 0;
  color: var(--selected);
}
.actions {
  margin-top: 16px;
}
.lines {
  padding: 0 24px 24px;
}
.lines-head {
  display: flex;
  align-items: center;
  gap: 16px;
  margin-bottom: 14px;
}
.lines-label {
  font-size: 20px;
  font-weight: 700;
}
.chip-x {
  display: inline-block;
  margin-left: 6px;
  padding: 0 4px;
  line-height: 1.1;
  border-radius: 50%;
  font-size: 14px;
  opacity: 0.6;
  cursor: crosshair;
}
.chip-x:hover {
  opacity: 1;
  background: rgba(245, 108, 108, 0.25);
  color: #f56c6c;
}
.lines-tabs {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  flex: 1;
}
.eps-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 12px;
}
.eps-title {
  font-size: 20px;
  font-weight: 700;
}
.eps-count {
  font-size: 13px;
  color: var(--dim);
}
.eps-hint {
  font-size: 12px;
  color: var(--dim);
}
.eps-seen {
  font-size: 13px;
  color: var(--selected);
}
.eps-order {
  margin-left: auto;
  font-size: 13px;
  color: var(--sub);
  cursor: pointer;
}
.eps-order:hover {
  color: #fff;
}
.eps {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(120px, 1fr));
  gap: 8px;
  max-height: 220px;
  overflow-y: auto;
}
.ep-chip {
  position: relative;
  padding: 8px 12px 12px;
  border-radius: var(--radius-sm);
  background: var(--white-15);
  font-size: 13px;
  text-align: center;
  cursor: pointer;
  transition: all 0.14s;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.ctx-menu {
  position: fixed;
  z-index: 3000;
  min-width: 150px;
  padding: 4px 0;
  border-radius: 8px;
  background: #2a2a31;
  border: 1px solid rgba(255, 255, 255, 0.12);
  box-shadow: 0 6px 20px rgba(0, 0, 0, 0.45);
  font-size: 13px;
}
.ctx-item {
  padding: 7px 16px;
  cursor: pointer;
  white-space: nowrap;
}
.ctx-item:hover {
  background: rgba(255, 255, 255, 0.1);
}
.ctx-item.disabled {
  opacity: 0.38;
  cursor: not-allowed;
}
.ctx-item.disabled:hover {
  background: transparent;
}
.ep-chip:hover {
  background: var(--card-hi);
}
.ep-dl {
  position: absolute;
  right: 4px;
  bottom: 1px;
  font-style: normal;
  font-size: 10px;
  line-height: 1;
  color: #409eff;
}
.ep-dl-cache {
  color: #909399;
}
.ep-chip.watched {
  color: var(--selected);
  border: 1px solid var(--selected);
}

/* ===== 移动端响应式 ===== */
@media (max-width: 640px) {
  .back { padding: 8px 16px; }
  /* 海报+详情从水平双栏 → 垂直单栏 */
  .detail {
    flex-direction: column;
    gap: 16px;
    padding: 8px 16px 16px;
  }
  .poster {
    width: 100%;
    max-width: 200px;
    align-self: center;
  }
  .title { font-size: 20px; }
  /* 剧集网格：手机 3 列 */
  .eps { grid-template-columns: repeat(3, 1fr); }
  /* 操作按钮换行 */
  .lines-head, .eps-head {
    flex-wrap: wrap;
    gap: 8px;
  }
  /* 线路 tab 横滑 */
  .lines-tabs {
    overflow-x: auto;
    white-space: nowrap;
    flex-wrap: nowrap;
    padding-bottom: 4px;
  }
  /* 骨架屏移动端 */
  .detail-loading {
    flex-direction: column;
    padding: 0 12px;
  }
  .detail-loading .left {
    width: 100%;
    max-width: 200px;
    margin: 0 auto;
  }
}

/* 详情页骨架屏 */
@keyframes shimmer {
  0% { background-position: -200% 0; }
  100% { background-position: 200% 0; }
}

.detail-loading {
  display: flex;
  gap: 24px;
  padding: 0 24px;
}

.detail-loading .left {
  width: 240px;
  flex-shrink: 0;
}

.detail-loading .right {
  flex: 1;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.skeleton-poster {
  aspect-ratio: 2 / 3;
  border-radius: var(--radius);
  background: linear-gradient(90deg, var(--card) 25%, var(--card-hi) 50%, var(--card) 75%);
  background-size: 200% 100%;
  animation: shimmer 1.4s infinite;
}

.skeleton-title {
  height: 32px;
  width: 60%;
  border-radius: 6px;
  background: linear-gradient(90deg, var(--card) 25%, var(--card-hi) 50%, var(--card) 75%);
  background-size: 200% 100%;
  animation: shimmer 1.4s infinite;
}

.skeleton-meta {
  height: 16px;
  width: 40%;
  border-radius: 4px;
  background: linear-gradient(90deg, var(--card) 25%, var(--card-hi) 50%, var(--card) 75%);
  background-size: 200% 100%;
  animation: shimmer 1.4s infinite 0.2s;
}

.skeleton-content {
  height: 14px;
  width: 100%;
  border-radius: 4px;
  background: linear-gradient(90deg, var(--card) 25%, var(--card-hi) 50%, var(--card) 75%);
  background-size: 200% 100%;
  animation: shimmer 1.4s infinite 0.4s;
}

.skeleton-content.short {
  width: 70%;
}
</style>
