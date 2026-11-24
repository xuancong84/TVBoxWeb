<script setup lang="ts">
import { ref, computed, onMounted, onActivated, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, humanizeApiError } from '@/api'
import { useAppStore } from '@/stores/app'
import { vodPerPage } from '@/itemPrefs'
import type { Site, Category, VodPage, FilterGroup, Vod } from '@/types'
import TopBar from '@/components/TopBar.vue'
import VodGrid from '@/components/VodGrid.vue'
import ErrorState from '@/components/ErrorState.vue'

const route = useRoute()
const router = useRouter()
const app = useAppStore()

const sites = ref<Site[]>([])
const siteKey = ref((route.query.site as string) || '')
const categories = ref<Category[]>([])
const page = ref<VodPage>({ items: [], page: 1, pageCount: 1, total: 0 })
const curPage = ref(1)
const selectedParent = ref('')
const selectedChild = ref('')
const filters = ref<FilterGroup[]>([])
const filterVals = ref<Record<string, string>>({})
// 片名关键词筛选：站源片单动辄几万条，只在本地已加载的这一页（二三十
// 条）里做字符串包含匹配基本搜不到东西，所以这其实是调用该站自己的搜索
// 接口（跟首页多站搜索共用同一个后端 SearchCtx），只是把范围收窄到当前
// 选中的这一个站。防抖 400ms 发请求，用 AbortController 让「还没回来的
// 上一次搜索」不会在「这一次」之后才回来把结果覆盖掉。
const keyword = ref('')
const searchResults = ref<Vod[] | null>(null) // null = 没在搜；[] = 搜了但没结果
const searching = ref(false)
const searchErr = ref('')
let searchTimer: ReturnType<typeof setTimeout> | undefined
let searchAbort: AbortController | undefined

const isSearching = computed(() => keyword.value.trim() !== '')
const displayItems = computed(() => (isSearching.value ? searchResults.value || [] : page.value.items))

function scheduleSiteSearch() {
  clearTimeout(searchTimer)
  searchAbort?.abort()
  const kw = keyword.value.trim()
  if (!kw) {
    searchResults.value = null
    searching.value = false
    searchErr.value = ''
    return
  }
  searchTimer = setTimeout(() => runSiteSearch(kw), 400)
}

async function runSiteSearch(kw: string) {
  if (!siteKey.value) return
  searchAbort = new AbortController()
  searching.value = true
  searchErr.value = ''
  try {
    searchResults.value = await api.vodSiteSearch(siteKey.value, kw, searchAbort.signal)
  } catch (e: any) {
    if (e?.name !== 'AbortError') {
      searchErr.value = humanizeApiError(e)
      searchResults.value = []
    }
  } finally {
    searching.value = false
  }
}

watch(keyword, scheduleSiteSearch)

const loading = ref(false)
// 分类列表单独一个 loading 标记，不能跟 loading 共用：分类和片单现在是并发
// 拉取的，共用一个标记会出现「分类先回来就把 loading 置 false，片单其实
// 还没回来」的竞态，画面会先闪一下「暂无内容」再变成真正的列表。
const catLoading = ref(false)
const loadError = ref('')

const currentSite = computed(() => sites.value.find((s) => s.key === siteKey.value))

// 下拉列表里站名前的小圆点：请求级健康度，跟「这个站有没有内容」无关，
// 是「最近调用这个站的接口（分类/列表/详情）顺不顺」——
//   绿 healthy：最近请求基本都成功，响应也不慢
//   黄 slow：错误率偏高（>50%）或响应偏慢（平均 >5s），接口还通但不稳
//   红 dead：超过 24 小时没成功过一次
//   灰 unknown：还从没在这个站发过请求（没点开看过），无数据可判断
const healthMap = ref<Record<string, string>>({})
const HEALTH_COLOR: Record<string, string> = {
  healthy: 'var(--live, #22c55e)',
  slow: '#eab308',
  dead: 'var(--danger, #ef4444)',
  unknown: 'var(--dim)'
}
function healthOf(key: string): string {
  return healthMap.value[key] || 'unknown'
}
// 先按最近一次内容更新时抓到的影片总数从多到少排；数量都是 0 的站（没内容）
// 再按 severity 从小到大排——「连上了确实空」比「这次压根没连上」靠前，
// 「这次没连上但 24 小时内成功过」又比「长期没成功过/已过期/完全失联」靠前。
const sortedSites = computed(() =>
  [...sites.value].sort((a, b) => b.count - a.count || a.severity - b.severity)
)
// 下拉里站源的颜色：设置里禁用=深红，启用且有内容=亮绿，启用但没内容=灰。
function siteState(s: { enabled: boolean; count: number }) {
  return !s.enabled ? 'st-off' : s.count > 0 ? 'st-on' : 'st-empty'
}
function refreshAll() {
  void loadHealth()
  load()
}
async function loadHealth() {
  const r = await api.sitesHealth()
  const m: Record<string, string> = {}
  for (const st of r.statuses) m[st.key] = st.status
  healthMap.value = m
}

