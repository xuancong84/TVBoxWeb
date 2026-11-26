<script setup lang="ts">
// 播放设置面板：画质 / 直播线路 / 外挂字幕 / 画面比例 / 跳过片头片尾。
// 桌面端是居中浮层，移动端是底部弹出式 sheet，两套结构共用同一份选项数据，
// 从 PlayerView.vue 抽出以消除模板里几乎完全重复的两段。
import type { QLevel } from '@/utils/quality'
import { AUTO, TOP, labelOfLevel } from '@/utils/quality'
import { computed } from 'vue'
import type { SubtitleTrack } from '@/composables/usePlayerSubtitles'

const props = withDefaults(defineProps<{
  modelValue: boolean
  variant: 'desktop' | 'mobile'
  isLive: boolean
  levels: QLevel[]
  qCap: number
  levelIdx: number
  /** 直播线路地址列表 */
  lines: string[]
  rawUrl: string
  /** 各条线路的轻量测速结果（url -> {ok, ms, kbps}），没测过的线路取不到值 */
  lineQuality?: Record<string, { ok: boolean; ms: number; kbps: number }>
  /** 点播的全部线路（播放源）；多于 1 条时可在面板里切换 */
  vodLines?: { name: string }[]
  vodLineIdx?: number
  /** 自动选线开关：关掉后，当前线路失败时原地无限重试，不切线路/不退出 */
  autoSwitch: boolean
  subDelay: number
  subFontSize: number
  externalSubActive: boolean
  subLabel: string
  /** HLS 内嵌字幕轨道（多语言），只有 m3u8 源才可能有。
   *  必须给默认值：模板里读的是 subTracks.length，一旦调用方漏传
   *  就会在渲染期抛 TypeError 把整个面板打崩。 */
  subTracks?: SubtitleTrack[]
  /** 当前激活的内嵌轨道下标，-1 表示关闭 */
  subTrackIdx?: number
  scaleNames: string[]
  scaleMode: number
  introSkipSec: number
  outroSkipSec: number
  /** 当前这部剧是否有自己的片头片尾设置（false 表示在用全局默认） */
  skipOwn?: boolean
  /** 全局默认值，用于说明「当前用的是默认」 */
  globalIntroSkip?: number
  globalOutroSkip?: number
}>(), {
  subTracks: () => [],
  vodLines: () => [],
  vodLineIdx: 0,
  lineQuality: () => ({}),
  subTrackIdx: -1,
  skipOwn: false,
  globalIntroSkip: 0,
  globalOutroSkip: 0
})

const emit = defineEmits<{
  (e: 'update:modelValue', v: boolean): void
  (e: 'choose-level', cap: number): void
  (e: 'switch-line', url: string): void
  (e: 'switch-vod-line', idx: number): void
  (e: 'update:autoSwitch', v: boolean): void
  (e: 'sub-file', ev: Event): void
  (e: 'sub-url'): void
  (e: 'remove-sub'): void
  (e: 'choose-sub-track', idx: number): void
  (e: 'sub-delay', ms: number): void
  (e: 'sub-size', px: number): void
  (e: 'scale', i: number): void
  (e: 'intro-skip', sec: number): void
  (e: 'outro-skip', sec: number): void
  (e: 'mark-skip', kind: 'intro' | 'outro'): void
  (e: 'skip-apply-all'): void
  (e: 'skip-reset'): void
}>()

const SKIP_PRESETS = [0, 15, 30, 60, 90]
const SUB_DELAYS = [-500, 0, 500]
const SUB_SIZES = [16, 20, 26, 32]

// 模板里所有内嵌轨道相关的地方都走这里，保证永远不会是 undefined。
// withDefaults 已兜住「不传」，这里再兜住「显式传 null」。
const subTrackList = computed<SubtitleTrack[]>(() => props.subTracks || [])

// 线路测速徽标：没测过时只显示「线路 N」，测过且连通时附上延迟，连不上标红字。
function lineLabel(u: string, i: number): string {
  const q = props.lineQuality?.[u]
  if (!q) return `线路 ${i + 1}`
  if (!q.ok) return `线路 ${i + 1} · 超时`
  return `线路 ${i + 1} · ${q.ms}ms`
}
function lineBad(u: string): boolean {
  const q = props.lineQuality?.[u]
  return !!q && !q.ok
}

