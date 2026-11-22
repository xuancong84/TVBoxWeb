<script setup lang="ts">
import { ref, computed, watch, onMounted, onUnmounted, nextTick } from 'vue'
import { useRouter } from 'vue-router'
import type { Vod } from '@/types'
import { gridCols } from '@/gridPrefs'
import { cacheVod } from '@/vodCache'
import VodCard from './VodCard.vue'
import SkeletonList from './SkeletonList.vue'

const props = defineProps<{
  vods: Vod[]
  loading?: boolean
  skeletonCount?: number
  showProgress?: boolean
  progressOf?: (v: Vod) => number
  /** 低于该条数直接全量渲染，超过才启用虚拟滚动 */
  virtualThreshold?: number
  /** 可视区上下多渲染几行，滚动时更跟手 */
  overscan?: number
}>()

const router = useRouter()

/**
 * 真虚拟滚动：只渲染可视区上下 overscan 行的卡片，
 * 用一个定高容器 + translateY 撑出正确的滚动高度。
 *
 * 与之前的「60 条渐进渲染」相比，DOM 节点数恒定（约 2~3 屏），
 * 上万条收藏/搜索结果也不会因为节点累积而卡顿。
 * 列数、列宽、行高全部从实际布局测量（gridTemplateColumns），
 * 因此响应式断点和移动端布局都不需要写死魔法数字。
 */
const THRESHOLD = props.virtualThreshold ?? 240
const OVERSCAN = props.overscan ?? 2

const gridEl = ref<HTMLElement | null>(null)
const wrapEl = ref<HTMLElement | null>(null)

const cols = ref(1)
const cellW = ref(160)
const rowGap = ref(16)
const cardH = ref(0)
const startRow = ref(0)
// 首帧先渲染 6 行：足够量到真实卡片高度，又不至于挂载上百个节点
const endRow = ref(6)
const scrolled = ref(false)

const total = computed(() => props.vods.length)
const useVirtual = computed(() => total.value > THRESHOLD)
const totalRows = computed(() => Math.max(1, Math.ceil(total.value / Math.max(1, cols.value))))
const rowH = computed(() => cardH.value + rowGap.value)
const totalH = computed(() => Math.max(0, totalRows.value * rowH.value - rowGap.value))

/** 当前窗口要渲染的切片 */
const windowVods = computed(() => {
  if (!useVirtual.value) return props.vods
  let from = startRow.value * cols.value
  // 兜底：窗口算飞了（比如测量失败导致 startRow 超界）也不要渲染空白
  if (from >= total.value) from = Math.max(0, (totalRows.value - 1) * cols.value)
  const to = Math.min(total.value, endRow.value * cols.value)
  return props.vods.slice(from, Math.max(from, to))
})

/** 窗口相对容器顶部的偏移，用来把渲染出来的这几行摆回正确位置 */
const offsetY = computed(() => (useVirtual.value ? startRow.value * rowH.value : 0))

let ro: ResizeObserver | null = null
let raf = 0
let lastVods: Vod[] | null = null
let lastWidth = 0
// 这一屏现在不一定是 window 在滚——点播/首页/搜索这几个 Tab 改成了
// 自己的 .tab-body 内部滚动（见 App.vue 的 .page--fixed 布局），window
// 本身压根不会触发 scroll 事件。之前写死监听 window 的 scroll，滚的是
// .tab-body 的话这个虚拟滚动窗口永远不会跟着更新，滚过初始渲染的那几
// 行后面就是一片空白——这里改成往上找最近「自己会出滚动条」的祖先，
// 找不到（比如这个组件以后用在别的、仍然是整页滚动的页面里）才退回
// window，不写死任何一种。
let scrollParent: HTMLElement | Window = window

function findScrollParent(el: HTMLElement | null): HTMLElement | Window {
  let node = el?.parentElement || null
  while (node && node !== document.body && node !== document.documentElement) {
    const cs = getComputedStyle(node)
    if (/(auto|scroll)/.test(cs.overflowY)) return node
    node = node.parentElement
  }
  return window
}

function curScrollTop(): number {
  return scrollParent === window ? window.scrollY : (scrollParent as HTMLElement).scrollTop
}

function measureLayout() {
  const el = gridEl.value
  if (!el) return
  const cs = getComputedStyle(el)
  const tracks = (cs.gridTemplateColumns || '').split(' ').filter((s) => s.trim() !== '')
  cols.value = Math.max(1, tracks.length)
  cellW.value = tracks.length ? parseFloat(tracks[0]) || 160 : 160
  rowGap.value = parseFloat(cs.rowGap) || 16

  const card = el.querySelector('.vod-card') as HTMLElement | null
  const h = card?.offsetHeight || 0
  // 没有卡片可量时按海报 3:2 + 名称/年份预留高度估一个，避免行高为 0
  cardH.value = h > 0 ? h : Math.round(cellW.value * 1.5) + 62
}

function updateWindow() {
  if (!useVirtual.value) {
    startRow.value = 0
    endRow.value = totalRows.value
    return
  }
  const el = wrapEl.value || gridEl.value
  if (!el) return
  const rect = el.getBoundingClientRect()
  // 容器顶部在视口中的位置；为负说明已经滚过了栅格开头
  const top = rect.top
  const viewH = window.innerHeight || document.documentElement.clientHeight
  const startPx = Math.max(0, -top)
  const endPx = Math.max(0, viewH - top)
  const h = rowH.value || 1
  startRow.value = Math.max(0, Math.floor(startPx / h) - OVERSCAN)
  endRow.value = Math.min(totalRows.value, Math.ceil(endPx / h) + OVERSCAN)
  scrolled.value = startPx > 0
}

