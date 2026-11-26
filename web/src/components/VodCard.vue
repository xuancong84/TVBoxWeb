<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import type { Vod, SubRef } from '@/types'
import { siteInfoMap } from '@/siteInfo'
import { imgUrl } from '@/api'
import { splitLines } from '@/utils/lines'

const props = defineProps<{
  vod: Vod
  showProgress?: number // 0~1 观看进度，压在海报底部
  showRemark?: boolean
}>()

const emit = defineEmits<{ (e: 'click', vod: Vod): void }>()

const router = useRouter()
// 真实 <a href>：中键/ctrl/cmd+点击让浏览器原生在新标签页打开，不用自己写
// window.open 逻辑——跟详情页路由一致（VodGrid.open 同样的 query）。
const href = computed(() =>
  router.resolve({
    name: 'detail',
    query: { site: props.vod.siteKey, siteName: props.vod.siteName, id: props.vod.id }
  }).href
)

function onClick(e: MouseEvent) {
  // 中键点击不会触发 click（走 auxclick），这里只需要拦住普通左键：
  // 保持 SPA 内部跳转（emit 给父组件走 router.push），不要让浏览器真的
  // 按 href 做整页跳转。ctrl/cmd/shift+左键和中键都交给浏览器原生处理
  // （新标签页打开），什么都不做。
  if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
  e.preventDefault()
  emit('click', props.vod)
}

const failed = ref(false)

function onErr() {
  failed.value = true
}

const firstChar = computed(() => (props.vod.name ? props.vod.name.trim()[0] || '影' : '影'))

// 总集数：不少站源的片单接口其实也带着播放地址（不是只有详情页才有），
// 几条线路里取集数最多的那条当作「总集数」——线路之间偶尔有抓取失败、
// 集数对不齐的情况，取最全的那条更接近真实总集数。单集（电影/数据缺失）
// 不显示，不然一堆电影卡片上全是「(共1集)」没意义。
const epCount = computed(() => {
  if (props.vod.playUrl) {
    const lines = splitLines(props.vod.from, props.vod.playUrl)
    return lines.reduce((max, l) => Math.max(max, l.eps.length), 0)
  }
  // 首页「最近更新」这类卡片没有 playUrl（落盘前被砍掉了），退回用
  // 后端存的总集数快照。
  return props.vod.epCount || 0
})
const metaText = computed(() => {
  const parts: string[] = []
  if (props.vod.year) parts.push(props.vod.year)
  if (epCount.value > 1) parts.push(`(共${epCount.value}集)`)
  return parts.join(' ')
})

// 鼠标悬停卡片时顺带标出「点播源 / 单仓源」——方便挑出要拉黑的垃圾/色情
// 源头，不用点进详情页再去查。subs 现查现填，拿不到也不影响卡片本身。
const subs = ref<SubRef[]>([])
onMounted(async () => {
  const map = await siteInfoMap()
  subs.value = map.get(props.vod.siteKey)?.subs || []
})
const subsTitle = computed(() => {
  if (!subs.value.length) return '单仓源：未知'
  return '单仓源：\n' + subs.value.map((s) => s.name || s.source).join('\n')
})
</script>

<template>
  <a
    class="vod-card"
    :href="href"
    :aria-label="`打开 ${vod.name}`"
    @click="onClick"
    @keydown.space.prevent="emit('click', vod)"
  >
    <div class="poster">
      <img v-if="!failed && vod.pic" :src="imgUrl(vod.pic)" :alt="vod.name" loading="lazy" @error="onErr" />
      <div v-else class="placeholder">
        <span class="ph-char">{{ firstChar }}</span>
        <span class="ph-name">{{ vod.name }}</span>
      </div>
      <div v-if="vod.remark && showRemark !== false" class="remark">{{ vod.remark }}</div>
      <div v-if="showProgress !== undefined && showProgress > 0" class="progress">
        <div class="progress-bar" :style="{ width: showProgress * 100 + '%' }"></div>
      </div>
      <!-- 悬停才出现：点播源徽标，再悬停它本身用原生 tooltip 展开单仓源列表 -->
      <div v-if="vod.siteName" class="src-badge" :title="subsTitle" @click.stop>
        {{ vod.siteName }}
      </div>
    </div>
    <div class="name">{{ vod.name }}</div>
    <div v-if="metaText" class="meta">{{ metaText }}</div>
  </a>
