<script setup lang="ts">
import { ref, shallowRef, computed, onMounted, onUnmounted, onActivated, onDeactivated, watch } from 'vue'
import { useRouter } from 'vue-router'
import { api, humanizeApiError } from '@/api'
import type { LiveScanStatus } from '@/types'
import { useAppStore } from '@/stores/app'
import { usePlayerStore } from '@/stores/player'
import type { LiveDir, Channel, EpgResult, EpgProgram } from '@/types'
import TopBar from '@/components/TopBar.vue'
import SkeletonList from '@/components/SkeletonList.vue'
import VirtualGrid from '@/components/VirtualGrid.vue'

const router = useRouter()
const app = useAppStore()
const player = usePlayerStore()

const loading = ref(false)
const refreshing = ref(false)
// root/flatChannels 挂的是全部直播源合并后的频道目录，大的时候有十几万个
// 频道节点；用 shallowRef 只在整体替换时触发响应，不让 Vue 递归把每个
// 频道对象、每个 urls 数组都转成响应式 Proxy——这一步转换本身就是「停在
// 直播页 CPU 一直很高」的主因之一，而这里从没依赖过「改某个频道对象的
// 某个字段后界面自动更新」这种深层响应式（永远是整份替换）。
const root = shallowRef<LiveDir | null>(null)
const sourceCount = ref(0)
const selectedPath = ref<string[]>([]) // 当前分组路径
const deadSet = ref<Set<string>>(new Set())

const probing = ref(false)
const checkedCount = ref(0)
const filter = ref('')
// 检测代次：切台/重载频道时递增，用于中断旧的异步检测循环，防止结果污染新源
let probeGen = 0

// ===== 性能优化：频道索引缓存 =====
// 避免每次搜索都递归遍历全树，改用预构建的扁平列表 + 首字母索引
const flatChannels = shallowRef<Channel[]>([])
const channelIndex = shallowRef<Map<string, Channel[]>>(new Map())

// ===== 可播频道（后台扫描 + 真实播放共同喂数据，见 store.ChannelProbe）=====
// 默认就显示这个分类：打开直播页第一眼看到的是「已经确认能播」的频道，
// 不是未经筛选的全量频道目录。
//
// 数据来源是专门的轻量接口 /api/live/playable（只有几千条），而不是从
// 全量合并目录（root，十几万条）里筛——后者体积太大，之前就是靠它筛出
// 「可播频道」的，筛选本身很快，但光是把十几万条频道整个下载+反序列化+
// 转响应式对象就已经把浏览器 CPU 占满了。
const showPlayable = ref(true)
const playableRaw = shallowRef<Channel[]>([])
const scanStatus = ref<LiveScanStatus>({ total: 0, scanned: 0, playable: 0, scanning: false })
let scanPollTimer: ReturnType<typeof setInterval> | null = null
let scanPollTicks = 0

async function loadPlayable() {
  try {
    const r = await api.livePlayable()
    playableRaw.value = r.channels
  } catch {
    /* ignore：拉不到就留着上一次的列表，下一轮轮询再试 */
  }
}

async function pollScanStatus() {
  try {
    scanStatus.value = await api.liveScanStatus()
  } catch {
    /* ignore：纯展示用的进度条，拉不到就留着上一次的数字 */
  }
  // 扫描进度数字每 5 秒刷一次就够跟手；可播频道的具体列表（谁新变成了
  // ok）不用跟这么勤，放慢到四倍（20 秒），而且只在正看着这个分类时才
  // 刷——不是这个视图就没必要在背后悄悄拉。
  if (++scanPollTicks % 4 === 0 && showPlayable.value) loadPlayable()
}

function startScanPolling() {
  if (scanPollTimer) return
  scanPollTicks = 0
  pollScanStatus()
  scanPollTimer = setInterval(pollScanStatus, 5000)
}
function stopScanPolling() {
  if (scanPollTimer) { clearInterval(scanPollTimer); scanPollTimer = null }
}

