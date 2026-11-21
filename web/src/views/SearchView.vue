<script lang="ts">
// 普通 <script> 块：只在模块加载时执行一次，真正的模块级作用域。
// 关键词 -> 搜索结果缓存必须放在这里，而不是 <script setup> 顶层——后者会被
// 编译进每个组件实例的 setup() 闭包，每次该组件重新挂载（比如没有 keep-alive
// 时「返回」会销毁重建组件）都会重新执行一遍，缓存就被悄悄清空重建了。
import type { Vod } from '@/types'
export interface CachedResult { results: Vod[]; siteInfo: string }
export const searchCache = new Map<string, CachedResult>()

// 把缓存落到 localStorage，整页刷新也不丢——只持久化「最近搜过」里仍然
// 存在的关键词（后端那边最多留 8 个），避免越攒越大。
const CACHE_KEY = 'yz_search_cache'

try {
  const raw = localStorage.getItem(CACHE_KEY)
  if (raw) {
    const obj = JSON.parse(raw) as Record<string, CachedResult>
    for (const [k, v] of Object.entries(obj)) searchCache.set(k, v)
  }
} catch { /* 隐私模式/数据损坏，忽略——退化成纯内存缓存 */ }

export function persistSearchCache(keywords: string[]) {
  try {
    const obj: Record<string, CachedResult> = {}
    for (const k of keywords) {
      const c = searchCache.get(k)
      if (c) obj[k] = c
    }
    localStorage.setItem(CACHE_KEY, JSON.stringify(obj))
  } catch { /* 配额超限/隐私模式，忽略 */ }
}
</script>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { api, humanizeApiError } from '@/api'
import { useAppStore } from '@/stores/app'
import TopBar from '@/components/TopBar.vue'
import VodGrid from '@/components/VodGrid.vue'
import SkeletonList from '@/components/SkeletonList.vue'

defineOptions({ name: 'SearchView' })

const router = useRouter()
const app = useAppStore()

const kw = ref('')
const results = ref<Vod[]>([])
const searching = ref(false)
const searched = ref(false)
const keywords = ref<string[]>([])
const hotKeywords = ref<string[]>([])
const siteInfo = ref('')
const sugg = ref<string[]>([])
const suggLoading = ref(false)
// 边搜边出的进度：done/total 是已经返回（成功或失败）的站源数 / 本次搜索
// 请求的站源总数，finished 之前结果随时可能继续增加。
const searchTotal    = ref(0)
const searchDone     = ref(0)
const searchFinished = ref(true)

onMounted(async () => {
  keywords.value = await api.keywords()
  // 清掉 localStorage 里关键词已经不在「最近搜过」里的陈旧条目
  persistSearchCache(keywords.value)
  // 热门词（feed 提取），失败静默
  api.hotKeywords().then(h => { hotKeywords.value = h }).catch(() => {})
})

// 判断关键词是否为纯 ASCII（可能是拼音缩写）
const isPinyin = computed(() => {
  const w = kw.value.trim()
  return w.length > 0 && /^[a-zA-Z0-9]+$/.test(w)
})

// 按站源分组结果
const grouped = computed(() => {
  const map = new Map<string, Vod[]>()
  for (const v of results.value) {
    const key = v.siteName || '其他'
    if (!map.has(key)) map.set(key, [])
    map.get(key)!.push(v)
  }
  return [...map.entries()]
})

// 搜索结果相关性排序（模糊匹配）
function rankResults(list: Vod[], word: string): Vod[] {
  const w = word.trim().toLowerCase()
  const score = (v: Vod): number => {
    const name = (v.name || '').toLowerCase()
    if (name === w) return 100          // 完全匹配
    if (name.startsWith(w)) return 80   // 前缀匹配
    if (name.includes(w)) return 60     // 包含匹配
    if (w.length >= 2 && name.replace(/[^\u4e00-\u9fa5a-z0-9]/g, '').includes(w.replace(/\s/g, ''))) return 40 // 去空格包含
    return 0                            // 不相关（保持原顺序）
  }
  // 稳定排序：按分数降序，分数相同保持原顺序
  return list
    .map((v, i) => ({ v, s: score(v), i }))
    .sort((a, b) => b.s - a.s || a.i - b.i)
    .map(x => x.v)
}

// force=true（点「搜索」按钮）时跳过缓存，强制重新搜索并刷新缓存；
// 否则（回车、点关键词芯片）命中缓存就直接展示，不打网络请求。
async function doSearch(k?: string, force = false) {
  const word = (k ?? kw.value).trim()
  if (!word) {
    app.showTip('请输入关键词')
    return
  }
  kw.value = word
  sugg.value = []
  searched.value = true

  if (!force) {
    const cached = searchCache.get(word)
    if (cached) {
      // 取消上一次还没搜完的搜索，缓存命中不该被它迟到的结果覆盖
      searchGen++
      results.value = cached.results
      siteInfo.value = cached.siteInfo
      searching.value = false
      searchFinished.value = true
      return
    }
  }

  await runLiveSearch(word)
}

