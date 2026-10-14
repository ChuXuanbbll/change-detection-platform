<script setup lang="ts">
import { ref, computed, watch, onMounted, onUnmounted, onActivated, nextTick } from 'vue';
import * as echarts from 'echarts/core';
import { CustomChart } from 'echarts/charts';
import { GridComponent, TooltipComponent, MarkLineComponent } from 'echarts/components';
import { CanvasRenderer } from 'echarts/renderers';
import type { DetectionStats } from '../utils/api';
const props = defineProps<{ stats?: DetectionStats }>();
echarts.use([CustomChart, GridComponent, TooltipComponent, MarkLineComponent, CanvasRenderer]);
// Show changed-pixel confidence statistics by default.
const population = ref<'changed' | 'all_valid'>('changed');
const histogram = computed(() => props.stats?.confidence_histograms);
const counts = computed(() => histogram.value?.[`${population.value}_counts`] ?? []);
const count = computed(() => histogram.value?.[`${population.value}_sample_count`] ?? 0);
const mean = computed(() => population.value === 'changed' ? props.stats?.mean_confidence : props.stats?.mean_confidence_all);
const empty = computed(() => !histogram.value || !count.value);

// Model scores are usually extremely bimodal: almost every unchanged pixel sits
// in the first bin, so a linear axis hides everything else. Switch to a log axis
// automatically when one bin dwarfs the rest; the user can override it.
const skewed = computed(() => {
  const sorted = counts.value.filter(n => n > 0).sort((a, b) => b - a);
  return sorted.length > 1 && sorted[0] > 20 * sorted[1];
});
const logOverride = ref<boolean | null>(null);
const logScale = computed(() => logOverride.value ?? skewed.value);
watch([population, histogram], () => { logOverride.value = null; });

const compact = (v: number) => v >= 1e6 ? `${+(v / 1e6).toFixed(1)}M` : v >= 1e3 ? `${+(v / 1e3).toFixed(1)}K` : `${v}`;
// Deep teal → cyan → amber → rose: low scores read as cold, high scores as hot.
const STOPS: [number, [number, number, number]][] = [[0, [21, 94, 130]], [0.45, [79, 216, 239]], [0.78, [240, 192, 106]], [1, [255, 122, 138]]];
function rampColor(t: number) {
  const i = Math.max(1, STOPS.findIndex(([p]) => p >= t));
  const [p0, c0] = STOPS[i - 1], [p1, c1] = STOPS[i];
  const k = Math.min(1, Math.max(0, (t - p0) / (p1 - p0 || 1)));
  return c0.map((c, j) => Math.round(c + (c1[j] - c) * k));
}

const container = ref<HTMLDivElement | null>(null);
let chart: echarts.EChartsType | undefined;
let observer: ResizeObserver | undefined;
function render() {
  if (!container.value?.isConnected) return;
  chart ??= echarts.init(container.value);
  if (empty.value) { chart.clear(); return; }
  const h = histogram.value!;
  const muted = '#8ca4c0';
  const log = logScale.value;
  const total = count.value;
  const average = mean.value != null && Number.isFinite(mean.value) ? mean.value : undefined;
  const data = counts.value.map((n, i) => {
    const [r, g, b] = rampColor((h.bin_edges[i] + h.bin_edges[i + 1]) / 2);
    return {
      value: [h.bin_edges[i], h.bin_edges[i + 1], n],
      itemStyle: { color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
        { offset: 0, color: `rgba(${r},${g},${b},1)` }, { offset: 1, color: `rgba(${r},${g},${b},0.55)` }]) },
    };
  }).filter(d => !log || d.value[2] > 0);
  chart.setOption({
    animation: false, backgroundColor: 'transparent',
    grid: { left: 46, right: 14, top: 30, bottom: 44 },
    tooltip: {
      trigger: 'item',
      formatter: (p: { value: number[] }) => {
        const [a, b, n] = p.value;
        return `得分 ${a.toFixed(2)}–${b.toFixed(2)}<br/>${n.toLocaleString()} 像元（${(n / total * 100).toFixed(n / total < 0.001 ? 3 : 1)}%）`;
      },
    },
    xAxis: {
      type: 'value', min: 0, max: 1, interval: 0.2, name: '模型变化得分', nameLocation: 'middle', nameGap: 26,
      nameTextStyle: { color: muted }, axisLabel: { color: muted, formatter: (v: number) => v.toFixed(1) },
      axisLine: { show: true, lineStyle: { color: muted } }, splitLine: { show: true, lineStyle: { color: '#1b2e44' } },
    },
    yAxis: {
      type: log ? 'log' : 'value', logBase: 10, min: log ? 1 : 0, minInterval: 1,
      axisLabel: { color: muted, formatter: compact },
      splitLine: { lineStyle: { color: '#21364e' } },
    },
    series: [{
      type: 'custom', data, encode: { x: [0, 1], y: 2 },
      renderItem: (_params: unknown, api: { value: (i: number) => number; coord: (v: number[]) => number[]; style: () => object }) => {
        const top = api.coord([api.value(0), api.value(2)]);
        const bottom = api.coord([api.value(1), log ? 1 : 0]);
        const gap = 1;
        return { type: 'rect', shape: { x: top[0] + gap, y: top[1], width: Math.max(1, bottom[0] - top[0] - 2 * gap), height: bottom[1] - top[1], r: [2, 2, 0, 0] }, style: api.style() };
      },
      markLine: average == null ? { data: [] } : {
        symbol: 'none', silent: true,
        lineStyle: { color: '#dbe7f5', type: 'dashed', width: 1.2 },
        label: { formatter: `均值 ${average > 0 && average < 0.001 ? '<0.001' : average.toFixed(3)}`, color: '#9fe3ff', position: 'end', fontWeight: 600 },
        data: [{ xAxis: average }],
      },
    }],
  }, true);
}
watch([histogram, population, logScale], () => { void nextTick(render); });
function resize() { if (container.value?.isConnected) chart?.resize(); }
onMounted(() => { render(); observer = new ResizeObserver(resize); if (container.value) observer.observe(container.value); });
onActivated(() => { void nextTick(() => { render(); resize(); }); });
onUnmounted(() => {
  observer?.disconnect(); observer = undefined;
  chart?.dispose(); chart = undefined;
});
</script>
<template>
  <section class="panel confidence-panel">
    <div class="section-heading"><h2>变化置信度分布</h2><select v-model="population" class="input" aria-label="置信度统计范围"><option value="changed">变化像元</option><option value="all_valid">全部有效像元</option></select></div>
    <div class="chart-meta">
      <p class="chart-sample">{{ histogram ? `${count.toLocaleString()} 个有效得分样本` : '置信度分布不可用' }}</p>
      <button v-if="!empty" type="button" class="scale-toggle" :aria-pressed="logScale" @click="logOverride = !logScale">对数刻度</button>
    </div>
    <div class="chart-frame"><div ref="container" class="confidence-chart" role="img" :aria-label="`${population === 'changed' ? '变化' : '全部有效'}像元置信度直方图，${count} 个样本`" /><p v-if="empty" class="chart-empty">{{ !histogram ? '当前结果未提供置信度分布，请重新运行检测' : '没有可统计的像元' }}</p></div>
  </section>
</template>
