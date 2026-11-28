<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { api, humanizeApiError } from '@/api'
import { useAppStore } from '@/stores/app'
import type { DownloadJob } from '@/types'
import TopBar from '@/components/TopBar.vue'

const app = useAppStore()
const jobs = ref<DownloadJob[]>([])
const dir = ref('')
const loaded = ref(false)
let timer = 0

async function load() {
  try {
    const r = await api.downloads()
    jobs.value = r.jobs
    dir.value = r.dir
  } catch (e) {
    app.showTip(humanizeApiError(e))
  } finally {
    loaded.value = true
  }
}
onMounted(() => {
  load()
  timer = window.setInterval(load, 1500)
})
onUnmounted(() => clearInterval(timer))

const STATUS: Record<string, string> = {
  queued: '排队中',
  running: '下载中',
  done: '已完成',
  error: '失败',
  canceled: '已取消'
}

function mb(n: number) {
  return n >= 1 << 30 ? (n / (1 << 30)).toFixed(2) + ' GB' : (n / (1 << 20)).toFixed(1) + ' MB'
}
function pct(j: DownloadJob) {
  if (j.status === 'done') return 100
  if (j.total > 0) return Math.floor((j.done * 100) / j.total)
  if (j.size > 0) return Math.min(99, Math.floor((j.bytes * 100) / j.size))
  return 0
}
function detail(j: DownloadJob) {
  if (j.status === 'error') return j.err || ''
  if (j.status === 'done') return j.file + (j.bytes ? ' · ' + mb(j.bytes) : '')
  if (j.status === 'running' && j.kind === 'convert') return '转换中…'
  if (j.status === 'running') return (j.total > 0 ? `${j.done}/${j.total} 片 · ` : '') + mb(j.bytes)
  return ''
}
const hasEnded = computed(() => jobs.value.some((j) => ['done', 'error', 'canceled'].includes(j.status)))

async function cancel(j: DownloadJob) {
  await api.downloadCancel(j.id)
  load()
}
async function remove(j: DownloadJob) {
  await api.downloadRemove(j.id)
  load()
}
async function clearEnded() {
  await api.downloadRemove('', true)
  load()
}
</script>

<template>
  <div class="page">
    <TopBar />
    <div class="head">
      <h2>下载</h2>
      <span class="dir">保存位置：{{ dir }}（可在 设置 → 下载 修改）</span>
      <el-button v-if="hasEnded" size="small" @click="clearEnded">清除已结束</el-button>
    </div>
    <div v-if="loaded && !jobs.length" class="empty">暂无下载任务。在影片详情页点「下载」即可。</div>
    <div v-for="j in jobs" :key="j.id" class="job">
      <div class="row">
        <span class="name">{{ j.vod }} · {{ j.name }}{{ j.kind === 'convert' ? '（转为 mp4）' : '' }}</span>
        <span class="st" :class="j.status">{{ STATUS[j.status] }}</span>
        <el-button v-if="j.status === 'queued' || j.status === 'running'" size="small" @click="cancel(j)">取消</el-button>
        <el-button v-else size="small" @click="remove(j)">移除</el-button>
      </div>
      <div v-if="(j.status === 'running' && j.kind !== 'convert') || j.status === 'done'" class="bar"><i :style="{ width: pct(j) + '%' }"></i></div>
      <div v-if="detail(j)" class="sub" :class="{ err: j.status === 'error' }">{{ detail(j) }}</div>
    </div>
  </div>
</template>

<style scoped>
.head {
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 8px 24px;
  flex-wrap: wrap;
}
.head h2 {
  margin: 0;
}
.dir,
.sub {
  color: var(--sub);
  font-size: 14px;
  word-break: break-all;
}
.empty {
  padding: 40px 24px;
  color: var(--sub);
}
.job {
  margin: 8px 24px;
  padding: 12px 16px;
  border-radius: 10px;
  background: var(--card, rgba(128, 128, 128, 0.12));
}
.row {
  display: flex;
  align-items: center;
  gap: 12px;
}
.name {
  flex: 1;
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.st.done {
  color: #67c23a;
}
.st.error {
  color: #f56c6c;
}
.st.running {
  color: #409eff;
}
.bar {
  height: 6px;
  border-radius: 3px;
  background: rgba(128, 128, 128, 0.25);
  margin: 8px 0 4px;
  overflow: hidden;
}
.bar i {
  display: block;
  height: 100%;
  background: #409eff;
  transition: width 0.3s;
}
.sub.err {
  color: #f56c6c;
}
</style>
