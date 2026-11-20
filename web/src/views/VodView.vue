<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, humanizeApiError } from '@/api'
import { useAppStore } from '@/stores/app'
import type { Site, Category, VodPage, FilterGroup } from '@/types'
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
const topCats = computed(() => categories.value.filter((c) => c.pid === '' && c.id !== ''))
const childrenOf = (tid: string) => categories.value.filter((c) => c.pid === tid)

// 初始化期间 siteKey 的首次赋值不算「用户切换站点」，不能清空刚恢复的筛选
let restoring = true

// 记住上次选过的站，下次切到点播页默认直接用它；从没选过（或那个站没了）就退化成
// 内容最多的站（sortedSites 已按数量降序、severity 升序排好，取第一个即可）。
const LAST_SITE_KEY = 'yz_vod_last_site'

onMounted(async () => {
  // 从 URL 恢复筛选/页码，返回列表时不会丢失状态
  curPage.value = Math.max(1, Number(route.query.page) || 1)
  if (route.query.f) {
    try { filterVals.value = JSON.parse(route.query.f as string) } catch { /* ignore */ }
  }
  void loadHealth()
  const list = await api.sites()
  sites.value = list.filter((s) => s.usable)
  if (!siteKey.value) {
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
      // 带着具体分类进来（比如从详情页返回）：分类/子分类归属要靠分类列表
      // 才能分清楚，这一种必须等分类回来才能发起正确的片单请求。
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
})

async function loadCategories() {
  catLoading.value = true
  try {
    categories.value = await api.categories(siteKey.value)
  } catch (e: any) {
    app.showTip('分类加载失败：' + humanizeApiError(e))
  } finally {
    catLoading.value = false
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

async function load() {
  if (!siteKey.value) return
  loading.value = true
  loadError.value = ''
  try {
    const ext = buildExt()
    page.value = await api.vodList(siteKey.value, currentTid(), curPage.value, ext, currentChildren())
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
  <div class="page">
    <TopBar />

    <div class="vod-head">
      <div class="site-row">
        <span class="site-name">{{ currentSite?.name || '点播' }}</span>
        <span v-if="page.total > 0" class="total-hint">共 {{ page.total.toLocaleString() }} 部</span>
        <el-select v-model="siteKey" size="small" placeholder="换站" style="width: 200px">
          <el-option v-for="s in sortedSites" :key="s.key" :label="s.name" :value="s.key" :class="{ 'opt-dead': s.count === 0 }">
            <span class="opt-health">
              <span class="health-dot" :style="{ background: HEALTH_COLOR[healthOf(s.key)] }"></span>
              <span>{{ s.name }}</span>
              <span class="health-txt">{{ s.count }}</span>
            </span>
          </el-option>
        </el-select>
        <el-button size="small" @click="refreshAll">刷新</el-button>
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

    <div>
      <ErrorState v-if="loadError" :message="loadError" @retry="load" />
      <VodGrid v-else :vods="page.items" :loading="loading" />
      <div v-if="!loading && !loadError && !page.items.length" class="vod-empty">
        <p>暂无内容</p>
        <p class="hint">可以切换分类或站源，或到「设置 → 订阅」重新导入</p>
      </div>

      <div v-if="page.pageCount > 1" class="pager">
        <el-button size="small" :disabled="page.page <= 1" @click="goto(page.page - 1)">上一页</el-button>
        <span class="pager-info">{{ page.page }} / {{ page.pageCount }}</span>
        <el-button size="small" :disabled="page.page >= page.pageCount" @click="goto(page.page + 1)">下一页</el-button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.vod-head {
  padding: 8px 24px 16px;
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
  gap: 8px;
  margin-bottom: 10px;
}
.cats.sub {
  padding-left: 24px;
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
  align-items: center;
  justify-content: center;
  gap: 16px;
  padding: 24px;
}
.pager-info {
  color: var(--sub);
  font-size: 14px;
}

/* ===== 移动端响应式 ===== */
@media (max-width: 640px) {
  .site-row { padding: 8px 16px; gap: 8px; flex-wrap: wrap; }
  .site-name { font-size: 18px; }
  .cats, .cats.sub {
    padding: 8px 16px;
    overflow-x: auto;
    flex-wrap: nowrap;
    padding-bottom: 8px;
  }
  .cats .chip, .cats.sub .chip { flex-shrink: 0; }
  .pager { padding: 16px; gap: 12px; }
  .vod-total { font-size: 13px; }
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
.opt-dead { opacity: 0.6; }
</style>
