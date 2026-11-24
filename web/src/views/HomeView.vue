<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { useRouter } from 'vue-router'
import { api, imgUrl } from '@/api'
import { useAppStore } from '@/stores/app'
import { fmtMs } from '@/utils/format'
import type { Vod, HistItem, Category } from '@/types'
import TopBar from '@/components/TopBar.vue'
import VodGrid from '@/components/VodGrid.vue'
import SkeletonList from '@/components/SkeletonList.vue'
import { gridCols } from '@/gridPrefs'
import { feedLimit, feedSort, setFeedSort, FEED_SORTS } from '@/itemPrefs'

const router = useRouter()
const app = useAppStore()

const history = ref<HistItem[]>([])
const feed = ref<Vod[]>([])
const feedSummary = ref('')
// 各指标下每个站源自己排好序的下标由后端算好（只存下标，站源之间不混排：各站计数
// 口径不同，跨站比大小没意义）。这里按名次轮流从每个站源各取一条合并：
// 外层循环名次，内层循环站源。站源不提供的指标不会出现在对应列表里。
// 'orig'（站点排序）直接用原始顺序。后端没给 index（旧缓存）时退回原始顺序。
type SiteRank = { s: string; i: number[] }
const feedIndex = ref<Record<string, SiteRank[]> | undefined>()
const feedPool = ref<Vod[]>([]) // 各榜单用到、但不在首页轮转列表里的候选，下标接在 feed 后面
const feedSorted = computed<Vod[]>(() => {
  const k = feedSort.value
  if (k === 'orig' || !feedIndex.value) return feed.value
  const ranks = feedIndex.value[k]
  if (!ranks) return []
  const nf = feed.value.length
  const lim = feedLimit.value
  const out: Vod[] = []
  const seen = new Set<string>()
  const seenName = new Set<string>()
  const depth = Math.max(0, ...ranks.map((r) => (Array.isArray(r?.i) ? r.i.length : 0)))
  for (let d = 0; d < depth; d++) {
    for (const r of ranks) {
      const i = r?.i?.[d]
      if (i === undefined) continue
      const v = i < nf ? feed.value[i] : feedPool.value[i - nf]
      if (!v) continue
      const key = v.name + '|' + (v.year || '')
      // 跨站同名同年（年份缺省当通配）只留先到的那条
      if (seen.has(key) || (!v.year && seenName.has(v.name)) || (v.year && seen.has(v.name + '|'))) continue
      seen.add(key)
      seenName.add(v.name)
      out.push(v)
      if (lim > 0 && out.length >= lim) return out
    }
  }
  return out
})
const feedShown = computed(() => (feedLimit.value > 0 ? feedSorted.value.slice(0, feedLimit.value) : feedSorted.value))
const feedTitle = computed(() => FEED_SORTS.find((s) => s.k === feedSort.value)?.title || '最近更新')
const loading = ref(true)
const hasSites = ref(false)
const confirmClear = ref(false)
let confirmTimer = 0

// 首页分类导航：取第一个可用站源的顶层分类
const cats = ref<Category[]>([])
const catSiteKey = ref('')

async function loadCats() {
  try {
    const sites = await api.sites()
    const usable = sites.filter((s) => s.usable)
    if (!usable.length) return
    catSiteKey.value = usable[0].key
    const all = await api.categories(usable[0].key)
    cats.value = all.filter((c) => c.pid === '' && c.id !== '').slice(0, 12)
  } catch {
    /* ignore */
  }
}

function goCat(tid: string) {
  router.push({ name: 'vod', query: { site: catSiteKey.value, tid } })
}