// 顶层分类（pid 为空且 id 不为空，过滤掉后端注入的虚拟「全部」条目）
// 后端探测出的空分类 id（没有结果时为空集 = 显示全部分类，不增加任何等待）。
const emptyCats = ref<Set<string>>(new Set())
// 当前选中的分类（含从 URL tid 恢复的）永远可见；空父分类只有在它的子分类也全空时才藏。
const isHidden = (c: Category): boolean => {
  if (!emptyCats.value.has(c.id)) return false
  if (c.id === selectedParent.value || c.id === selectedChild.value) return false
  return !categories.value.some((k) => k.pid === c.id && !emptyCats.value.has(k.id))
}
const topCats = computed(() => categories.value.filter((c) => c.pid === '' && c.id !== '' && !isHidden(c)))
const childrenOf = (tid: string) => categories.value.filter((c) => c.pid === tid && !isHidden(c))

// 初始化期间 siteKey 的首次赋值不算「用户切换站点」，不能清空刚恢复的筛选
let restoring = true

// 记住上次选过的站，下次切到点播页默认直接用它；从没选过（或那个站没了）就退化成
// 内容最多的站（sortedSites 已按数量降序、severity 升序排好，取第一个即可）。
const LAST_SITE_KEY = 'yz_vod_last_site'

// 页面被 <keep-alive include="VodView"> 缓存住了（见 App.vue）：从详情页
// 点「返回」回到这页时组件不会重新创建，只会重新 activate。这个函数在
// 首次 onMounted、以及 onActivated 判定「不是单纯返回，而是带着新的
// 站/分类从别处（比如首页分类入口）跳进来」时都会被调用一遍，所以每次
// 进来都先清空筛选关键词/分类选中状态，再按本次 route.query 重新套一遍。
async function init() {
  selectedParent.value = ''
  selectedChild.value = ''
  filters.value = []
  filterVals.value = {}
  keyword.value = ''
  searchResults.value = null
  searching.value = false
  searchErr.value = ''
  // 从 URL 恢复筛选/页码，返回列表时不会丢失状态
  curPage.value = Math.max(1, Number(route.query.page) || 1)
  if (route.query.f) {
    try { filterVals.value = JSON.parse(route.query.f as string) } catch { /* ignore */ }
  }
  void loadHealth()
  const list = await api.sites()
  sites.value = list.filter((s) => s.usable)
  let qSite = (route.query.site as string) || ''
  // URL/记忆里的站已不在列表里（比如它所属的单仓源被停用了）：当作没指定
  if (qSite && !sites.value.some((s) => s.key === qSite)) qSite = ''
  if (siteKey.value && !sites.value.some((s) => s.key === siteKey.value)) siteKey.value = ''
  if (qSite) {
    siteKey.value = qSite
  } else if (!siteKey.value) {
    let last = ''
    try { last = localStorage.getItem(LAST_SITE_KEY) || '' } catch { /* ignore */ }
    if (last && sites.value.some((s) => s.key === last)) {
      siteKey.value = last
    } else if (sortedSites.value.length) {
      siteKey.value = sortedSites.value[0].key
    }
  }
  if (siteKey.value) {
    if (route.query.tid) {
      // 带着具体分类进来：分类/子分类归属要靠分类列表才能分清楚，这一种
      // 必须等分类回来才能发起正确的片单请求。
      await loadCategories()
      applyTidFromQuery()
    } else {
      // 最常见的情况——没指定分类，默认就是「全部」：片单请求不依赖分类
      // 列表，没必要白等一轮网络请求再去拿列表，两个并发着拉，谁先回来
      // 就先显示谁的那部分。
      void loadCategories()
      load()
    }
  }
}

onMounted(async () => {
  await init()
  restoring = false
})

// 首次挂载时 onMounted 自己会调一次 init，紧跟着触发的第一次 onActivated
// 要跳过，不然首次打开页面会多发一次重复请求（同 LiveView 的套路）。
let skipNextActivate = true

