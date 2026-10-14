<script setup lang="ts">
import { ref, computed, watch } from 'vue';
import { downloadTaskResult, type ResultFormat } from '../utils/api';
import { phaseDate, type TimelinePair } from '../utils/timeline';

const props = withDefaults(defineProps<{ taskId?: string; pairs?: TimelinePair[] }>(), { taskId: '' });
const completedPairs = computed(() => props.pairs?.filter(pair => pair.status === 'succeeded' && pair.taskId) || []);
const selectedTaskId = ref('');
watch(() => completedPairs.value.map(pair => pair.taskId).join(','), () => {
  selectedTaskId.value = completedPairs.value[completedPairs.value.length - 1]?.taskId || '';
}, { immediate: true });
const exportTaskId = computed(() => props.pairs === undefined ? props.taskId : selectedTaskId.value);

type Format = ResultFormat;

const formats: { key: Format; label: string }[] = [
  { key: 'geojson', label: 'GeoJSON' },
  { key: 'shp', label: 'SHP（ZIP）' },
  { key: 'geotiff', label: 'GeoTIFF' },
  { key: 'csv', label: 'CSV' }
];

const busy = ref<Format | null>(null);
const error = ref<string>('');

function extOf(f: Format): string {
  return f === 'geotiff' ? 'tif' : f;
}

async function handleExport(f: Format): Promise<void> {
  if (!exportTaskId.value) {
    error.value = '任务 ID 为空，请先创建任务';
    return;
  }
  busy.value = f;
  error.value = '';
  const taskId = exportTaskId.value;
  try {
    const blob = await downloadTaskResult(taskId, f);
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = f === 'shp' ? `result_${taskId}_shp.zip` : `result_${taskId}.${extOf(f)}`;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
  } catch (e: unknown) {
    error.value = e instanceof Error ? e.message : '下载失败';
  } finally {
    busy.value = null;
  }
}
</script>

<template>
  <div class="panel p-3 space-y-2">
    <h3 class="panel-title">导出结果</h3>
    <label v-if="completedPairs.length" class="block text-xs text-muted">
      时相组
      <select v-model="selectedTaskId" class="input w-full mt-1">
        <option v-for="pair in completedPairs" :key="pair.key" :value="pair.taskId">
          {{ phaseDate(pair.before) }} → {{ phaseDate(pair.after) }}
        </option>
      </select>
    </label>
    <div class="grid grid-cols-2 gap-2">
      <button
        v-for="f in formats"
        :key="f.key"
        :disabled="busy !== null || !exportTaskId"
        class="btn-secondary px-2 py-1 text-xs hover:bg-cyan-500/25 hover:text-cyan-200"
        @click="handleExport(f.key)"
      >
        <span v-if="busy === f.key">导出中…</span>
        <span v-else>{{ f.label }}</span>
      </button>
    </div>
    <div v-if="error" class="text-xs text-rose-400">{{ error }}</div>
    <p class="text-xs text-faint">导出任务 ID: {{ exportTaskId || '暂无已完成结果' }}</p>
  </div>
</template>
