<script setup lang="ts">
import { useWorkspace } from '../composables/useWorkspace';
import { phaseDate } from '../utils/timeline';
import { statusLabel } from '../utils/results';
const { pairs, selectedPairKey } = useWorkspace();
</script>
<template>
  <label class="pair-selector"><span>当前时相组</span>
    <select v-model="selectedPairKey" class="input" :disabled="!pairs.length" aria-label="当前时相组">
      <option value="" disabled>{{ pairs.length ? '请选择时相组' : '暂无检测任务' }}</option>
      <option v-for="(pair, index) in pairs" :key="pair.key" :value="pair.key">{{ index + 1 }}. {{ phaseDate(pair.before) }} → {{ phaseDate(pair.after) }} · {{ statusLabel(pair.status) }}</option>
    </select>
  </label>
</template>