function close() {
  emit('update:modelValue', false)
}

function levelCap(lv: QLevel) {
  return lv.height > 0 ? lv.height : TOP
}

function delayLabel(d: number) {
  return d === 0 ? '同步' : (d > 0 ? '+' : '') + (d / 1000).toFixed(1) + 's'
}
</script>

<template>
  <!-- 桌面端：居中浮层。
       可见性必须由 modelValue 决定 —— 之前这里只判断 variant，
       父组件把 modelValue 置 false 后组件自身不接受，面板永远关不掉。 -->
  <div v-if="props.modelValue && props.variant === 'desktop'" class="q-panel" @click.stop @click.self="close">
    <div class="q-content">
      <div class="q-title">播放设置</div>
      <div class="q-section">画质</div>
      <div class="q-chips">
        <span class="chip" :class="{ active: props.qCap === AUTO }" @click="emit('choose-level', AUTO)">自动</span>
        <span class="chip" :class="{ active: props.qCap >= TOP }" @click="emit('choose-level', TOP)">原画</span>
        <span
          v-for="(lv, i) in props.levels"
          :key="i"
          class="chip"
          :class="{ active: props.qCap > 0 && props.qCap < TOP && props.levelIdx === i }"
          @click="emit('choose-level', levelCap(lv))"
        >{{ labelOfLevel(lv) }}</span>
      </div>
      <div v-if="!props.levels.length" class="q-hint">源只提供这一档清晰度。</div>

      <template v-if="!props.isLive && props.vodLines.length > 1">
        <div class="q-section">线路（{{ props.vodLines.length }} 条）</div>
        <div class="q-chips">
          <span
            v-for="(l, i) in props.vodLines"
            :key="'vl' + i"
            class="chip"
            :class="{ active: i === props.vodLineIdx }"
            @click="emit('switch-vod-line', i)"
          >{{ l.name }}</span>
        </div>
      </template>

      <template v-if="props.isLive">
        <div class="q-section">线路</div>
        <label class="line-auto-row">
          <el-checkbox
            :model-value="props.autoSwitch"
            @update:model-value="emit('update:autoSwitch', $event as boolean)"
          />
          <span>自动选线（当前线路播放失败时自动切换到其它线路；关闭后原地无限重试这条线路）</span>
        </label>
        <div v-if="props.lines.length > 1" class="q-chips">
          <span
            v-for="(u, i) in props.lines"
            :key="'line' + i"
            class="chip"
            :class="{ active: u === props.rawUrl, 'chip-bad': lineBad(u) }"
            @click="emit('switch-line', u)"
          >{{ lineLabel(u, i) }}</span>
        </div>
      </template>

      <!-- 内嵌字幕轨道：只有 m3u8 源可能带多语言字幕。
           这段功能（usePlayerSubtitles 的 subtitles/switchSubtitle）之前
           完全没有 UI，代码里留了句 `void switchSubtitle // 保留给将来 UI 使用`，
           等于内嵌字幕永远用不上。 -->
      <!-- 跳过片头片尾按「这部剧」保存：不同剧的片头长度差很多，
           全局一套值套所有剧必然错位，所以明确标注作用范围，
           并给出「应用到所有剧」「恢复默认」两个出口。
           排位放在画质之后：这是最常改的一项，原来排在面板最底部，
           手机上（尤其 360x640）整块落在折叠线以下，用户以为没这功能。 -->
      <div class="q-section">跳过片头 / 片尾 · 本剧</div>
      <div class="skip-row">
        <span class="skip-label">片头</span>
        <span
          v-for="s in SKIP_PRESETS"
          :key="'i' + s"
          class="chip chip-sm"
          :class="{ active: props.introSkipSec === s }"
          @click="emit('intro-skip', s)"
        >{{ s === 0 ? '关' : s + '秒' }}</span>
        <span class="chip chip-sm" @click="emit('mark-skip', 'intro')">标当前</span>
      </div>
      <div class="skip-row">
        <span class="skip-label">片尾</span>
        <span
          v-for="s in SKIP_PRESETS"
          :key="'o' + s"
          class="chip chip-sm"
          :class="{ active: props.outroSkipSec === s }"
          @click="emit('outro-skip', s)"
        >{{ s === 0 ? '关' : s + '秒' }}</span>
        <span class="chip chip-sm" @click="emit('mark-skip', 'outro')">标当前</span>
      </div>
      <div class="q-hint q-hint-skip">
        <template v-if="props.skipOwn">本剧已有单独设置，其他剧不受影响。</template>
        <template v-else>
          正在使用默认值（片头 {{ props.globalIntroSkip }}s / 片尾 {{ props.globalOutroSkip }}s），
          改动后只对本剧生效。
        </template>
        <span class="skip-actions">
          <span class="chip chip-xs" @click="emit('skip-apply-all')">应用到所有剧</span>
          <span v-if="props.skipOwn" class="chip chip-xs" @click="emit('skip-reset')">恢复默认</span>
        </span>
      </div>

      <template v-if="subTrackList.length">
        <div class="q-section">内嵌字幕</div>
        <div class="q-chips">
          <span
            class="chip"
            :class="{ active: props.subTrackIdx === -1 }"
            @click="emit('choose-sub-track', -1)"
          >关闭</span>
          <span
            v-for="(t, i) in subTrackList"
            :key="'st' + i"
            class="chip"
            :class="{ active: props.subTrackIdx === i }"
            @click="emit('choose-sub-track', i)"
          >{{ t.label || t.lang || '字幕 ' + (i + 1) }}</span>
        </div>
      </template>

      <div class="q-section">外挂字幕</div>
      <div class="q-chips">
        <label class="chip" style="cursor: pointer">本地字幕
          <input type="file" accept=".srt,.vtt,.ass,.ssa" style="display: none" @change="emit('sub-file', $event)" />
        </label>
        <span class="chip" @click="emit('sub-url')">网络字幕</span>
        <span v-if="props.externalSubActive" class="chip" @click="emit('remove-sub')">移除字幕</span>
        <span
          v-for="d in SUB_DELAYS"
          :key="'sd' + d"
          class="chip"
          :class="{ active: props.subDelay === d }"
          @click="emit('sub-delay', d)"
        >{{ delayLabel(d) }}</span>
        <span
          v-for="s in SUB_SIZES"
          :key="'sf' + s"
          class="chip"
          :class="{ active: props.subFontSize === s }"
          @click="emit('sub-size', s)"
        >{{ s }}px</span>
      </div>
      <div class="q-hint">{{ props.subLabel || '支持 SRT / VTT / ASS，文件仅在本机解析' }}</div>

      <div class="q-section">画面比例</div>
      <div class="q-chips">
        <span
          v-for="(n, i) in props.scaleNames"
          :key="'sc' + i"
          class="chip"
          :class="{ active: props.scaleMode === i }"
          @click="emit('scale', i)"
        >{{ n }}</span>
      </div>

      <div class="q-footer"><el-button size="small" @click="close">关闭</el-button></div>
    </div>
  </div>

  <!-- 移动端：底部弹出 sheet（同样受 modelValue 控制） -->
  <div v-else-if="props.modelValue" class="m-q-mask" @click.stop @click.self="close">
    <div class="m-q-sheet">
      <div class="m-q-handle"></div>
      <div class="m-q-title">播放设置</div>
      <div class="m-q-section">画质</div>
      <div class="m-q-chips">
        <span class="chip" :class="{ active: props.qCap === AUTO }" @click="emit('choose-level', AUTO)">自动</span>
        <span class="chip" :class="{ active: props.qCap >= TOP }" @click="emit('choose-level', TOP)">原画</span>
        <span
          v-for="(lv, i) in props.levels"
          :key="i"
          class="chip"
          :class="{ active: props.qCap > 0 && props.qCap < TOP && props.levelIdx === i }"
          @click="emit('choose-level', levelCap(lv))"
        >{{ labelOfLevel(lv) }}</span>
      </div>

      <template v-if="!props.isLive && props.vodLines.length > 1">
        <div class="m-q-section">线路（{{ props.vodLines.length }} 条）</div>
        <div class="m-q-chips">
          <span
            v-for="(l, i) in props.vodLines"
            :key="'vl' + i"
            class="chip"
            :class="{ active: i === props.vodLineIdx }"
            @click="emit('switch-vod-line', i)"
          >{{ l.name }}</span>
        </div>
      </template>

      <template v-if="props.isLive">
        <div class="m-q-section">线路</div>
        <label class="line-auto-row">
          <el-checkbox
            :model-value="props.autoSwitch"
            @update:model-value="emit('update:autoSwitch', $event as boolean)"
          />
          <span>自动选线（当前线路播放失败时自动切换到其它线路；关闭后原地无限重试这条线路）</span>
        </label>
        <div v-if="props.lines.length > 1" class="m-q-chips">
          <span
            v-for="(u, i) in props.lines"
            :key="'line' + i"
            class="chip"
            :class="{ active: u === props.rawUrl, 'chip-bad': lineBad(u) }"
            @click="emit('switch-line', u)"
          >{{ lineLabel(u, i) }}</span>
        </div>
      </template>

      <div class="m-q-section">跳过片头 / 片尾 · 本剧</div>
      <div class="skip-row">
        <span class="skip-label">片头</span>
        <span
          v-for="s in SKIP_PRESETS"
          :key="'mi' + s"
          class="chip chip-sm"
          :class="{ active: props.introSkipSec === s }"
          @click="emit('intro-skip', s)"
        >{{ s === 0 ? '关' : s + '秒' }}</span>
        <span class="chip chip-sm" @click="emit('mark-skip', 'intro')">标当前</span>
      </div>
      <div class="skip-row">
        <span class="skip-label">片尾</span>
        <span
          v-for="s in SKIP_PRESETS"
          :key="'mo' + s"
          class="chip chip-sm"
          :class="{ active: props.outroSkipSec === s }"
          @click="emit('outro-skip', s)"
        >{{ s === 0 ? '关' : s + '秒' }}</span>
        <span class="chip chip-sm" @click="emit('mark-skip', 'outro')">标当前</span>
      </div>
      <div class="q-hint q-hint-skip">
        <template v-if="props.skipOwn">本剧已有单独设置，其他剧不受影响。</template>
        <template v-else>
          正在使用默认值（片头 {{ props.globalIntroSkip }}s / 片尾 {{ props.globalOutroSkip }}s），
          改动后只对本剧生效。
        </template>
        <span class="skip-actions">
          <span class="chip chip-xs" @click="emit('skip-apply-all')">应用到所有剧</span>
          <span v-if="props.skipOwn" class="chip chip-xs" @click="emit('skip-reset')">恢复默认</span>
        </span>
      </div>

      <template v-if="subTrackList.length">
        <div class="m-q-section">内嵌字幕</div>
        <div class="m-q-chips">
          <span
            class="chip"
            :class="{ active: props.subTrackIdx === -1 }"
            @click="emit('choose-sub-track', -1)"
          >关闭</span>
          <span
            v-for="(t, i) in subTrackList"
            :key="'mst' + i"
            class="chip"
            :class="{ active: props.subTrackIdx === i }"
            @click="emit('choose-sub-track', i)"
          >{{ t.label || t.lang || '字幕 ' + (i + 1) }}</span>
        </div>
      </template>

      <div class="m-q-section">外挂字幕</div>
      <div class="m-q-chips">
        <label class="chip" style="cursor: pointer">本地字幕
          <input type="file" accept=".srt,.vtt,.ass,.ssa" style="display: none" @change="emit('sub-file', $event)" />
        </label>
        <span class="chip" @click="emit('sub-url')">网络字幕</span>
        <span v-if="props.externalSubActive" class="chip" @click="emit('remove-sub')">移除字幕</span>
        <span
          v-for="d in SUB_DELAYS"
          :key="'msd' + d"
          class="chip"
          :class="{ active: props.subDelay === d }"
          @click="emit('sub-delay', d)"
        >{{ delayLabel(d) }}</span>
        <span
          v-for="s in SUB_SIZES"
          :key="'msf' + s"
          class="chip"
          :class="{ active: props.subFontSize === s }"
          @click="emit('sub-size', s)"
        >{{ s }}px</span>
      </div>
      <div class="q-hint">{{ props.subLabel || '支持 SRT / VTT / ASS，文件仅在本机解析' }}</div>

      <div class="m-q-section">画面比例</div>
      <div class="m-q-chips">
        <span
          v-for="(n, i) in props.scaleNames"
          :key="'sc' + i"
          class="chip"
          :class="{ active: props.scaleMode === i }"
          @click="emit('scale', i)"
        >{{ n }}</span>
      </div>

      <div class="m-q-close">
        <el-button size="small" style="width: 100%" @click="close">关闭</el-button>
      </div>
    </div>
  </div>