const playableChannels = computed<Channel[]>(() => {
  const kw = filter.value.trim().toLowerCase()
  return kw ? playableRaw.value.filter((c) => c.name.toLowerCase().includes(kw)) : playableRaw.value
})

function showPlayableView() {
  showPlayable.value = true
  showFavs.value = false
  filter.value = ''
  selectedPath.value = []
  loadPlayable()
}

// 从「可播频道」里移除：清掉测试记录（变回「没测过」），不是标成播不通。
async function removeFromPlayable(ch: Channel) {
  try {
    await api.removeLiveChannelStatus(ch.name)
    // 本地直接从列表里摘掉，不用等下一轮轮询才消失
    playableRaw.value = playableRaw.value.filter((c) => c.name !== ch.name)
    app.showTip(`已从「可播频道」移除「${ch.name}」`)
    pollScanStatus()
  } catch (e: any) {
    app.showTip('移除失败：' + humanizeApiError(e))
  }
}

// ===== 直播频道收藏 =====
const liveFavs = ref<Channel[]>([])
const showFavs = ref(false) // 是否处于「我的频道」视图
const favKeys = computed(() => new Set(liveFavs.value.map((c) => c.name + '|' + c.url)))

function isFav(ch: Channel): boolean {
  return favKeys.value.has(ch.name + '|' + ch.url)
}

async function toggleFav(ch: Channel) {
  const r = await api.toggleLiveFav(ch)
  liveFavs.value = await api.liveFavs()
  app.showTip(r.added ? `已收藏「${ch.name}」` : `已取消收藏「${ch.name}」`)
}

function showMyFavs() {
  showFavs.value = true
  showPlayable.value = false
  filter.value = ''
  selectedPath.value = []
}

// 我的频道视图下的频道列表（按收藏顺序）
const favChannels = computed<Channel[]>(() => {
  if (!filter.value.trim()) return liveFavs.value
  const kw = filter.value.trim().toLowerCase()
  return liveFavs.value.filter((c) => c.name.toLowerCase().includes(kw))
})

const junkFilter = ref(true)
// 页面被 <keep-alive include="LiveView"> 缓存住了（见 App.vue）：从播放页
// 点「返回」回到这页时组件不会重新创建，只会重新 activate，搜索关键字/
// 分组筛选（filter/selectedPath）作为组件内部状态原样留着，不用额外存取。
// 首次挂载时 onMounted 自己会调一次 loadChannels，紧跟着触发的第一次
// onActivated 要跳过，不然首次打开页面会多发一次重复请求。
let skipNextActivate = true

onMounted(async () => {
  await loadChannels()
  // 加载收藏的直播频道
  liveFavs.value = await api.liveFavs().catch(() => [])
  const p = await api.prefs().catch(() => null)
  if (p) junkFilter.value = p.liveJunkFilter !== false
  // 默认视图就是「可播频道」，先主动拉一次，不等第一个 20 秒轮询节拍
  if (showPlayable.value) loadPlayable()
  startScanPolling()
})

// 从播放页返回时触发：静默刷新一次频道列表，捡起刚才那次播放写回的
// 红/绿播放状态；不重置 filter/selectedPath，也不弹骨架屏。
onActivated(() => {
  startScanPolling()
  if (skipNextActivate) {
    skipNextActivate = false
    return
  }
  if (showPlayable.value) {
    loadPlayable()
  } else {
    loadChannels(false, true)
  }
})

onDeactivated(() => {
  if (reloadTimer) { clearTimeout(reloadTimer); reloadTimer = null }
  stopScanPolling()
})

onUnmounted(() => {
  if (reloadTimer) clearTimeout(reloadTimer)
  stopScanPolling()
})

