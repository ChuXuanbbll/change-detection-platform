<script setup lang="ts">
import { computed, ref, shallowRef, watch, onUnmounted } from 'vue';
import { useWorkspace } from '../composables/useWorkspace';
import { phaseDate, pairBounds } from '../utils/timeline';
import { rankedPatches, patchId, patchBounds, statusLabel } from '../utils/results';
import { imagePreview, maskPreview, type RasterPreview, type Bounds } from '../utils/previews';
import type { ChangePatch } from '../utils/api';
import ComparisonViewer, { type ViewPosition } from '../components/ComparisonViewer.vue';
import PairSelector from '../components/PairSelector.vue';
import ResultMetrics from '../components/ResultMetrics.vue';
import ConfidenceHistogram from '../components/ConfidenceHistogram.vue';
import HotspotCard from '../components/HotspotCard.vue';
const { pairs, selectedPair, batchMessage } = useWorkspace();
const before = shallowRef<RasterPreview>(), after = shallowRef<RasterPreview>(), mask = shallowRef<RasterPreview>();
const errors = ref(['', '', '']), loading = ref(false);
const view = ref<ViewPosition>(), focus = ref<Bounds>();
const selected = shallowRef<ChangePatch>();
const ranked = computed(() => rankedPatches(selectedPair.value));
const page = ref(1), pageSize = 10;
const pageCount = computed(() => Math.max(1, Math.ceil(ranked.value.length / pageSize)));
const jumpPage = ref('');
const pageItems = computed(() => {
  const count = pageCount.value;
  if (count <= 7) return Array.from({ length: count }, (_, i) => i + 1);
  const start = Math.max(1, Math.min(page.value - 2, count - 4));
  const end = Math.min(count, start + 4);
  const items: (number | string)[] = [];
  if (start > 1) items.push(1);
  if (start > 2) items.push('leading-gap');
  for (let i = start; i <= end; i++) items.push(i);
  if (end < count - 1) items.push('trailing-gap');
  if (end < count) items.push(count);
  return items;
});
function goToPage() {
  const value = jumpPage.value.trim();
  if (!/^\d+$/.test(value)) return;
  const target = Number(value);
  if (!Number.isSafeInteger(target)) return;
  page.value = Math.max(1, Math.min(pageCount.value, target));
  jumpPage.value = '';
}
const visible = computed(() => ranked.value.slice((page.value - 1) * pageSize, page.value * pageSize));
watch(() => selectedPair.value?.key, () => { page.value = 1; jumpPage.value = ''; });
watch(pageCount, count => { page.value = Math.min(page.value, count); });
const bounds = computed(() => selectedPair.value ? pairBounds(selectedPair.value.before, selectedPair.value.after) : undefined);
let generation = 0;
async function load() {
  const token = ++generation, pair = selectedPair.value;
  before.value = after.value = mask.value = undefined;
  errors.value = ['', '', '']; loading.value = false;
  view.value = undefined; focus.value = undefined; selected.value = undefined;
  if (!pair) return;
  loading.value = true;
  const assets = await Promise.allSettled([imagePreview(pair.before.file_path), imagePreview(pair.after.file_path),
    pair.maskPath ? maskPreview(pair.maskPath, pair.color) : Promise.resolve(undefined)]);
  if (token !== generation) return;
  const previews = assets.map((asset, i) => {
    if (asset.status === 'fulfilled') return asset.value;
    errors.value[i] = asset.reason instanceof Error ? asset.reason.message : '预览加载失败';
    return undefined;
  });
  before.value = previews[0]; after.value = previews[1]; mask.value = previews[2];
  // Render endpoints return source-CRS rasters; do not silently stretch projections.
  if (before.value && after.value && before.value.crs !== after.value.crs) {
    after.value = undefined; errors.value[1] = '前后影像预览坐标系不同，无法直接联动显示';
  }
  if (before.value && mask.value && before.value.crs !== mask.value.crs) {
    mask.value = undefined; errors.value[2] = '结果预览坐标系与前时相不同';
  }
  loading.value = false;
}
watch(() => [selectedPair.value?.key, selectedPair.value?.maskPath], load, { immediate: true });
onUnmounted(() => { generation++; });
function selectPatch(patch: ChangePatch) { selected.value = patch; view.value = undefined; focus.value = patchBounds(patch); }
function resetView() { selected.value = undefined; view.value = undefined; focus.value = bounds.value ? [...bounds.value] : undefined; }
</script>
<template>
  <div class="page-content comparison-page">
    <div v-if="!selectedPair" class="panel empty-workspace"><span class="empty-symbol">◫</span><h2>{{ pairs.length ? '等待选择时相组' : '从一个区域开始' }}</h2><p>{{ pairs.length ? batchMessage || '选择上方时相组，或等待首组检测完成。' : '在概览页框选区域并提交检测，随后在这里查看影像与变化。' }}</p><RouterLink to="/overview" class="btn-primary">前往概览 →</RouterLink></div>
    <template v-else>
      <div class="comparison-toolbar"><PairSelector /><span class="status-pill" :data-status="selectedPair.status">{{ statusLabel(selectedPair.status) }}</span><span class="change-legend"><i :style="{ background: selectedPair.color }" />检测变化</span><span>联动视图</span><button class="btn-secondary" @click="resetView">重置视图</button><button v-if="errors.some(Boolean)" class="btn-secondary" @click="load">重试预览</button><RouterLink to="/reports" class="text-link">查看报告 ↗</RouterLink></div>
      <p v-if="selectedPair.warning" class="warning-text">{{ selectedPair.warning }}</p>
      <div class="comparison-layout">
        <div class="comparison-primary">
          <div class="triptych">
            <ComparisonViewer title="前时相" :date="phaseDate(selectedPair.before)" :image="before" :bounds="bounds" :focus="focus" :selected="selected" :view="view" :loading="loading" :error="errors[0]" @view="view = $event" />
            <ComparisonViewer title="后时相" :date="phaseDate(selectedPair.after)" :image="after" :bounds="bounds" :focus="focus" :selected="selected" :view="view" :loading="loading" :error="errors[1]" @view="view = $event" />
            <ComparisonViewer title="变化结果" :date="`${phaseDate(selectedPair.before)} → ${phaseDate(selectedPair.after)}`" :image="before" :mask="mask" :bounds="bounds" :focus="focus" :selected="selected" :view="view" :loading="loading" :error="errors[2] || errors[0]" @view="view = $event" />
          </div>
          <section class="panel hotspots-panel">
            <div class="section-heading"><h2>变化区域 <span class="subtle">{{ ranked.length }}</span></h2><RouterLink to="/reports" class="text-link">全部区域 ↗</RouterLink></div>
            <div v-if="ranked.length" class="hotspot-grid"><HotspotCard v-for="(patch, index) in visible" :key="patchId(patch) || (page - 1) * pageSize + index" :patch="patch" :rank="(page - 1) * pageSize + index + 1" :image="after" :color="selectedPair.color" :selected="selected === patch" @select="selectPatch(patch)" /></div>
            <p v-else class="section-empty">{{ selectedPair.status !== 'succeeded' ? '检测完成后显示变化区域' : selectedPair.geojson ? '此时相组没有检测到变化图斑' : '变化图斑数据不可用' }}</p>
            <nav v-if="pageCount > 1" class="comparison-pagination" aria-label="变化区域分页">
              <div class="page-buttons">
                <button type="button" class="page-button" aria-label="上一页" :disabled="page === 1" @click="page--"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m14 6-6 6 6 6" /></svg></button>
                <template v-for="item in pageItems" :key="item">
                  <button v-if="typeof item === 'number'" type="button" class="page-button" :class="{ active: page === item }" :aria-label="`第 ${item} 页`" :aria-current="page === item ? 'page' : undefined" @click="page = item">{{ item }}</button>
                  <span v-else class="page-ellipsis" aria-hidden="true">···</span>
                </template>
                <button type="button" class="page-button" aria-label="下一页" :disabled="page === pageCount" @click="page++"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m10 6 6 6-6 6" /></svg></button>
              </div>
              <form class="page-jump" @submit.prevent="goToPage">
                <label for="comparison-jump-page">跳至</label>
                <input id="comparison-jump-page" v-model="jumpPage" type="text" inputmode="numeric" pattern="[0-9]+" required :aria-label="`输入页码，共 ${pageCount} 页`" autocomplete="off" />
                <span>页</span><button type="submit" class="page-jump-submit">跳转</button>
              </form>
            </nav>
          </section>
        </div>
        <aside class="insights-rail"><section class="panel insights-panel"><div class="section-heading"><h2>变化概况</h2><span class="eyebrow">当前时相组</span></div><ResultMetrics :stats="selectedPair.stats" /></section><ConfidenceHistogram :stats="selectedPair.stats" /></aside>
      </div>
    </template>
  </div>
</template>