</template>

<style scoped>
.vod-card {
  display: block;
  color: inherit;
  text-decoration: none;
  cursor: pointer;
  transition: transform 0.14s ease-out;
}
.vod-card:hover {
  transform: scale(1.06);
}
.poster {
  position: relative;
  /* 不用 aspect-ratio：旧电视浏览器（旧 webOS/Chromium<88）不支持，
     容器高度会塌成 0、图片看不见。padding-bottom 150% = 2:3，处处可用 */
  height: 0;
  padding-bottom: 150%;
  border-radius: var(--radius);
  overflow: hidden;
  background: var(--card);
  border: 2px solid transparent;
  transition: border-color 0.14s;
}
.vod-card:hover .poster {
  border-color: #fff;
}
.poster img {
  /* 绝对定位铺满：不依赖「aspect-ratio 盒子里 height:100% 能解析」，
     部分浏览器（旧 Safari/WebView）解析不了，图片会按原始尺寸撑开卡片 */
  position: absolute;
  left: 0;
  top: 0;
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
  border-radius: var(--radius);
}
.placeholder {
  position: absolute;
  left: 0;
  top: 0;
  box-sizing: border-box;
  width: 100%;
  height: 100%;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 6px;
  background: linear-gradient(160deg, #26242b 0%, #1c1b1f 100%);
  padding: 8px;
  text-align: center;
}
.ph-char {
  font-size: 40px;
  font-weight: 700;
  color: rgba(255, 255, 255, 0.18);
  line-height: 1;
}
.ph-name {
  font-size: 12px;
  color: rgba(255, 255, 255, 0.5);
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
  word-break: break-all;
}
.remark {
  position: absolute;
  right: 0;
  top: 0;
  background: rgba(23, 117, 53, 0.7);
  color: #fff;
  font-size: 12px;
  padding: 4px;
  border-radius: 0 0 0 4px;
}
.progress {
  position: absolute;
  left: 0;
  right: 0;
  bottom: 0;
  height: 4px;
  background: rgba(255, 255, 255, 0.2);
}
.progress-bar {
  height: 100%;
  background: var(--live);
}
/* 点播源徽标：平时隐藏，卡片悬停才出现；再悬停它本身触发原生 title
   tooltip 展开单仓源列表，不用额外写一套弹层组件 */
.src-badge {
  position: absolute;
  left: 0;
  right: 0;
  bottom: 0;
  padding: 3px 7px;
  font-size: 11px;
  line-height: 1.4;
  color: #fff;
  background: rgba(0, 0, 0, 0.62);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  opacity: 0;
  pointer-events: none;
  transition: opacity 0.14s;
}
.vod-card:hover .src-badge {
  opacity: 1;
  pointer-events: auto;
  cursor: help;
}
.name {
  margin-top: 8px;
  font-size: 14px;
  line-height: 1.3;
  color: var(--txt);
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
.meta {
  margin-top: 2px;
  font-size: 12px;
  color: var(--dim);
}

/* ===== 移动端：触摸替代 hover ===== */
@media (max-width: 640px) {
  .vod-card:active .poster {
    border-color: #fff;
    transform: scale(0.97);
  }
  /* 名称在手机上不限2行 */
  .name { -webkit-line-clamp: 2; font-size: 13px; }
  /* remark 标签在小屏不截断 */
  .remark { font-size: 10px; padding: 1px 5px; }
}
</style>