onActivated(async () => {
  if (skipNextActivate) {
    skipNextActivate = false
    return
  }
  // 页面是 keep-alive 缓存的：在设置里停用/启用单仓源后回到这里，站源下拉
  // 必须重新取一遍（有 ETag/缓存，几乎零成本）。当前站已经没了就整页重新初始化。
  try {
    const list = (await api.sites()).filter((s) => s.usable)
    sites.value = list
    if (siteKey.value && !list.some((s) => s.key === siteKey.value)) {
      restoring = true
      await init()
      restoring = false
      return
    }
  } catch { /* 保持旧列表 */ }
  const qSite = (route.query.site as string) || ''
  const qTid = (route.query.tid as string) || ''
  // route.query 跟当前状态完全一致：就是从详情页点「返回」回到刚才浏览
  // 的这同一个站/分类，筛选关键词和搜索结果原样留着，什么都不用做。
  if (qSite === siteKey.value && qTid === currentTid()) return
  // 否则是带着新的站/分类从别处（比如首页分类入口）跳进来，不是「返回」，
  // 要整页重新初始化。
  restoring = true
  await init()
  restoring = false
})

// 把站点 / 分类 / 筛选 / 页码同步进 URL，返回时由 scrollBehavior + query 完整还原
function syncQuery() {
  const q: Record<string, string> = { ...(route.query as Record<string, string>) }
  q.site = siteKey.value
  const tid = currentTid()
  if (tid) q.tid = tid; else delete q.tid
  if (curPage.value > 1) q.page = String(curPage.value); else delete q.page
  const fv = Object.fromEntries(Object.entries(filterVals.value).filter(([, v]) => v !== ''))
  if (Object.keys(fv).length) q.f = JSON.stringify(fv); else delete q.f
  router.replace({ query: q })
}

// 从 URL query 的 tid 参数选中对应分类
function applyTidFromQuery() {
  const tid = (route.query.tid as string) || ''
  const cat = tid ? categories.value.find((c) => c.id === tid) : null
  if (cat) {
    if (cat.pid === '') {
      selectedParent.value = tid
      selectedChild.value = ''
    } else {
      selectedParent.value = cat.pid
      selectedChild.value = tid
    }
  }
  load()
}

watch(siteKey, async () => {
  try { if (siteKey.value) localStorage.setItem(LAST_SITE_KEY, siteKey.value) } catch { /* ignore */ }
  if (restoring) return
  selectedParent.value = ''
  selectedChild.value = ''
  filterVals.value = {}
  filters.value = [] // 清空旧站源的筛选选项，等新站源分类加载后重建
  curPage.value = 1
  syncQuery()
  // 切站：selectedParent/selectedChild 已经清空成「全部」，片单请求不依赖
  // 分类列表，两个并发着拉，不用等分类先回来。
  void loadCategories()
  load()
  // 切到另一个站，筛选词还在的话要跟着换到新站重新搜，不然界面上明明
  // 显示着关键词，结果却还是刚才那个站的。
  if (isSearching.value) scheduleSiteSearch()
})

async function loadCategories() {
  catLoading.value = true
  try {
    categories.value = await api.categories(siteKey.value)
    void loadEmptyCats(siteKey.value)
  } catch (e: any) {
    app.showTip('分类加载失败：' + humanizeApiError(e))
  } finally {
    catLoading.value = false
  }
}

// 不 await：拿不到（后端还在后台探测）就保持显示全部，下次进来再生效。
async function loadEmptyCats(site: string) {
  emptyCats.value = new Set()
  try {
    const r = await api.categoriesEmpty(site)
    if (site === siteKey.value && r.ready) emptyCats.value = new Set(r.empty)
  } catch {
    /* 探测只是锦上添花，失败就当没有 */
  }
}

function selectParent(tid: string) {
  selectedParent.value = tid
  selectedChild.value = ''
  filterVals.value = {}
  curPage.value = 1
  syncQuery()
  load()
  // 更新筛选
  if (currentSite.value) {
    filters.value = parseFilters(currentSite.value.ext, tid)
  }
}

function selectChild(tid: string) {
  selectedChild.value = tid
  curPage.value = 1
  syncQuery()
  load()
}

function currentTid(): string {
  if (selectedChild.value) return selectedChild.value
  return selectedParent.value
}

