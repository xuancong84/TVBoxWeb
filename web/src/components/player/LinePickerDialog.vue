<script setup lang="ts">
// 直播选择线路弹窗：列出当前频道全部线路（高亮当前线路）+ 自动切线路开关。
// 跟 EpisodeDrawer 一样故意不用 append-to-body：桌面端原生全屏时只有
// .player/.m-player 这棵子树还会显示在屏幕上，传送到 document.body 的话
// 弹窗会被挡在全屏背后点了没反应。
const props = withDefaults(defineProps<{
  modelValue: boolean
  lines: string[]
  rawUrl: string
  lineQuality?: Record<string, { ok: boolean; ms: number; kbps: number }>
  autoSwitch: boolean
}>(), {
  lineQuality: () => ({})
})

const emit = defineEmits<{
  (e: 'update:modelValue', v: boolean): void
  (e: 'update:autoSwitch', v: boolean): void
  (e: 'switch-line', url: string): void
}>()

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

function pick(u: string) {
  // 选线路是「试一条不行再点下一条」的高频操作，选完就把弹窗关掉的话每次
  // 都要重新打开——留着弹窗，让用户能连着试好几条线路。
  emit('switch-line', u)
}
</script>

<template>
  <el-dialog
    :model-value="props.modelValue"
    title="选择线路"
    width="min(480px, 94vw)"
    destroy-on-close
    @click.stop
    @update:model-value="emit('update:modelValue', $event)"
  >
    <label class="line-auto-row">
      <el-checkbox
        :model-value="props.autoSwitch"
        @update:model-value="emit('update:autoSwitch', $event as boolean)"
      />
      <span>自动选线（当前线路播放失败时自动切换到其它线路）</span>
    </label>
    <div class="line-list">
      <button
        v-for="(u, i) in props.lines"
        :key="u + i"
        class="line-cell"
        :class="{ on: u === props.rawUrl, bad: lineBad(u) }"
        @click="pick(u)"
      >{{ lineLabel(u, i) }}</button>
    </div>
    <div v-if="!props.lines.length" class="line-none">该频道暂无其它线路</div>
  </el-dialog>
</template>

<style scoped>
.line-auto-row {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
  color: var(--sub);
  cursor: pointer;
  margin-bottom: 12px;
}
.line-list {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(110px, 1fr));
  gap: 8px;
  max-height: 52vh;
  overflow-y: auto;
}
.line-cell {
  border: 1px solid var(--border);
  background: var(--card);
  color: var(--sub);
  border-radius: var(--radius-sm);
  padding: 8px 6px;
  font-size: 12px;
  cursor: pointer;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.line-cell:hover { color: var(--txt); border-color: var(--sub); }
.line-cell.on { background: var(--selected); color: #000; border-color: var(--selected); font-weight: 600; }
.line-cell.bad { color: #e06060; border-color: #e06060; }
.line-none { text-align: center; color: var(--dim); font-size: 13px; padding: 24px; }
</style>