// force=true 时让后台把没过期的源缓存也重新抓一遍（「刷新」按钮用）；
// 不管 force 与否，接口本身永远立刻用现有缓存返回，不会等网络。
// silent=true 用于下面的自动跟进重载：安静地刷新数据，不打开骨架屏/不弹提示，
// 避免用户正在看列表时界面突然闪一下「加载中」。
async function loadChannels(force = false, silent = false) {
  if (!silent) {
    loading.value = true
    probeGen++ // 重载频道时中断正在进行的旧检测
  }
  try {
    const r = await api.liveChannels({ force })
    root.value = r.root
    sourceCount.value = r.sources || 0
    if (!silent) deadSet.value = new Set()
    // 还有源没抓到（首次加载中/上次失败）：服务端已经在后台重新拉取，
    // 过一会儿静默刷新一次列表，自动捡起新结果，不需要用户手动点刷新。
    scheduleFollowUpReload(Object.keys(r.errs || {}).length > 0)
  } catch (e: any) {
    if (!silent) app.showTip('直播源加载失败：' + humanizeApiError(e))
  } finally {
    if (!silent) loading.value = false
  }
}

let reloadTimer: ReturnType<typeof setTimeout> | null = null
let reloadAttempts = 0
function scheduleFollowUpReload(pending: boolean) {
  if (reloadTimer) { clearTimeout(reloadTimer); reloadTimer = null }
  if (!pending || reloadAttempts >= 4) {
    reloadAttempts = 0
    return
  }
  reloadAttempts++
  reloadTimer = setTimeout(() => { loadChannels(false, true) }, reloadAttempts * 6000)
}

async function refreshChannels() {
  refreshing.value = true
  try {
    await loadChannels(true)
    app.showTip('已在后台重新拉取全部直播源，列表会陆续自动更新')
  } finally {
    refreshing.value = false
  }
}

async function toggleJunkFilter(val: boolean) {
  await api.setPrefs({ liveJunkFilter: val }).catch(() => {})
  await loadChannels()
}

// 当前分组下的频道（选中路径下递归收集）
const currentDir = computed<LiveDir | null>(() => {
  if (!root.value) return null
  if (!selectedPath.value.length) return root.value
  let node = root.value
  for (const name of selectedPath.value) {
    const child = node.children.find((c) => c.name === name)
    if (!child) return root.value
    node = child
  }
  return node
})

// 构建频道索引（当分组改变时）
function buildChannelIndex() {
  flatChannels.value = []
  channelIndex.value.clear()
  if (currentDir.value) {
    collect(currentDir.value, flatChannels.value)
  }
  
  // 按首字母分桶索引，加速搜索
  for (const ch of flatChannels.value) {
    const firstChar = ch.name.charAt(0).toLowerCase()
    if (!channelIndex.value.has(firstChar)) {
      channelIndex.value.set(firstChar, [])
    }
    channelIndex.value.get(firstChar)!.push(ch)
  }
}

// 当 currentDir 变化时重建索引
watch(currentDir, () => {
  buildChannelIndex()
}, { deep: true })

// 优化搜索：先用首字母索引快速定位候选集，再精准过滤
const channels = computed<Channel[]>(() => {
  const kw = filter.value.trim().toLowerCase()
  if (!kw) return flatChannels.value
  
  // 用首字母快速查找候选集
  const firstChar = kw.charAt(0)
  const candidates = channelIndex.value.get(firstChar) || flatChannels.value
  
  // 在候选集内精准过滤
  return candidates.filter((c) => c.name.toLowerCase().includes(kw))
})

function collect(n: LiveDir, out: Channel[]) {
  out.push(...n.channels)
  n.children.forEach((c) => collect(c, out))
}

function selectGroup(path: string[]) {
  selectedPath.value = path
  showFavs.value = false
  showPlayable.value = false
}

function pathLabel(): string {
  if (!selectedPath.value.length) return '全部频道'
  return selectedPath.value.join(' › ')
}

// 当前视图实际展示的频道列表：三个互斥视图（可播频道 / 我的频道 / 分组）
// 共用的唯一入口，播放列表、换台上一曲/下一曲都从这份列表取。
const displayChannels = computed<Channel[]>(() => {
  if (showPlayable.value) return playableChannels.value
  if (showFavs.value) return favChannels.value
  return channels.value
})

