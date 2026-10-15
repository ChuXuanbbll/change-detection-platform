<script setup lang="ts">
import { computed, onMounted } from 'vue';
import { models, settingsState, refreshSettings, managementError } from '../utils/settings';
import { useWorkspace } from '../composables/useWorkspace';
const { inProgress, selectedModel: selected, detectionSettings: params } = useWorkspace();
const available = computed(() => models.value.filter((m) => m.status === 'ready' && m.enabled));
const labels: Record<string, string> = {
  threshold: '变化阈值',
  resolution: '推理分辨率（米）',
  kernel_size: '形态学核（像元）',
  min_area_pixels: '最小图斑（像元）',
};
onMounted(() => {
  void refreshSettings().catch(() => {});
});
</script>
<template>
  <div class="panel p-3 space-y-3">
    <h3 class="panel-title">算法参数</h3>
    <p v-if="managementError" role="alert" class="text-xs text-rose-400">
      {{ managementError }} <button @click="refreshSettings().catch(() => {})">重试</button>
    </p>
    <label class="text-xs block"
      >本批次模型<select v-model="selected" class="input w-full" :disabled="inProgress">
        <option value="" disabled>请选择已验证模型</option>
        <option v-for="model in available" :key="model.id" :value="model.id">
          {{ model.name }} · {{ model.sha256.slice(0, 10) }}
        </option>
      </select></label
    >
    <p v-if="!available.length || !settingsState?.effective" class="text-xs text-rose-400">
      模型或配置尚未就绪，请前往 <RouterLink to="/management">管理与配置</RouterLink> 验证并应用。
    </p>
    <label v-for="(_, key) in params" :key="key" class="flex items-center gap-2 text-xs"
      ><span class="w-28">{{ labels[key] || key }}</span
      ><input
        v-model.number="params[key]"
        :disabled="inProgress"
        class="input flex-1 min-w-0"
        type="number"
        :step="key === 'threshold' || key === 'resolution' ? 0.01 : 1"
        :min="key === 'resolution' ? 0.01 : key === 'kernel_size' ? 1 : 0"
        :max="key === 'threshold' ? 1 : key === 'kernel_size' ? 99 : undefined"
    /></label>
  </div>
</template>
