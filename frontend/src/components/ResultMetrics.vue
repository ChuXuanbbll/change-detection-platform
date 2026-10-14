<script setup lang="ts">
import type { DetectionStats } from '../utils/api';
import { formatArea, meanChangeScore } from '../utils/results';
defineProps<{ stats?: DetectionStats }>();
</script>
<template>
  <div class="metrics-grid">
    <article class="metric-card"><span class="metric-icon">▱</span><h3>变化面积</h3><strong>{{ formatArea(stats?.changed_area_m2) }}</strong></article>
    <article class="metric-card"><span class="metric-icon">◈</span><h3>平均变化置信度</h3><strong>{{ meanChangeScore(stats) }}</strong></article>
    <article class="metric-card"><span class="metric-icon">▦</span><h3>变化图斑数</h3><strong>{{ stats?.feature_count?.toLocaleString() ?? '—' }}<small v-if="stats?.feature_count != null"> 个</small></strong></article>
    <article class="metric-card"><span class="metric-icon">◔</span><h3>变化占比</h3><strong>{{ stats?.change_ratio == null ? '—' : `${(stats.change_ratio * 100).toFixed(2)}%` }}</strong></article>
  </div>
</template>