// 边搜边出：/search/start 立即拿到 session id，不等任何站源返回；
// 然后轮询 /search/poll，哪个站源先响应就先展示哪个的结果，直到
// finished=true。myGen 用来在「这次搜索还没结束、用户又开始了新搜索」时
// 让旧的轮询自己安静退出，不会用旧搜索迟到的结果覆盖新搜索。
let searchGen = 0
async function runLiveSearch(word: string) {
  const myGen = ++searchGen
  searching.value = true
  searchFinished.value = false
  searchTotal.value = 0
  searchDone.value = 0
  results.value = []
  siteInfo.value = ''

  let id: string
  let total: number
  try {
    const r = await api.searchStart(word)
    id = r.id
    total = r.total
  } catch (e: any) {
    if (myGen !== searchGen) return
    searching.value = false
    searchFinished.value = true
    app.showTip('搜索失败：' + humanizeApiError(e))
    return
  }
  if (myGen !== searchGen) return
  searchTotal.value = total
  // 关键词记录不用等搜完——哪怕中途离开，这次搜过的词也该留下来
  keywords.value = await api.pushKeyword(word).catch(() => keywords.value)

  const poll = async () => {
    if (myGen !== searchGen) return
    try {
      const r = await api.searchPoll(id)
      if (myGen !== searchGen) return
      results.value = rankResults(r.list, word)
      searchDone.value = r.done
      searchTotal.value = r.total
      searchFinished.value = r.finished
      if (r.finished) {
        searching.value = false
        siteInfo.value = `${r.sites}/${r.total} 个站源有响应`
        searchCache.set(word, { results: results.value, siteInfo: siteInfo.value })
        persistSearchCache(keywords.value)
        return
      }
    } catch (e: any) {
      if (myGen !== searchGen) return
      searching.value = false
      searchFinished.value = true
      app.showTip('搜索失败：' + humanizeApiError(e))
      return
    }
    pollTimer = window.setTimeout(poll, 450)
  }
  void poll()
}

function clearKw() {
  searchGen++ // 停掉还在轮询的搜索，别让它迟到的结果把清空后的空页面重新填上
  if (pollTimer) { clearTimeout(pollTimer); pollTimer = 0 }
  kw.value = ''
  results.value = []
  searched.value = false
  sugg.value = []
  searching.value = false
  searchFinished.value = true
}

function clearKeywords() {
  api.clearKeywords().then(() => {
    keywords.value = []
    searchCache.clear()
    persistSearchCache([])
    app.showTip('已清空搜索记录')
  })
}

async function removeKeyword(k: string) {
  keywords.value = await api.removeKeyword(k)
  searchCache.delete(k)
  persistSearchCache(keywords.value)
}

// 实时联想 / 搜索轮询控制
let suggTimer = 0
let suggAbort: AbortController | null = null
let pollTimer = 0

function onInput() {
  const w = kw.value.trim()
  if (!w) {
    sugg.value = []
    return
  }
  if (suggTimer) clearTimeout(suggTimer)
  // abort 之前还未完成的联想请求，防止旧请求结果覆盖新输入的结果
  if (suggAbort) suggAbort.abort()
  suggTimer = window.setTimeout(async () => {
    const ctrl = new AbortController()
    suggAbort = ctrl
    try {
      sugg.value = await api.suggest(w, ctrl.signal)
    } catch (e: any) {
      if (e?.name !== 'AbortError') sugg.value = []
    } finally {
      if (suggAbort === ctrl) suggAbort = null
    }
  }, 350) // 350ms 防抖，减少打字过程中的无效请求
}
</script>