function currentChildren(): string {
  if (selectedChild.value) return ''
  // 「全部」分类直接取全集，不聚合子分类
  if (!selectedParent.value) return ''
  return childrenOf(selectedParent.value).map((c) => c.id).join(',')
}

// 每页条数自定义：站源自己的分页大小 S 固定（取第 1 页的条数），
// 自定义第 p 页 = 总序号 [(p-1)N, pN)，换算成站源页 floor(start/S)+1 ..
// floor((end-1)/S)+1，并发拉这几页再拼起来切片。分类聚合（children）走的
// 是「父+子分类第 1 页合并」，不能这样换算，保持原样。
const srcSize = new Map<string, number>()
async function loadPage(ext: string): Promise<VodPage> {
  const site = siteKey.value
  const tid = currentTid()
  const children = currentChildren()
  const N = vodPerPage.value
  if (!N || children) return api.vodList(site, tid, curPage.value, ext, children)
  const key = site + '|' + tid + '|' + ext
  let p1: VodPage | null = null
  let S = srcSize.get(key) || 0
  if (!S) {
    p1 = await api.vodList(site, tid, 1, ext, '')
    S = p1.items.length
    if (!S) return p1
    srcSize.set(key, S)
  }
  const start = (curPage.value - 1) * N
  const from = Math.floor(start / S) + 1
  const to = Math.floor((start + N - 1) / S) + 1
  const first = p1
  const pgs = await Promise.all(
    Array.from({ length: to - from + 1 }, (_, i) => {
      const n = from + i
      return n === 1 && first ? Promise.resolve(first) : api.vodList(site, tid, n, ext, '')
    })
  )
  const last = pgs[pgs.length - 1]
  const all = pgs.flatMap((p) => p.items)
  const off = start - (from - 1) * S
  const total = last.total || last.pageCount * S
  return {
    items: all.slice(off, off + N),
    page: curPage.value,
    pageCount: Math.max(1, Math.ceil((last.pageCount > 0 ? Math.max(total, 0) : 0) / N)),
    total: last.total
  }
}

watch(vodPerPage, () => {
  curPage.value = 1
  syncQuery()
  load()
})

async function load() {
  if (!siteKey.value) return
  loading.value = true
  loadError.value = ''
  try {
    const ext = buildExt()
    page.value = await loadPage(ext)
  } catch (e: any) {
    loadError.value = humanizeApiError(e)
    app.showTip('列表加载失败：' + humanizeApiError(e))
  } finally {
    loading.value = false
  }
}

function buildExt(): string {
  const parts = Object.entries(filterVals.value).filter(([, v]) => v !== '')
  if (!parts.length) return ''
  return '&' + parts.map(([k, v]) => `${k}=${encodeURIComponent(v)}`).join('&')
}

function applyFilter(key: string, v: string) {
  filterVals.value[key] = v
  curPage.value = 1
  syncQuery()
  load()
}

function setFilterVal(key: string, v: string) {
  applyFilter(key, v)
}

function goto(p: number) {
  if (p < 1 || p > page.value.pageCount) return
  curPage.value = p
  syncQuery()
  load()
}

// 翻页步长：10 的幂，且小于总页数（>10 页出现 10 页，>100 页出现 100 页，>1000 页出现 1000 页…）
const jumpSteps = computed(() => {
  const out: number[] = []
  for (let s = 10; s < page.value.pageCount; s *= 10) out.push(s)
  return out
})
const jumpInput = ref('')
watch(() => page.value.page, (p) => { jumpInput.value = String(p) }, { immediate: true })
function jumpTo(p: number) {
  goto(Math.min(Math.max(p, 1), page.value.pageCount))
}
function commitJump() {
  const n = parseInt(jumpInput.value, 10)
  if (!Number.isFinite(n)) { jumpInput.value = String(page.value.page); return }
  const t = Math.min(Math.max(n, 1), page.value.pageCount)
  jumpInput.value = String(t)
  if (t !== page.value.page) goto(t)
}

// 解析站点 ext 里的 filters（与后端 cms.FiltersOf 同逻辑）
function parseFilters(ext: string, tid: string): FilterGroup[] {
  const out: FilterGroup[] = []
  if (!ext || ext.trim()[0] !== '{') return out
  try {
    const j = JSON.parse(ext)
    const map = j.filters
    if (!map) return out
    const groups = map[tid]
    if (!groups) return out
    for (const g of groups) {
      const gg: FilterGroup = { key: g.key, name: g.name || g.key, values: [] }
      for (const v of g.value || []) {
        if (v.n) gg.values.push({ n: v.n, v: v.v })
      }
      if (gg.key && gg.values.length) out.push(gg)
    }
  } catch {
    /* ignore */
  }
  return out
}
</script>