async function loadFeed() {
  try {
    // 网盘历史和点播历史是两套存储（/api/webdav/history vs /api/history）。
    // 首页「继续观看」原来只读后者，所以在网盘里看过的片子在这里永远看不到，
    // 用户会以为「网盘没记录播放历史」。这里把两边合并，按时间倒序。
    const [h, f, wd] = await Promise.all([
      api.history(),
      api.feed(),
      api.webdavHistory().catch(() => [])
    ])
    // 过滤掉影片名为空的记录（来自旧格式或测试遗留）
    const normal = h.filter((x: HistItem) => x.name && x.name.trim())
    // 网盘记录转成首页卡片能用的形状。用 siteKey='webdav' 做标记，
    // 点击时走网盘续播而不是跳详情页。
    const wdItems: HistItem[] = (wd || [])
      .filter((x: any) => x.vodName && x.pos > 0)
      .map((x: any) => ({
        siteKey: 'webdav',
        siteName: x.srcName || '网盘',
        vodId: `${x.srcId}:${x.vodName}`,
        name: x.vodName,
        pic: '',
        epName: x.epName || '',
        epIndex: x.epIndex,
        from: '',
        pos: x.pos,
        dur: x.dur || 0,
        time: x.time || 0
      } as HistItem))
    history.value = [...normal, ...wdItems].sort((a, b) => (b.time || 0) - (a.time || 0))
    feed.value = f.feed
    feedIndex.value = f.index
    feedPool.value = f.pool || []
    feedSummary.value = f.summary
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  await app.loadStatus()
  hasSites.value = (app.status?.totalSites ?? 0) > 0
  await loadFeed()
  loadCats()
  // 问一下后台要不要补跑一轮抓取（已经在跑 / 没到 24 小时间隔都不会真的
  // 现抓）。只有「这次真的有活在后台跑」时才去轮询进度——撞上「配置了
  // 站源但一条都还没抓到」的时候（比如服务刚重启，后台这轮正好在跑），
  // 不能干等着什么都不做：之前的写法只在用户手动点「更新」时才会轮询
  // 进度、抓完了重载 Feed，首次进首页撞上这个空窗口的话，这个页面就会
  // 一直空着，必须手动切走再切回来（重新 mount 一次）才会再 fetch 一次。
  // 这里补上同样的轮询，让「等一等」真的有用。
  api.triggerCrawl().then((r) => {
    if (r.started || r.running) pollCrawlUntilDone()
  }).catch(() => {})
})

// 监听订阅/站源变更信号，自动重载 Feed 和状态
watch(() => app.feedVersion, async () => {
  loading.value = true
  await app.loadStatus()
  hasSites.value = (app.status?.totalSites ?? 0) > 0
  await loadFeed()
})

// 进度钳制：>0.99 视为看完（不再显示继续观看），<0.02 抬到 0.02（区分「刚开始看」与「没看过」）
function histRatio(h: HistItem): number {
  if (!h.dur || h.dur <= 0 || h.pos <= 0) return 0
  const r = h.pos / h.dur
  if (r > 0.99) return 0
  return r < 0.02 ? 0.02 : r
}

function progressOf(h: HistItem): number {
  return histRatio(h)
}

// 相对时间
function ago(ms: number): string {
  if (!ms) return ''
  const diff = Date.now() - ms
  const m = Math.floor(diff / 60000)
  if (m < 1) return '刚刚'
  if (m < 60) return m + ' 分钟前'
  const hh = Math.floor(m / 60)
  if (hh < 24) return hh + ' 小时前'
  const d = Math.floor(hh / 24)
  return d + ' 天前'
}

function continueVods(): Vod[] {
  return history.value
    .filter((h) => h.pos > 0)
    .slice(0, 20)
    .map((h) => ({
      id: h.vodId,
      name: h.name,
      pic: h.pic,
      remark: '',
      year: '',
      area: '',
      actor: '',
      director: '',
      content: '',
      typeName: '',
      from: h.from,
      playUrl: '',
      siteKey: h.siteKey,
      siteName: h.siteName
    }))
}

function openHistory(h: HistItem) {
  // 网盘记录没有点播详情页，直接交给网盘页的续播流程：
  // WebdavView 支持 ?continue=<srcId>:<vodName> 自动续播。
  if (h.siteKey === 'webdav') {
    router.push({ name: 'webdav', query: { continue: h.vodId } })
    return
  }
  router.push({ name: 'detail', query: { site: h.siteKey, siteName: h.siteName, id: h.vodId } })
}