<template>
  <div class="page page--fixed">
    <div class="tab-head">
    <TopBar />
    <div class="search-box">
      <el-input
        v-model="kw"
        size="large"
        placeholder="输入片名，回车搜索（支持拼音首字母，如 sgyy → 三国演义）"
        clearable
        @keyup.enter="doSearch()"
        @input="onInput"
        @clear="clearKw"
      >
        <template #append>
          <el-button :loading="searching" @click="doSearch(undefined, true)">搜索</el-button>
        </template>
      </el-input>
      <!-- 拼音输入提示 -->
      <div v-if="isPinyin && !searched" class="pinyin-hint">
        检测到字母输入，将按「拼音首字母」匹配，如 sgyy → 三国演义
      </div>
    </div>
    </div>

    <div class="tab-body">
    <!-- 猜你想搜 -->
    <div v-if="sugg.length && !searched" class="sugg-box">
      <div class="kw-head">猜你想搜</div>
      <div class="sugg-chips">
        <span v-for="s in sugg" :key="s" class="chip" @click="doSearch(s)">{{ s }}</span>
      </div>
    </div>

    <div v-if="!searched && !sugg.length && keywords.length" class="kw-box">
      <div class="kw-head">
        <span>最近搜过</span>
        <span class="clear" @click="clearKeywords">清空记录</span>
      </div>
      <div class="kw-chips">
        <span v-for="k in keywords" :key="k" class="chip">
          <span class="chip-text" @click="doSearch(k)">{{ k }}</span>
          <span class="chip-del" title="删除" @click="removeKeyword(k)">✕</span>
        </span>
      </div>
    </div>

    <!-- 热门搜索 -->
    <div v-if="!searched && !sugg.length && hotKeywords.length" class="kw-box">
      <div class="kw-head"><span>热门搜索</span></div>
      <div class="kw-chips">
        <span v-for="k in hotKeywords" :key="k" class="chip hot" @click="doSearch(k)">{{ k }}</span>
      </div>
    </div>

    <div v-if="!searched && !sugg.length && !keywords.length && !hotKeywords.length" class="empty-guide">
      <div class="guide-title">搜索影片</div>
      <p class="guide-sub">会在已启用的协议型站源里并发检索（最多 18 个）</p>
      <p class="guide-sub">支持拼音首字母，如输入 sgyy 搜「三国演义」</p>
    </div>

    <div v-if="searched">
      <div class="section-title">
        搜索结果
        <!-- 还没搜完：边搜边出的进度，站源陆续返回时这个数字会一直涨 -->
        <span v-if="!searchFinished" class="count searching-count">
          <span class="search-spinner"></span>
          正在搜索…{{ searchDone }}/{{ searchTotal }} 个站源已返回
        </span>
        <span v-else-if="siteInfo" class="count">{{ siteInfo }}</span>
        <span v-if="results.length" class="count">共 {{ results.length }} 条</span>
      </div>
      <!-- 还没出第一批结果：骨架屏；一旦有结果就直接展示，不用等全部站源返回 -->
      <SkeletonList v-if="searching && !results.length" type="search" :count="10" />
      <!-- 搜索结果：边搜边出，站源陆续返回时这里会跟着一组组地增加 -->
      <div v-else-if="results.length">
        <div v-for="[site, vods] in grouped" :key="site" class="group">
          <div class="group-head">{{ site }} <span class="count">{{ vods.length }} 个结果</span></div>
          <VodGrid :vods="vods" />
        </div>
      </div>
      <div v-else-if="searchFinished" class="empty">没有搜到结果（站源可能已失效，或需要爬虫运行时）</div>
    </div>
    </div>
  </div>
</template>

<style scoped>
.search-box {
  padding: 16px 24px;
  max-width: 640px;
}
.pinyin-hint {
  margin-top: 6px;
  font-size: 12px;
  color: var(--primary, #409eff);
}
.sugg-box,
.kw-box {
  padding: 0 24px 16px;
}
.kw-head {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 10px;
  font-size: 14px;
  color: var(--sub);
}
.clear {
  color: var(--dim);
  cursor: pointer;
}
.sugg-chips,
.kw-chips {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
.chip {
  gap: 6px;
}
.chip.hot {
  color: #ffb74d;
  border: 1px solid rgba(255, 183, 77, 0.3);
}
.chip-text {
  cursor: pointer;
}
.chip-del {
  font-size: 10px;
  color: var(--dim);
  cursor: pointer;
  padding: 2px;
  border-radius: 50%;
}
.chip-del:hover {
  color: #ff4d4f;
  background: rgba(255, 77, 79, 0.15);
}
.group {
  margin-bottom: 20px;
}
.group-head {
  font-size: 15px;
  font-weight: 600;
  color: var(--sub);
  padding: 0 24px;
  margin-bottom: 10px;
}
.group-head .count {
  font-size: 12px;
  color: var(--dim);
  font-weight: 400;
}
/* 边搜边出的进度指示 */
.searching-count {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  color: var(--primary, #409eff);
}
.search-spinner {
  width: 11px;
  height: 11px;
  border-radius: 50%;
  border: 2px solid currentColor;
  border-top-color: transparent;
  animation: search-spin 0.7s linear infinite;
  flex-shrink: 0;
}
@keyframes search-spin {
  to { transform: rotate(360deg); }
}
.empty {
  text-align: center;
  padding: 60px 24px;
  color: var(--dim);
}
.empty-guide {
  text-align: center;
  padding: 60px 24px;
}
.guide-title {
  font-size: 22px;
  font-weight: 600;
  margin-bottom: 10px;
}
.guide-sub {
  font-size: 13px;
  color: var(--dim);
  margin: 4px 0;
}

/* ===== 移动端响应式 ===== */
@media (max-width: 640px) {
  .top { padding: 8px 16px; gap: 8px; }
  .hints { padding: 0 16px; }
  .hint-item { font-size: 13px; padding: 8px 10px; }
  .groups { padding: 0 16px; }
  .group-head { font-size: 16px; padding: 12px 0 6px; }
  .result-list { padding: 0 16px; }
  .result-row { padding: 8px 0; gap: 10px; }
  .result-thumb { width: 80px; flex-shrink: 0; }
  .result-info .name { font-size: 14px; }
  .result-info .meta { font-size: 12px; }
  .empty-guide { padding: 40px 16px; }
  .guide-title { font-size: 18px; }
}
</style>