<template>
  <div class="page page--fixed">
    <div class="tab-head">
    <TopBar />

    <div class="vod-head">
      <div class="site-row">
        <span class="site-name">{{ currentSite?.name || '点播' }}</span>
        <span v-if="isSearching" class="total-hint">
          {{ searching ? '搜索中…' : `搜到 ${(searchResults || []).length} 部` }}
        </span>
        <span v-else-if="page.total > 0" class="total-hint">共 {{ page.total.toLocaleString() }} 部</span>
        <el-select v-model="siteKey" placeholder="换站" style="width: 200px" :class="currentSite ? siteState(currentSite) : ''">
          <el-option v-for="s in sortedSites" :key="s.key" :label="s.name" :value="s.key" :class="siteState(s)">
            <span class="opt-health">
              <span class="health-dot" :style="{ background: HEALTH_COLOR[healthOf(s.key)] }"></span>
              <span>{{ s.name }}</span>
              <span class="health-txt">{{ s.count }}</span>
            </span>
          </el-option>
        </el-select>
        <el-button @click="refreshAll">刷新</el-button>
        <el-input v-model="keyword" clearable placeholder="筛选片名" class="vod-filter" />
      </div>

      <!-- 父分类 -->
      <div class="cats">
        <span class="chip" :class="{ active: selectedParent === '' && !selectedChild }" @click="selectParent('')">全部</span>
        <span
          v-for="c in topCats"
          :key="c.id"
          class="chip"
          :class="{ active: selectedParent === c.id && !selectedChild }"
          @click="selectParent(c.id)"
        >
          {{ c.name }}
        </span>
      </div>

      <!-- 子分类（只在选中具体父分类时展开，「全部」='' 不触发子分类展开） -->
      <div v-if="selectedParent !== '' && childrenOf(selectedParent).length" class="cats sub">
        <!-- 子分类「全部」：显示父分类下全部内容，清空子分类选中 -->
        <span
          class="chip"
          :class="{ active: !selectedChild }"
          @click="selectedChild = ''; load()"
        >全部</span>
        <span
          v-for="c in childrenOf(selectedParent)"
          :key="c.id"
          class="chip"
          :class="{ active: selectedChild === c.id }"
          @click="selectChild(c.id)"
        >
          {{ c.name }}
        </span>
      </div>

      <!-- 筛选 -->
      <div v-for="g in filters" :key="g.key" class="cats filter">
        <span class="filter-name">{{ g.name }}</span>
        <span
          v-for="v in g.values"
          :key="v.v"
          class="chip"
          :class="{ active: filterVals[g.key] === v.v }"
          @click="setFilterVal(g.key, v.v)"
        >
          {{ v.n }}
        </span>
      </div>
    </div>
    </div>

    <div class="tab-body">
      <ErrorState v-if="isSearching ? searchErr : loadError" :message="isSearching ? searchErr : loadError" @retry="isSearching ? scheduleSiteSearch() : load()" />
      <VodGrid v-else :vods="displayItems" :loading="isSearching ? searching : loading" />
      <div v-if="!(isSearching ? searching : loading) && !(isSearching ? searchErr : loadError) && !displayItems.length" class="vod-empty">
        <p v-if="isSearching">在「{{ currentSite?.name }}」没搜到片名含“{{ keyword }}”的内容</p>
        <p v-else>暂无内容</p>
        <p class="hint">
          {{ isSearching ? '可以换个关键词，或切换站源再搜' : '可以切换分类或站源，或到「设置 → 订阅」重新导入' }}
        </p>
      </div>

      <div v-if="!isSearching && page.pageCount > 1" class="pager">
        <el-button v-for="st in [...jumpSteps].reverse()" :key="'b' + st" size="small"
          :disabled="page.page <= 1" @click="jumpTo(page.page - st)">上{{ st }}页</el-button>
        <el-button size="small" :disabled="page.page <= 1" @click="goto(page.page - 1)">上1页</el-button>
        <span class="pager-info">
          第
          <input v-model="jumpInput" class="pager-input" type="number" inputmode="numeric" min="1"
            :max="page.pageCount" aria-label="页码" @keydown.enter="commitJump" @blur="commitJump" />
          / {{ page.pageCount }} 页
        </span>
        <el-button size="small" :disabled="page.page >= page.pageCount" @click="goto(page.page + 1)">下1页</el-button>
        <el-button v-for="st in jumpSteps" :key="'f' + st" size="small"
          :disabled="page.page >= page.pageCount" @click="jumpTo(page.page + st)">下{{ st }}页</el-button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.vod-head {
  padding: 8px 12px 16px;
}
.site-row {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 16px;
}
.site-name {
  font-size: 20px;
  font-weight: 600;
}
.total-hint {
  font-size: 13px;
  color: var(--dim);
  margin-right: 4px;
}
.vod-empty {
  text-align: center;
  padding: 60px 24px;
  color: var(--dim);
}
.vod-empty .hint {
  font-size: 13px;
  margin-top: 6px;
}
.cats {
  display: flex;
  flex-wrap: wrap;
  align-content: flex-start;
  gap: 8px;
  margin-bottom: 10px;
  /* 最多露出 2 行胶囊，更多的纵向滚动 */
  max-height: 76px;
  overflow-y: auto;
  overscroll-behavior: contain;
}
.cats .chip {
  box-sizing: border-box;
  height: 34px;
  flex-shrink: 0;
}
.cats.sub {
  padding-left: 12px;
}
.cats.filter {
  align-items: center;
}
.filter-name {
  font-size: 13px;
  color: var(--dim);
  margin-right: 4px;
}
.pager {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: center;
  gap: 8px 12px;
  padding: 24px;
}
.pager-info {
  color: var(--sub);
  font-size: 14px;
}
.pager .el-button + .el-button { margin-left: 0; }
.pager-input {
  width: 48px;
  height: 24px;
  margin: 0 4px;
  text-align: center;
  color: inherit;
  background: transparent;
  border: 1px solid var(--dim, #888);
  border-radius: 4px;
  font-size: 14px;
  -moz-appearance: textfield;
}
.pager-input::-webkit-outer-spin-button,
.pager-input::-webkit-inner-spin-button { -webkit-appearance: none; margin: 0; }

/* ===== 移动端响应式 ===== */
@media (max-width: 640px) {
  .site-row { padding: 8px 16px; gap: 8px; flex-wrap: wrap; }
  .site-name { font-size: 18px; }
  .cats, .cats.sub {
    padding: 8px 16px;
    max-height: 84px;
  }
  .cats .chip, .cats.sub .chip { height: 30px; }
  .pager { padding: 16px; gap: 12px; }
  .vod-total { font-size: 13px; }
  /* 窄屏换行后不用 auto 外边距硬顶右边，否则换行后前面会空出一大截 */
  .vod-filter { margin-left: 0; width: 100%; }
}
.opt-health {
  display: flex;
  align-items: center;
  gap: 6px;
}
.health-dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  flex: none;
}
.health-txt {
  margin-left: auto;
  font-size: 11px;
  color: var(--dim);
}
.el-select-dropdown__item.st-off, .el-select.st-off :deep(.el-select__selected-item) { color: #a31515; }
.el-select-dropdown__item.st-on, .el-select.st-on :deep(.el-select__selected-item) { color: #19c037; }
.el-select-dropdown__item.st-empty, .el-select.st-empty :deep(.el-select__selected-item) { color: #909399; }
.el-select-dropdown__item.st-off .health-txt, .el-select-dropdown__item.st-on .health-txt, .el-select-dropdown__item.st-empty .health-txt { color: inherit; }

/* 筛选片名输入框去掉 size="small" 后用的是默认尺寸——字号跟随项目
   整体放大过的 --el-font-size-base，高度跟 --el-component-size(38px)
   走，是三者里最高的那个。换站下拉框和刷新按钮的高度在 Element Plus
   编译后的 CSS 里是写死的像素值（不吃 --el-component-size 这个变量），
   字号也不是按 size 走 var(--el-font-size-base)，所以即使同样去掉
   size="small" 也凑不齐——这里直接显式把它们的字号/高度焊死成跟筛选框
   一致，不依赖 size 属性本身的默认值。 */
.site-row :deep(.el-select .el-select__wrapper) {
  font-size: var(--el-font-size-base);
  min-height: 38px;
}
.site-row .el-button {
  font-size: var(--el-font-size-base);
  height: 38px;
}
/* 筛选框挪到最右：其余控件仍按原顺序从左排起，筛选框自己用 auto
   外边距顶到这一行的最右端，跟它们之间空出间隔而不是紧挨着。 */
.vod-filter {
  width: 180px;
  margin-left: auto;
}
</style>
