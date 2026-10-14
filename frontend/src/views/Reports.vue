<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import { useWorkspace } from '../composables/useWorkspace';
import { phaseDate } from '../utils/timeline';
import { rankedPatches, formatArea, formatScore, meanChangeScore, patchId, statusLabel } from '../utils/results';
import PairSelector from '../components/PairSelector.vue';
import ResultMetrics from '../components/ResultMetrics.vue';
import ExportPanel from '../components/ExportPanel.vue';
const { pairs, selectedPairKey, selectedPair, batchMessage, taskError } = useWorkspace();
const patches = computed(() => rankedPatches(selectedPair.value));
const page = ref(1), pageSize = 25;
const pageCount = computed(() => Math.max(1, Math.ceil(patches.value.length / pageSize)));
const visible = computed(() => patches.value.slice((page.value - 1) * pageSize, page.value * pageSize));
watch(selectedPairKey, () => { page.value = 1; });
</script>
<template>
  <div class="page-content reports-page">
    <div class="page-heading"><div><span class="eyebrow">检测成果</span><h2>本批次报告</h2><p>每组变化独立记录，结果可追溯、可导出。</p></div><PairSelector /></div>
    <div v-if="!pairs.length" class="panel empty-workspace"><span class="empty-symbol">▤</span><h2>暂无检测报告</h2><p>提交检测任务后，本批次结果将在此汇总。</p><RouterLink to="/overview" class="btn-primary">前往概览 →</RouterLink></div>
    <template v-else>
      <div class="batch-summary"><span>{{ batchMessage }}</span><span>当前会话 · {{ pairs.length }} 组时相</span></div><p v-if="taskError" class="error-text">{{ taskError }}</p>
      <section class="panel report-table-panel"><div class="section-heading"><h2>时相检测记录</h2><span class="subtle">面积与比例按组统计</span></div>
        <div class="table-scroll"><table class="results-table"><thead><tr><th>时相组</th><th>状态</th><th>变化面积</th><th>图斑数</th><th>变化占比</th><th>平均变化置信度</th><th>操作</th></tr></thead><tbody>
          <tr v-for="(pair, index) in pairs" :key="pair.key" :class="{ 'selected-row': pair.key === selectedPairKey }"><td><b>{{ String(index + 1).padStart(2, '0') }}</b> {{ phaseDate(pair.before) }} → {{ phaseDate(pair.after) }}<small class="task-id">{{ pair.taskId || '尚未提交' }}</small></td><td><span class="status-pill" :data-status="pair.status">{{ statusLabel(pair.status) }}</span></td><td>{{ formatArea(pair.stats?.changed_area_m2) }}</td><td>{{ pair.stats?.feature_count ?? '—' }}</td><td>{{ pair.stats?.change_ratio == null ? '—' : `${(pair.stats.change_ratio * 100).toFixed(2)}%` }}</td><td>{{ meanChangeScore(pair.stats) }}</td><td><button class="text-link" @click="selectedPairKey = pair.key">查看明细</button><RouterLink to="/comparison" class="text-link" @click="selectedPairKey = pair.key">影像对比 ↗</RouterLink></td></tr>
        </tbody></table></div>
      </section>
      <template v-if="selectedPair">
        <div class="report-detail-heading"><h2>{{ phaseDate(selectedPair.before) }} → {{ phaseDate(selectedPair.after) }}</h2><span class="subtle">选中时相组</span></div>
        <div class="report-summary-grid"><ResultMetrics :stats="selectedPair.stats" /><ExportPanel :task-id="selectedPair.status === 'succeeded' ? selectedPair.taskId : ''" /></div>
        <p v-if="selectedPair.warning" class="warning-text">{{ selectedPair.warning }}</p>
        <section class="panel report-table-panel"><div class="section-heading"><h2>变化区域明细 <span class="subtle">{{ patches.length }}</span></h2><span class="subtle">按面积降序</span></div>
          <div v-if="patches.length" class="table-scroll"><table class="results-table"><thead><tr><th>排名</th><th>图斑编号</th><th>变化面积</th><th>变化像元</th><th>平均置信度</th><th>中心位置（经度，纬度）</th></tr></thead><tbody><tr v-for="(patch, i) in visible" :key="patchId(patch) || i"><td>{{ (page - 1) * pageSize + i + 1 }}</td><td>{{ patchId(patch) || '—' }}</td><td>{{ formatArea(patch.properties.area_m2) }}</td><td>{{ patch.properties.area_pixels?.toLocaleString() ?? '—' }}</td><td>{{ formatScore(patch.properties.confidence_mean) }}</td><td>{{ patch.properties.centroid?.map(n => n.toFixed(6)).join(', ') || '—' }}</td></tr></tbody></table></div>
          <p v-else class="section-empty">{{ selectedPair.status !== 'succeeded' ? '该时相组尚无完成结果' : selectedPair.geojson ? '未检测到变化图斑' : '变化图斑数据不可用' }}</p>
          <div v-if="pageCount > 1" class="pagination"><button class="btn-secondary" :disabled="page === 1" @click="page--">上一页</button><span>{{ page }} / {{ pageCount }}</span><button class="btn-secondary" :disabled="page === pageCount" @click="page++">下一页</button></div>
        </section>
      </template>
    </template>
  </div>
</template>