function playChannel(ch: Channel) {
  const list = displayChannels.value
  const idx = list.findIndex((c) => c.name === ch.name && c.url === ch.url)
  player.set(
    list.map((c) => ({ name: c.name, url: c.url, urls: c.urls })),
    idx,
    '直播',
    true,
    false
  )
  router.push({ name: 'play', query: { url: ch.url, title: ch.name, site: 'live', id: '', from: 'live' } })
}

// 真实 <a href>：中键/ctrl/cmd+点击让浏览器原生在新标签页打开，
// 跟 playChannel 里 router.push 的目标路由保持一致。
function channelHref(ch: Channel): string {
  return router.resolve({
    name: 'play',
    query: { url: ch.url, title: ch.name, site: 'live', id: '', from: 'live' }
  }).href
}

function onChannelClick(ch: Channel, e: MouseEvent) {
  // 普通左键：拦住 <a> 的原生整页跳转，走 playChannel 里现成的 SPA 跳转
  // （还要顺带维护 player.eps 播放列表）。中键/ctrl/cmd/shift+左键交给
  // 浏览器原生处理（新标签页打开），这里什么都不做。
  if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
  e.preventDefault()
  playChannel(ch)
}

// 检测当前分组频道可用性
async function probeChannels() {
  if (!channels.value.length) {
    app.showTip('当前分组没有频道')
    return
  }
  if (probing.value) {
    app.showTip('正在检测，请稍等')
    return
  }
  probing.value = true
  checkedCount.value = 0
  const myGen = ++probeGen
  const list = channels.value.slice(0, 40)
  app.showTip('开始检测 ' + list.length + ' 个频道')
  let dead = 0
  for (const ch of list) {
    // 切台/重载后中断旧检测，防止旧源结果污染新源 deadSet
    if (myGen !== probeGen) break
    try {
      const r = await api.peek(ch.url)
      // await 期间可能已切台，再次校验
      if (myGen !== probeGen) break
      const key = ch.name + '|' + ch.url
      if (!r.alive) {
        dead++
        if (!deadSet.value.has(key)) {
          deadSet.value = new Set([...deadSet.value, key])
        }
      } else if (deadSet.value.has(key)) {
        const next = new Set(deadSet.value)
        next.delete(key)
        deadSet.value = next
      }
    } catch {
      /* ignore */
    }
    checkedCount.value++
  }
  // 检测被中断（切台/重载）：不更新状态、不提示结果
  if (myGen !== probeGen) return
  probing.value = false
  if (dead === 0) {
    app.showTip(`这 ${list.length} 台全部可连`)
  } else {
    app.showTip(`${list.length} 台里 ${dead} 台连不上，已标灰${channels.value.length > 40 ? '（只测了前 40 台）' : ''}`)
  }
}

function isDead(ch: Channel): boolean {
  return deadSet.value.has(ch.name + '|' + ch.url)
}

// ===== EPG 节目单 =====
const epgOpen = ref(false)
const epgUrl = ref('')
const epgChannel = ref('')
const epgLoading = ref(false)
const epgData = ref<EpgResult | null>(null)
const epgError = ref('')
const epgChannelOptions = computed<Channel[]>(() => flatChannels.value.slice(0, 800))

async function openEpg(ch?: Channel) {
  epgOpen.value = true
  if (!epgUrl.value) {
    const p = await api.prefs().catch(() => null)
    if (p) epgUrl.value = p.epgUrl || ''
  }
  if (ch) epgChannel.value = ch.name
  if (!epgChannel.value && flatChannels.value.length) epgChannel.value = flatChannels.value[0].name
  if (epgChannel.value && epgUrl.value) await loadEpg()
}

