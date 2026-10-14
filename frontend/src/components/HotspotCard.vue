<script setup lang="ts">
import { ref, watch, onMounted } from 'vue';
import L from 'leaflet';
import type { ChangePatch } from '../utils/api';
import { patchBounds, formatArea, formatScore } from '../utils/results';
import { previewProjection, type RasterPreview } from '../utils/previews';
const props = defineProps<{ patch: ChangePatch; rank: number; image?: RasterPreview; selected?: boolean; color: string }>();
defineEmits<{ (e: 'select'): void }>();
const canvas = ref<HTMLCanvasElement | null>(null);
const available = ref(false);
let generation = 0;
async function draw() {
  const token = ++generation;
  available.value = false;
  const extent = patchBounds(props.patch), raster = props.image;
  if (!canvas.value || !extent || !raster) return;
  const bitmap = new Image(); bitmap.src = raster.url;
  try { await bitmap.decode(); } catch { return; }
  if (token !== generation || !canvas.value) return;
  const ctx = canvas.value.getContext('2d'); if (!ctx) return;
  const projection = previewProjection(raster.crs);
  const point = (lng: number, lat: number) => projection.project(L.latLng(lat, lng));
  const sw = point(raster.bounds[1], raster.bounds[0]), ne = point(raster.bounds[3], raster.bounds[2]);
  const lo = point(extent[1], extent[0]), hi = point(extent[3], extent[2]);
  const centerX = (lo.x + hi.x) / 2, centerY = (lo.y + hi.y) / 2;
  const height = Math.max(hi.y - lo.y, (hi.x - lo.x) / 1.8) * 1.5;
  if (!(height > 0)) return;
  const width = height * 1.8, left = centerX - width / 2, top = centerY + height / 2;
  canvas.value.width = 360; canvas.value.height = 200;
  ctx.fillStyle = '#0a1728'; ctx.fillRect(0, 0, 360, 200);
  // Draw the georeferenced preview into a shared projected crop, then its outline.
  ctx.drawImage(bitmap, (sw.x - left) / width * 360, (top - ne.y) / height * 200, (ne.x - sw.x) / width * 360, (ne.y - sw.y) / height * 200);
  ctx.strokeStyle = props.color; ctx.lineWidth = 3;
  const rings = props.patch.geometry.type === 'Polygon' ? props.patch.geometry.coordinates : props.patch.geometry.coordinates.flat();
  for (const ring of rings) {
    ctx.beginPath();
    ring.forEach(([lng, lat], i) => { const p = point(lng, lat); const x = (p.x - left) / width * 360, y = (top - p.y) / height * 200; if (i) ctx.lineTo(x, y); else ctx.moveTo(x, y); });
    ctx.closePath(); ctx.stroke();
  }
  available.value = true;
}
watch(() => [props.patch, props.image, props.color], draw);
onMounted(draw);
</script>
<template>
  <button class="hotspot-card" :class="{ selected }" :aria-pressed="selected" :aria-label="`定位变化区域 ${rank}，${formatArea(patch.properties.area_m2)}`" @click="$emit('select')">
    <div class="hotspot-image"><canvas ref="canvas" aria-hidden="true" /><span v-if="!available" class="thumbnail-placeholder">预览不可用</span><span class="hotspot-rank">{{ String(rank).padStart(2, '0') }}</span></div>
    <div class="hotspot-caption"><strong>{{ formatArea(patch.properties.area_m2) }}</strong><span>置信度 {{ formatScore(patch.properties.confidence_mean) }}</span></div>
  </button>
</template>