</template>

<style scoped>
/* ===== 桌面端浮层 ===== */
.q-panel {
  position: absolute;
  inset: 0;
  background: rgba(0, 0, 0, 0.5);
  display: flex;
  align-items: center;
  justify-content: center;
}
.q-content {
  background: var(--surface);
  border-radius: 16px;
  padding: 24px;
  max-width: 640px;
  width: 80%;
  max-height: 80vh;
  overflow-y: auto;
}
.q-title { font-size: 16px; font-weight: 600; margin-bottom: 6px; }
.q-section { font-size: 13px; color: var(--sub); margin: 16px 0 8px; }
.q-chips { display: flex; flex-wrap: wrap; gap: 10px; }
.q-hint { margin-top: 8px; color: var(--dim); font-size: 13px; }
/* 跳过区的说明文字 + 两个操作按钮要能同排也能折行，
   否则窄屏上按钮会被挤到下一行白白多占 30px 高度。
   不用 :has() —— 部分旧 WebView 不支持。 */
.q-hint-skip {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 4px 0;
}
.q-footer { margin-top: 20px; text-align: right; }

.chip {
  padding: 8px 16px;
  border-radius: 20px;
  background: var(--card);
  border: 1px solid transparent;
  font-size: 14px;
  cursor: pointer;
  transition: all 0.15s;
}
.chip:hover { background: var(--card-hi); }
.chip.active {
  background: var(--live);
  color: #fff;
  font-weight: 600;
}
.chip-bad {
  color: var(--warn, #ffb4ab);
  border-color: var(--warn, #ffb4ab);
}
.line-auto-row {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
  color: var(--sub);
  cursor: pointer;
  margin-bottom: 10px;
}

/* ===== 跳过片头/片尾：两行紧凑排布 ===== */
/* 原来是两个独立 section，各占一行标题+一行 chips，
   手机上 7 段叠起来高 850px+，跳过区整块落在折叠线以下。
   合并成一段两行后省下约 60px，配合上移就能在 360x640 直接看到。 */
.skip-row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
  margin-top: 6px;
}
.skip-label {
  flex: 0 0 auto;
  min-width: 30px;
  font-size: 12px;
  color: var(--dim);
}
/* 同一条选项行里 chip 要更小，否则 6 个选项放不下会被挤到第二行 */
.chip-sm {
  padding: 4px 10px;
  font-size: 13px;
  border-radius: 14px;
}
.chip-xs {
  padding: 3px 9px;
  font-size: 12px;
  border-radius: 12px;
}
/* 「应用到所有剧 / 恢复默认」跟在说明文字后面，不单独占一行 */
.skip-actions {
  display: inline-flex;
  gap: 6px;
  margin-left: 8px;
  vertical-align: middle;
}

/* ===== 移动端 sheet ===== */
.m-q-mask {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.5);
  display: flex;
  align-items: flex-end;
  z-index: 100;
}
.m-q-sheet {
  width: 100%;
  background: var(--surface);
  border-radius: 16px 16px 0 0;
  padding: 20px 16px;
  max-height: 80vh;
  overflow-y: auto;
}
.m-q-handle {
  width: 36px;
  height: 4px;
  border-radius: 2px;
  background: var(--card-hi);
  margin: 0 auto 16px;
}
.m-q-title {
  font-size: 16px;
  font-weight: 600;
  margin-bottom: 16px;
  text-align: center;
}
.m-q-section {
  font-size: 13px;
  color: var(--sub);
  margin: 16px 0 8px;
}
.m-q-chips {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
.m-q-close {
  margin-top: 16px;
}
</style>
