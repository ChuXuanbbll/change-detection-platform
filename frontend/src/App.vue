<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import { useRoute } from 'vue-router';
import { provideWorkspace } from './composables/useWorkspace';
const { pairs, inProgress, taskProgress, batchMessage, taskError, indexLoading, indexError, retryIndex } = provideWorkspace();
const route = useRoute();
const menuOpen = ref(false);
const completed = computed(() => pairs.value.filter(pair => pair.status === 'succeeded').length);
const navigation = [
  { path: '/overview', label: '概览', sub: '区域与任务', icon: 'M3 11 12 3l9 8M5 10v11h5v-7h4v7h5V10' },
  { path: '/comparison', label: '对比', sub: '时相与变化', icon: 'M3 4h7v16H3zM14 4h7v16h-7zM10 12h4' },
  { path: '/reports', label: '报告', sub: '结果与导出', icon: 'M5 3h10l4 4v14H5zM9 11h6M9 15h6M15 3v5h4' },
  { path: '/management', label: '管理', sub: '模型与设置', icon: 'M4 7h16M4 17h16M8 4v6M16 14v6' },
];
watch(() => route.path, () => { menuOpen.value = false; });
</script>

<template>
  <div class="workspace-shell" @keydown.esc="menuOpen = false">
    <div class="backdrop-grain" aria-hidden="true" />
    <button v-if="menuOpen" class="nav-scrim" aria-label="关闭导航" @click="menuOpen = false" />
    <aside class="app-nav" :class="{ 'nav-open': menuOpen }">
      <RouterLink to="/overview" class="brand-mark" aria-label="遥感变化检测平台首页"><span>Δ</span><b>时空观测</b></RouterLink>
      <nav aria-label="主导航">
        <RouterLink v-for="item in navigation" :key="item.path" :to="item.path" class="nav-item" :title="item.label">
          <svg viewBox="0 0 24 24" aria-hidden="true"><path :d="item.icon" /></svg>
          <span><strong>{{ item.label }}</strong><small>{{ item.sub }}</small></span>
        </RouterLink>
      </nav>
      <div class="nav-footer"><span class="signal-dot" />遥感变化检测<br><small>同一片土地 · 不同时刻?</small></div>
    </aside>
    <div class="app-workspace">
      <header class="workspace-header">
        <button class="mobile-menu btn-secondary" :aria-expanded="menuOpen" aria-label="打开导航" @click="menuOpen = !menuOpen">☰</button>
        <div><h1>遥感<span>变化检测</span>平台</h1><p>空间观测 / {{ navigation.find(item => item.path === route.path)?.label }}</p></div>
        <div class="header-tools">
          <RouterLink to="/overview" class="batch-indicator" :class="{ 'is-running': inProgress }" :title="taskError || batchMessage">
            <span class="signal-dot" />{{ inProgress ? `检测中 ${Math.floor(taskProgress)}%` : taskError ? '检测已停止' : pairs.length ? `已完成 ${completed} / ${pairs.length} 组` : '等待选择区域' }}
          </RouterLink>
        </div>
      </header>
      <div v-if="indexLoading" role="status" class="px-4 py-2 text-xs text-cyan-300">正在扫描影像目录，完成后即可加载时相…</div>
      <div v-else-if="indexError" role="alert" class="px-4 py-2 text-xs text-rose-400">
        {{ indexError }} <button class="btn-ghost" :disabled="inProgress" @click="retryIndex">重试读取目录</button>
      </div>
      <main class="route-workspace">
        <RouterView v-slot="{ Component }"><KeepAlive><component :is="Component" /></KeepAlive></RouterView>
      </main>
    </div>
  </div>
</template>