// 真实 <a href>：中键/ctrl/cmd+点击让浏览器原生在新标签页打开，跟
// openHistory 的目标路由保持一致（VodCard.href 同样的理由）。
function historyHref(h: HistItem): string {
  if (h.siteKey === 'webdav') {
    return router.resolve({ name: 'webdav', query: { continue: h.vodId } }).href
  }
  return router.resolve({ name: 'detail', query: { site: h.siteKey, siteName: h.siteName, id: h.vodId } }).href
}

function onContinueClick(h: HistItem, e: MouseEvent) {
  if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
  e.preventDefault()
  openHistory(h)
}

async function removeHist(h: HistItem) {
  if (h.siteKey === 'webdav') {
    const [srcId, ...rest] = h.vodId.split(':')
    await api.removeWdHistory({ srcId, vodName: rest.join(':') })
    await loadFeed()
    app.showTip('已删除「' + h.name + '」')
    return
  }
  await api.removeHistory(h)
  await loadFeed()
  app.showTip('已删除「' + h.name + '」')
}

async function clearHist() {
  if (!history.value.length) return
  if (!confirmClear.value) {
    confirmClear.value = true
    app.showTip(`再点一次确认清空（共 ${history.value.length} 条）`)
    if (confirmTimer) clearTimeout(confirmTimer)
    confirmTimer = window.setTimeout(() => (confirmClear.value = false), 6000)
    return
  }
  confirmClear.value = false
  if (confirmTimer) clearTimeout(confirmTimer)
  const n = history.value.length
  // 两套历史都要清，否则点「清空」后网盘那几条还留在列表里
  await Promise.all([
    api.clearHistory().catch(() => {}),
    api.clearWebdavHistory().catch(() => {})
  ])
  history.value = []
  app.showTip('已清空 ' + n + ' 条记录')
}

// 内容更新进度
const updating = ref(false)
const updatePct = ref(0)
let updateTimer = 0

// 轮询抓取进度直到跑完，再重载一次 Feed。refresh() 点「更新」和 onMounted
// 撞上「还没抓到任何数据」的空窗口时共用这一套逻辑。
function pollCrawlUntilDone() {
  if (updateTimer) return
  updating.value = true
  updatePct.value = 0
  updateTimer = window.setInterval(async () => {
    try {
      const r = await api.crawlSummary()
      if (r.total > 0) {
        updatePct.value = Math.min(100, Math.round((r.processed / r.total) * 100))
      }
      if (!r.running) {
        clearInterval(updateTimer)
        updateTimer = 0
        updating.value = false
        updatePct.value = 0
        await loadFeed()
        if (r.summary) app.showTip(r.summary)
      }
    } catch {
      clearInterval(updateTimer)
      updateTimer = 0
      updating.value = false
      updatePct.value = 0
    }
  }, 800)
}

function refresh() {
  if (updating.value) return
  api.triggerCrawl().then((r) => {
    if (r.started || r.running) {
      pollCrawlUntilDone()
      return
    }
    // 没到 24 小时间隔，后台没有真的再去抓一轮——内容本来就还是最新的，
    // 不用假装刷新了一下。
    app.showTip('内容已是最新，不需要重新抓取')
  })
}
</script>