async function loadEpg() {
  if (!epgChannel.value) { app.showTip('请先选择频道'); return }
  epgLoading.value = true
  epgError.value = ''
  try {
    epgData.value = await api.liveEpg(epgChannel.value, epgUrl.value)
    if (!epgData.value.programs.length) epgError.value = '暂无该频道的节目单，可以检查频道名或 EPG 源'
  } catch (e) {
    epgError.value = humanizeApiError(e)
    epgData.value = null
  } finally {
    epgLoading.value = false
  }
}

async function saveEpgUrl() {
  await api.setPrefs({ epgUrl: epgUrl.value.trim() }).catch(() => {})
  app.showTip('EPG 地址已保存')
  if (epgChannel.value) await loadEpg()
}

function epgTime(ms: number): string {
  if (!ms) return '--:--'
  const d = new Date(ms)
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}
function epgRange(p: EpgProgram): string {
  return `${epgTime(p.start)} - ${epgTime(p.stop)}`
}
function isOnAir(p: EpgProgram): boolean {
  const n = Date.now()
  return p.start <= n && (p.stop === 0 || p.stop > n)
}
// 优先展示今天的节目，最多 120 条，避免大 EPG 卡顿
const epgVisible = computed<EpgProgram[]>(() => {
  const list = epgData.value?.programs || []
  const d = new Date(); d.setHours(0, 0, 0, 0)
  const today = list.filter(p => p.start >= d.getTime())
  return (today.length ? today : list).slice(0, 120)
})
</script>