function onScroll() {
  if (raf) return
  raf = requestAnimationFrame(() => {
    raf = 0
    updateWindow()
  })
}

// 骨架屏阶段 wrapEl/gridEl 还没渲染出来（v-else-if="vods.length"），
// onMounted 那一刻往上找祖先只能落到 window 兜底；真正的网格 DOM 挂出来
// 后（vods 从空变有内容，触发下面那个 watch → relayout）这里会重新找一遍，
// 找到真正会出滚动条的那个祖先（.tab-body）就把监听换过去。
function rebindScrollParent() {
  const target = findScrollParent(wrapEl.value || gridEl.value)
  if (target === scrollParent) return
  scrollParent.removeEventListener('scroll', onScroll)
  scrollParent = target
  scrollParent.addEventListener('scroll', onScroll, { passive: true })
}

async function relayout() {
  rebindScrollParent()
  measureLayout()
  await nextTick()
  // 测量到真实卡片高度后，行高会变，需要重算窗口
  updateWindow()
}

function open(v: Vod) {
  // 这张卡片当下这份 Vod 数据存一下：如果已经带着 playUrl（点播/搜索
  // 这类列表接口，不少站源本来就把播放地址一起吐出来了），详情页就不用
  // 为了同一部片子再打一次网络请求。
  cacheVod(v)
  router.push({
    name: 'detail',
    query: { site: v.siteKey, siteName: v.siteName, id: v.id }
  })
}

watch(
  () => props.vods,
  async (v) => {
    if (v === lastVods) return
    lastVods = v
    // 列表整体换掉（切分类/换关键词）时回到顶部
    if (scrolled.value) {
      const el = wrapEl.value || gridEl.value
      const top = curScrollTop()
      const y = el ? el.getBoundingClientRect().top + top : 0
      if (el && y < top) scrollParent.scrollTo({ top: y })
    }
    scrolled.value = false
    startRow.value = 0
    await nextTick()
    await relayout()
  }
)

watch(cols, () => updateWindow())

// 用户在「设置 → 显示」里手动改列数：gridTemplateColumns 的 inline style
// 变了，但 gridEl 自身的宽度不变，ResizeObserver 不会被触发，这里单独
// 盯一下，手动重新测量布局。
watch(gridCols, () => relayout())

onMounted(async () => {
  // 骨架屏阶段大概率还没有真实网格 DOM，这里先按 window 兜底订阅一次，
  // 等 relayout() 里的 rebindScrollParent() 找到真正的滚动祖先再切换过去。
  scrollParent.addEventListener('scroll', onScroll, { passive: true })
  window.addEventListener('resize', onScroll, { passive: true })
  if (typeof ResizeObserver !== 'undefined' && gridEl.value) {
    // 只关心宽度变化：行数变化同样会触发 RO，不加宽度判定会形成
    // 「重排 → 节点数变化 → RO 再次触发」的循环。
    ro = new ResizeObserver((entries) => {
      const w = Math.round(entries[0]?.contentRect.width || 0)
      if (w === lastWidth) return
      lastWidth = w
      relayout()
    })
    ro.observe(gridEl.value)
  }
  await nextTick()
  await relayout()
})

onUnmounted(() => {
  scrollParent.removeEventListener('scroll', onScroll)
  window.removeEventListener('resize', onScroll)
  ro?.disconnect()
  if (raf) cancelAnimationFrame(raf)
})
</script>

<template>
  <!-- 加载中：复用全局骨架屏组件，不再内联一份 shimmer CSS -->
  <SkeletonList v-if="loading" type="card" :count="skeletonCount ?? 20" />

  <!-- 有内容 -->
  <div v-else-if="vods.length" ref="wrapEl" class="vod-grid-wrap" :style="{ height: useVirtual ? totalH + 'px' : undefined }">
    <div
      ref="gridEl"
      class="vod-grid"
      :style="{
        transform: useVirtual ? `translateY(${offsetY}px)` : undefined,
        gridTemplateColumns: `repeat(${gridCols}, 1fr)`
      }"
    >
      <VodCard
        v-for="v in windowVods"
        :key="v.siteKey + v.id"
        :vod="v"
        :show-progress="showProgress ? progressOf?.(v) : undefined"
        @click="open"
      />
    </div>
  </div>

  <div v-else class="empty">暂无内容，可以切换分类或站源</div>
</template>

<style scoped>
/* 定高容器负责真实滚动高度；内部栅格整体平移，只渲染窗口内的行 */
.vod-grid-wrap {
  position: relative;
  width: 100%;
}
.vod-grid {
  display: grid;
  /* 实际列数由 gridPrefs.ts 的 gridCols 通过 inline style 决定（见上面模板），
     这里的 auto-fill 只是兜底（inline style 还没挂上之前的一帧）。 */
  grid-template-columns: repeat(auto-fill, minmax(160px, 1fr));
  gap: 8px;
  padding: 0 12px;
}

/* 卡片高度必须一致，行高才能算准：
   片名固定占 2 行、年份固定占 1 行，缺内容时留白而不是塌陷 */
.vod-grid :deep(.name) {
  min-height: calc(14px * 1.3 * 2);
}
.vod-grid :deep(.meta) {
  min-height: 16px;
}

.empty {
  color: var(--dim);
  text-align: center;
  padding: 60px 0;
}

@media (max-width: 640px) {
  .vod-grid {
    grid-template-columns: repeat(auto-fill, minmax(110px, 1fr));
    gap: 5px;
    padding: 0 6px;
  }
  .vod-grid :deep(.name) {
    min-height: calc(13px * 1.3 * 2);
  }
}
</style>