<template>
  <div class="page page--fixed">
    <div class="tab-head">
      <TopBar />
      <!-- 分类导航 -->
      <div v-if="cats.length" class="cat-nav">
        <span
          v-for="c in cats"
          :key="c.id"
          class="cat-chip"
          @click="goCat(c.id)"
        >{{ c.name }}</span>
      </div>
    </div>

    <div class="tab-body">
    <!-- 继续观看 -->
    <template v-if="loading">
      <div class="section-title">继续观看</div>
      <SkeletonList type="card" :count="6" />
    </template>
    <template v-else-if="history.length">
      <div class="section-title">
        继续观看
        <span class="count">{{ history.length }} 部</span>
        <div class="title-right">
          <span class="more" :class="{ danger: confirmClear }" @click="clearHist">{{ confirmClear ? '确认清空？' : '清空' }}</span>
          <span class="more" @click="router.push('/history')">全部 ›</span>
        </div>
      </div>
      <div class="continue-row" :style="{ gridTemplateColumns: `repeat(${gridCols}, 1fr)` }">
        <a
          v-for="h in history.slice(0, 20)"
          :key="h.siteKey + h.vodId"
          class="continue-item"
          :href="historyHref(h)"
          @click="onContinueClick(h, $event)"
        >
          <div class="poster">
            <img v-if="h.pic" :src="imgUrl(h.pic)" :alt="h.name" loading="lazy" />
            <!-- 网盘记录没有封面图，用图标占位，避免一片空白让人以为加载失败 -->
            <div v-else class="poster-ph" aria-hidden="true">
              <span>{{ h.siteKey === 'webdav' ? '☁' : '🎬' }}</span>
            </div>
            <div v-if="histRatio(h) > 0" class="progress"><div class="bar" :style="{ width: histRatio(h) * 100 + '%' }"></div></div>
            <!-- 下面 c-meta 那行灰字已经有「第 01 集」了，这里不重复显示当前
                 第几集，改成总集数，两处信息互补而不是重叠。单集/未知
                 （epCount <= 1）不显示，跟 VodCard 的「(共N集)」同样处理。 -->
            <div v-if="h.epCount && h.epCount > 1" class="ep">共{{ h.epCount }}集</div>
            <div class="del" @click.stop.prevent="removeHist(h)">✕</div>
          </div>
          <div class="name">{{ h.name }}</div>
          <div class="c-meta">
            <span v-if="h.epName">{{ h.epName }} · </span>
            <span v-if="histRatio(h) > 0">已看 {{ Math.round(histRatio(h) * 100) }}%</span>
            <span v-if="ago(h.time)"> · {{ ago(h.time) }}</span>
          </div>
        </a>
      </div>
    </template>

    <!-- 最近更新 -->
    <div class="section-title">
      {{ feedTitle }}
      <span v-if="feed.length" class="count">{{ feedShown.length }} 条</span>
      <span v-if="feedSummary" class="summary">{{ feedSummary }}</span>
      <el-select :model-value="feedSort" size="small" class="sort-sel" :teleported="true" @update:model-value="setFeedSort">
        <el-option v-for="s in FEED_SORTS" :key="s.k" :label="s.n" :value="s.k" />
      </el-select>
      <span class="more" :class="{ updating }" @click="refresh">{{ updating ? `更新中 ${updatePct}%` : '更新 ›' }}</span>
    </div>
    <VodGrid :vods="feedShown" :loading="loading" />
    <div v-if="!loading && feed.length && !feedShown.length" class="empty">
      <p>没有站源提供「{{ FEED_SORTS.find((s) => s.k === feedSort)?.n }}」这项数据</p>
      <p class="hint">换一个排序方式试试</p>
    </div>
    <div v-if="!loading && !feed.length" class="empty">
      <template v-if="hasSites">
        <p>暂无内容，正在拉取最新影片…</p>
        <p class="hint">也可以点上方「点播」按分类浏览，或到「设置 → 内容更新 → 立即更新」</p>
      </template>
      <template v-else>
        <p>暂无内容</p>
        <p class="hint">可以进「设置」点「订阅」，一个链接同时导入点播 + 直播源</p>
        <el-button type="primary" @click="router.push('/settings')">去订阅</el-button>
      </template>
    </div>
    </div>
  </div>
</template>