<template>
  <div class="page page--fixed">
    <div class="tab-head">
    <TopBar />
    <div class="live-head">
      <div class="live-title">
        {{ root ? root.count + ' 台 · ' + root.children.length + ' 组' + (sourceCount ? ' · 合并自 ' + sourceCount + ' 个源' : '') : '直播' }}
        <span v-if="pathLabel() !== '全部频道'" class="path">{{ pathLabel() }}</span>
      </div>
      <div class="live-actions">
        <el-switch
          v-model="junkFilter"
          size="small"
          inline-prompt
          active-text="过滤垃圾频道"
          inactive-text="过滤垃圾频道"
          @change="(v: boolean | string | number) => toggleJunkFilter(v as boolean)"
        />
        <el-button size="small" :loading="probing" @click="probeChannels">检测</el-button>
        <el-button size="small" :loading="refreshing" @click="refreshChannels">刷新</el-button>
        <el-button size="small" @click="openEpg()">节目单</el-button>
      </div>
    </div>
    </div>

    <div class="tab-body tab-body--split">
    <div v-if="!root && !loading" class="empty">
      <p>暂无直播源</p>
      <p class="empty-sub">可以去「设置 → 订阅」导入订阅链接</p>
      <el-button type="primary" @click="router.push('/settings')">去设置</el-button>
    </div>

    <div v-else class="live-body" v-loading="loading">
      <!-- 左分组 -->
      <div class="groups">
        <div class="group playable-group" :class="{ active: showPlayable }" @click="showPlayableView">
          <span class="playable-dot">●</span> 可播频道
          <span class="cnt">{{ scanStatus.playable }}</span>
        </div>
        <div class="group" :class="{ active: !showFavs && !showPlayable && !selectedPath.length }" @click="selectGroup([])">
          全部频道
        </div>
        <div class="group fav-group" :class="{ active: showFavs }" @click="showMyFavs">
          <span class="fav-star">⭐</span> 我的频道
          <span v-if="liveFavs.length" class="cnt">{{ liveFavs.length }}</span>
        </div>
        <template v-if="root">
          <div v-for="g in root.children" :key="g.name">
            <div
              class="group"
              :class="{ active: !showFavs && selectedPath.length === 1 && selectedPath[0] === g.name }"
              @click="selectGroup([g.name])"
            >
              {{ g.name }} <span class="cnt">{{ g.count }}</span>
            </div>
            <div
              v-for="sub in g.children"
              :key="sub.name"
              class="group sub"
              :class="{ active: !showFavs && selectedPath.length === 2 && selectedPath[0] === g.name && selectedPath[1] === sub.name }"
              @click="selectGroup([g.name, sub.name])"
            >
              {{ sub.name }} <span class="cnt">{{ sub.count }}</span>
            </div>
          </div>
        </template>
      </div>

      <!-- 右频道 -->
      <div class="channels">
        <!-- ch-head 把「扫描进度条（可选）+ 搜索框」包成一个整体占第一个
             grid 行：scan-bar 是否渲染全凭 v-if，不会影响 ch-virtual 在
             grid-template-rows 里该落在哪一行。 -->
        <div class="ch-head">
          <!-- 可播频道分类：显示后台扫描进度 -->
          <div v-if="showPlayable" class="scan-bar">
            <span class="scan-text">
              {{ scanStatus.playable }} 可播 / {{ scanStatus.scanned }} 已测 / {{ scanStatus.total }} 总数
              <span v-if="scanStatus.scanning" class="scan-live">扫描中…</span>
            </span>
            <div class="scan-track">
              <div
                class="scan-fill"
                :style="{ width: (scanStatus.total ? (scanStatus.scanned / scanStatus.total * 100) : 0) + '%' }"
              ></div>
            </div>
          </div>
          <div class="ch-search">
            <el-input
              v-model="filter"
              size="small"
              clearable
              placeholder="搜索频道（如 CCTV / 卫视）"
              style="width: 260px"
            />
            <span v-if="filter" class="ch-search-count">{{ displayChannels.length }} 台</span>
          </div>
        </div>
        <!-- 加载中：骨架屏 -->
        <SkeletonList v-if="loading" type="channel" :count="30" />
        <!-- 我的频道空态 -->
        <div v-else-if="showFavs && !liveFavs.length" class="empty">
          <p>暂无收藏的频道</p>
          <p class="empty-sub">可以点频道右侧的 ⭐ 收藏常用频道</p>
        </div>
        <!-- 可播频道空态 -->
        <div v-else-if="showPlayable && !playableChannels.length" class="empty">
          <p>{{ scanStatus.scanning || scanStatus.scanned < scanStatus.total ? '后台正在扫描频道可播性，稍后自动出现…' : '暂无确认可播的频道' }}</p>
          <p class="empty-sub">播放成功过的频道，或后台扫描测出能播的频道，都会出现在这里</p>
        </div>
        <!-- 频道列表（虚拟滚动：上千频道只渲染可视区） -->
        <VirtualGrid
          v-else
          class="ch-virtual"
          max-height="100%"
          :items="displayChannels"
          :item-height="46"
          :item-min-width="150"
          :gap="8"
          :overscan="3"
        >
          <template #default="{ item: ch }">
            <a
              class="channel"
              :class="{ dead: isDead(ch) }"
              :href="channelHref(ch)"
              @click="onChannelClick(ch, $event)"
            >
              <span
                class="ch-name"
                :class="{ 'ch-name-ok': ch.status === 'ok', 'ch-name-fail': ch.status === 'fail' }"
                :title="ch.status === 'ok' ? '能播' : ch.status === 'fail' ? '播不通（24 小时后会自动重新测试）' : ''"
              >{{ ch.name }}</span>
              <span v-if="ch.urls.length > 1" class="ch-alts">{{ ch.urls.length }} 线路</span>
              <span v-if="showPlayable" class="ch-remove" title="从可播频道移除" @click.stop.prevent="removeFromPlayable(ch)">✕</span>
              <span class="ch-epg" title="查看节目单" @click.stop.prevent="openEpg(ch)">▤</span>
              <span
                class="ch-fav"
                :class="{ on: isFav(ch) }"
                :title="isFav(ch) ? '取消收藏' : '收藏频道'"
                @click.stop.prevent="toggleFav(ch)"
              >{{ isFav(ch) ? '★' : '☆' }}</span>
            </a>
          </template>
        </VirtualGrid>
      </div>
    </div>
    </div>

    <!-- EPG 节目单 -->
    <el-dialog v-model="epgOpen" title="节目单 (EPG)" width="620px" append-to-body>
      <div class="epg-bar">
        <el-select v-model="epgChannel" size="small" filterable placeholder="选择频道" style="width: 220px" @change="loadEpg">
          <el-option v-for="c in epgChannelOptions" :key="c.name + c.url" :label="c.name" :value="c.name" />
        </el-select>
        <el-button size="small" :loading="epgLoading" @click="loadEpg">刷新</el-button>
      </div>
      <div class="epg-bar">
        <el-input v-model="epgUrl" size="small" clearable placeholder="EPG 地址（XMLTV，如 https://.../epg.xml）" />
        <el-button size="small" type="primary" @click="saveEpgUrl">保存</el-button>
      </div>
      <div v-if="epgLoading" class="epg-tip">正在加载节目单…</div>
      <div v-else-if="epgError" class="epg-tip">{{ epgError }}</div>
      <template v-else-if="epgData && epgData.programs.length">
        <div v-if="epgData.now" class="epg-now">
          <span class="epg-now-tag">正在播出</span>
          <div class="epg-now-title">{{ epgData.now.title }}</div>
          <div class="epg-now-meta">
            {{ epgRange(epgData.now) }}
            <span v-if="epgData.next"> · 接下来：{{ epgData.next.title }}</span>
          </div>
        </div>
        <div class="epg-list">
          <div v-for="(p, i) in epgVisible" :key="i" class="epg-row" :class="{ on: isOnAir(p) }">
            <span class="epg-time">{{ epgRange(p) }}</span>
            <div class="epg-info">
              <div class="epg-name">{{ p.title }}</div>
              <div v-if="p.desc" class="epg-desc">{{ p.desc }}</div>
            </div>
          </div>
        </div>
      </template>
      <div v-else class="epg-tip">暂无节目单，可以填入 EPG 地址后点「保存」</div>
      <template #footer><el-button type="primary" @click="epgOpen = false">关闭</el-button></template>
    </el-dialog>
  </div>