<style scoped>
.cat-nav {
  display: flex;
  gap: 8px;
  padding: 4px 24px 16px;
  overflow-x: auto;
  flex-wrap: wrap;
}
.cat-chip {
  padding: 5px 14px;
  border-radius: 16px;
  font-size: 13px;
  color: var(--sub);
  background: var(--card);
  cursor: pointer;
  white-space: nowrap;
  transition: all 0.14s;
}
.cat-chip:hover {
  color: #fff;
  background: var(--card-hi);
}
.more.updating {
  color: var(--live, #ff6b6b);
  cursor: default;
}
.continue-row {
  display: grid;
  /* 列数由 gridPrefs.ts 的 gridCols 通过 inline style 决定（跟 VodGrid 的
     「最近更新」保持一致），这里的 auto-fill 只是 inline style 还没挂上
     那一帧的兜底，不要再写死列数。 */
  grid-template-columns: repeat(auto-fill, minmax(180px, 1fr));
  gap: 8px;
  padding: 0 12px;
}
.continue-item {
  /* .name 是单行 nowrap + ellipsis：不设 min-width:0 的话，网格给每一项算
     「自动最小宽度」时会按它未截断的整行文字宽度算（grid item 自身
     overflow 默认 visible，子元素的 overflow:hidden 不会把这个尺寸盖掉），
     标题越长那一列的 1fr 轨道就被撑得越宽，几列宽度完全不一致，
     表现成「一列大一列小」。VodCard 用的是两行 line-clamp（允许换行），
     中文逐字可断行，min-content 本来就很小，不会触发这个问题。 */
  min-width: 0;
  cursor: pointer;
  color: inherit;
  text-decoration: none;
}
.continue-item .poster {
  position: relative;
  aspect-ratio: 16 / 9;
  border-radius: var(--radius);
  overflow: hidden;
  background: var(--card);
  border: 2px solid transparent;
}
.continue-item:hover .poster {
  border-color: #fff;
}
.poster img {
  width: 100%;
  height: 100%;
  object-fit: cover;
}
/* 网盘记录没有封面：给个居中图标占位 */
.poster-ph {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 30px;
  opacity: 0.55;
}
.progress {
  position: absolute;
  left: 0;
  right: 0;
  bottom: 0;
  height: 4px;
  background: rgba(255, 255, 255, 0.2);
}
.progress .bar {
  height: 100%;
  background: var(--live);
}
.ep {
  position: absolute;
  right: 6px;
  bottom: 6px;
  font-size: 11px;
  color: #fff;
  background: rgba(0, 0, 0, 0.5);
  padding: 2px 6px;
  border-radius: 4px;
}
.del {
  position: absolute;
  right: 6px;
  top: 6px;
  width: 24px;
  height: 24px;
  border-radius: 50%;
  background: rgba(0, 0, 0, 0.6);
  display: flex;
  align-items: center;
  justify-content: center;
  color: #fff;
  font-size: 12px;
  opacity: 0;
  transition: opacity 0.14s;
}
.continue-item:hover .del {
  opacity: 1;
}
.del:hover {
  background: var(--live);
}
.name {
  margin-top: 6px;
  font-size: 14px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.c-meta {
  font-size: 12px;
  color: var(--dim);
  margin-top: 2px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.sort-sel {
  width: 140px;
  margin: 0 10px 0 auto; /* 靠右，紧挨着「更新」 */
  flex: none;
  vertical-align: middle;
  font-weight: normal;
}
/* 高度/字号跟小节标题（桌面 24px、≤640px 手机 18px）对齐 */
.sort-sel :deep(.el-select__wrapper) {
  min-height: 34px;
  font-size: 18px;
  padding: 0 12px;
}
@media (max-width: 640px) {
  .sort-sel {
    width: 108px;
  }
  .sort-sel :deep(.el-select__wrapper) {
    min-height: 26px;
    font-size: 14px;
    padding: 0 8px;
  }
}
.sort-sel + .more {
  margin-left: 0;
}
.summary {
  font-size: 12px;
  color: var(--dim);
  flex: 1;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.title-right {
  margin-left: auto;
  display: flex;
  align-items: center;
  gap: 12px;
}
/* 取消 .more 自带的 margin-left: auto，由父容器统一控制 */
.title-right .more {
  margin-left: 0;
}
.more.danger {
  color: var(--warn);
}
.empty {
  text-align: center;
  padding: 80px 24px;
  color: var(--dim);
}
.empty .hint {
  font-size: 13px;
  margin-bottom: 16px;
}

/* 移动端 */
@media (max-width: 640px) {
  .continue-row {
    gap: 5px;
    padding: 0 6px;
  }
  /* 移动端删除按钮始终可见（hover 在触摸屏无效） */
  .del {
    opacity: 1;
  }
}
</style>