</template>

<style scoped>
.live-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 8px 24px 16px;
}
.live-title {
  font-size: 20px;
  font-weight: 600;
}
.path {
  font-size: 14px;
  color: var(--sub);
  font-weight: 400;
  margin-left: 12px;
}
.live-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}
.live-body {
  display: flex;
  height: 100%;
  padding: 0 24px;
  gap: 16px;
}
.groups {
  width: 220px;
  flex-shrink: 0;
  height: 100%;
  overflow-y: auto;
}
.group {
  padding: 10px 14px;
  border-radius: 6px;
  cursor: pointer;
  color: var(--sub);
  font-size: 14px;
  display: flex;
  justify-content: space-between;
}
.group:hover {
  background: var(--card);
}
.group.active {
  color: var(--selected);
  background: var(--card-hi);
}
.group.sub {
  padding-left: 28px;
  font-size: 13px;
}
.cnt {
  color: var(--dim);
  font-size: 12px;
}
.channels {
  flex: 1;
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
  /* 第一行给搜索框按内容高度，剩下全部给频道区——频道区自己内部
     （VirtualGrid 的 .vg-viewport）滚动，这一层本身不再滚，避免跟
     里面的虚拟滚动区套出两层滚动条。 */
  grid-template-rows: auto 1fr;
  gap: 8px;
  align-content: start;
  height: 100%;
  overflow: hidden;
}
.ch-virtual { grid-column: 1 / -1; height: 100%; }
.ch-head {
  grid-column: 1 / -1;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.ch-search {
  display: flex;
  align-items: center;
  gap: 10px;
  padding-bottom: 4px;
}
.ch-search-count {
  font-size: 12px;
  color: var(--dim);
  white-space: nowrap;
}
/* ===== 可播频道：侧栏入口 + 扫描进度条 ===== */
.playable-group .playable-dot {
  font-size: 10px;
  color: #4caf50;
}
.playable-group.active {
  color: #4caf50;
}
.scan-bar {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.scan-text {
  font-size: 12px;
  color: var(--dim);
}
.scan-live {
  color: var(--selected);
  margin-left: 6px;
}
.scan-track {
  height: 4px;
  border-radius: 2px;
  background: var(--white-15);
  overflow: hidden;
}
.scan-fill {
  height: 100%;
  background: #4caf50;
  transition: width 0.3s ease;
}
.ch-remove {
  color: var(--dim);
  cursor: pointer;
  font-size: 13px;
  padding: 0 4px;
}
.ch-remove:hover { color: #f44336; }
.channel {
  padding: 12px;
  border-radius: 6px;
  background: var(--white-15);
  cursor: pointer;
  font-size: 14px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  transition: all 0.14s;
  color: inherit;
  text-decoration: none;
}
.channel:hover {
  background: var(--card-hi);
}
.channel.dead {
  color: var(--dim);
  text-decoration: line-through;
}
/* 播放状态上色：ok=真实播放成功过，fail=播放过但所有线路都失败过，
   没有这两个 class（status 未知/没播过）保持默认的当前文字颜色。*/
.ch-name-ok {
  color: #4caf50;
}
.ch-name-fail {
  color: #f44336;
}
.ch-alts {
  font-size: 11px;
  color: var(--dim);
}
.ch-fav {
  font-size: 15px;
  color: var(--dim);
  cursor: pointer;
  padding: 0 4px;
  transition: color 0.14s, transform 0.14s;
}
.ch-fav:hover {
  transform: scale(1.2);
}
.ch-fav.on {
  color: #ffd54f;
}
.fav-group .fav-star {
  font-size: 12px;
}
.fav-group.active {
  color: #ffd54f;
}
.empty {
  grid-column: 1 / -1;
  text-align: center;
  padding: 80px 24px;
  color: var(--dim);
}
.empty-sub {
  font-size: 12px;
  margin-top: 8px;
}
/* ===== 移动端响应式 ===== */
@media (max-width: 640px) {
  .top { padding: 8px 16px; gap: 8px; }
  .groups { 
    overflow-x: auto; 
    flex-wrap: nowrap;
    padding: 4px 12px 8px;
  }
  .group-btn { font-size: 13px; padding: 6px 12px; }
  .channels {
    grid-template-columns: repeat(2, 1fr);
    gap: 8px;
    padding: 0 12px;
  }
  .channel { padding: 10px; font-size: 13px; }
  .empty { padding: 60px 16px; }
}

/* ===== EPG 节目单 ===== */
.ch-epg {
  color: var(--dim);
  cursor: pointer;
  font-size: 13px;
  padding: 0 4px;
}
.ch-epg:hover { color: var(--selected); }
.epg-bar { display: flex; gap: 8px; margin-bottom: 10px; }
.epg-tip { color: var(--dim); text-align: center; padding: 24px; font-size: 13px; }
.epg-now {
  background: var(--card-hi);
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 12px 14px;
  margin-bottom: 12px;
}
.epg-now-tag {
  display: inline-block;
  background: var(--selected);
  color: #000;
  border-radius: 4px;
  font-size: 11px;
  padding: 1px 6px;
  margin-bottom: 6px;
}
.epg-now-title { font-size: 15px; font-weight: 600; }
.epg-now-meta { font-size: 12px; color: var(--dim); margin-top: 4px; }
.epg-list { max-height: 360px; overflow-y: auto; }
.epg-row {
  display: flex;
  gap: 12px;
  padding: 8px 6px;
  border-bottom: 1px solid var(--border);
  font-size: 13px;
}
.epg-row.on { color: var(--selected); background: var(--card); }
.epg-time { color: var(--dim); white-space: nowrap; font-variant-numeric: tabular-nums; }
.epg-info { min-width: 0; }
.epg-desc { color: var(--dim); font-size: 12px; margin-top: 2px; }
@media (max-width: 640px) {
  .epg-bar { flex-wrap: wrap; }
  .epg-bar .el-input { width: 100%; }
  .epg-row { flex-direction: column; gap: 2px; }
}
</style>
