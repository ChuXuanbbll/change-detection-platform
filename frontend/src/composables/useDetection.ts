import { ref, computed, onUnmounted } from 'vue';
import axios from 'axios';
import { fetchLocalImagery, getLocalProgress, createTask, getTask, getTaskResult, getMaskOverlay, downloadTaskResult } from '../utils/api';
import type { AreaBounds, AreaPolygon, ChangePatchCollection, DetectionStats, ImageInfo, TaskInfo } from '../utils/api';
import { buildTimeline, pairBounds, phaseDate, type TimelinePair } from '../utils/timeline';
import { parseDetectionStats } from '../utils/results';

export interface ResultOverlayPayload {
  aoi?: AreaPolygon;
  pair?: TimelinePair;
  maskDataUrl?: string;
  maskBounds?: [number, number, number, number];
  resultBounds?: [number, number, number, number];
  geojson?: ChangePatchCollection;
  stats?: DetectionStats;
}

export function useDetection() {
  const currentTaskId = ref('');
  const beforeImage = ref<ImageInfo | null>(null);
  const afterImage = ref<ImageInfo | null>(null);
  const isSubmitting = ref(false);
  const overlayMsg = ref('');
  const taskStatus = ref<TaskInfo['status'] | ''>('');
  const taskProgress = ref(0);
  const taskStage = ref('');
  const stages: Record<string, string> = {
    preparing: '正在准备影像', reading_images: '正在读取影像', cropping: '正在裁切影像',
    mosaicking: '正在拼接影像', saving_images: '正在保存影像', queued: '等待检测',
    aligning: '正在对齐影像', model_loading: '正在加载模型', inference: '正在检测',
    reprojecting: '正在整理检测结果', postprocessing: '正在处理变化区域',
    vectorizing: '正在生成变化图斑', statistics: '正在统计检测结果', saving: '正在保存检测结果',
  };
  const taskStageLabel = computed(() => {
    if (taskStatus.value === 'succeeded') return '检测完成';
    if (taskStatus.value === 'failed') return '检测已停止';
    const label = stages[taskStage.value] || '正在检测';
    return pairs.value.length ? `${label} · 第 ${Math.min(pairIndex + 1, pairs.value.length)}/${pairs.value.length} 组` : label;
  });
  function advanceProgress(value: number): void {
    if (Number.isFinite(value)) taskProgress.value = Math.max(taskProgress.value, Math.min(99.9, value));
  }
  function updatePairProgress(task: TaskInfo): void {
    taskStage.value = task.stage || (task.status === 'pending' ? 'queued' : 'inference');
    advanceProgress(20 + 80 * (pairIndex + Math.max(0, Math.min(99, task.progress ?? 0)) / 100) / pairs.value.length);
  }
  const taskError = ref('');
  const currentTask = ref<TaskInfo | null>(null);
  const images = ref<ImageInfo[]>([]);
  const pairs = ref<TimelinePair[]>([]);
  const batchRunning = ref(false);
  const batchMessage = ref('');
  let pairIndex = 0;
  let batchId = "";
  let batchAOI: AreaPolygon | undefined;
  let pollFailures = 0;
  let taskParams: Record<string, number | string | boolean> = {};
  let preparationTimer: ReturnType<typeof setTimeout> | null = null;
  let preparationActive = false;
  let preparationFailures = 0;
  let pollTimer: ReturnType<typeof setTimeout> | null = null;
  // Invalidate in-flight replies whenever a batch or task is replaced.
  let generation = 0;
  type ResultListener = (payload: ResultOverlayPayload) => void | Promise<void>;
  let resultListener: ResultListener | null = null;
  const inProgress = computed(() => batchRunning.value);
  const isSuccess = computed(() => !batchRunning.value && taskStatus.value === 'succeeded');
  function stopPreparationPolling(): void {
    preparationActive = false;
    if (preparationTimer) clearTimeout(preparationTimer);
    preparationTimer = null;
  }
  function pollPreparation(id: string, token: number): void {
    preparationTimer = setTimeout(async () => {
      preparationTimer = null;
      try {
        const progress = await getLocalProgress(id);
        if (token !== generation || !preparationActive) return;
        preparationFailures = 0;
        taskError.value = '';
        // The synchronous response confirms saved metadata and hands over to tasks.
        if (progress.status === 'running') {
          taskStage.value = progress.stage;
          advanceProgress(Math.min(99, progress.progress) * .2);
        }
      } catch (error) {
        if (token !== generation || !preparationActive) return;
        preparationFailures++;
        if (!axios.isAxiosError(error) || error.response?.status !== 404) {
          taskError.value = '影像准备进度暂不可达，正在重试';
        }
      }
      if (token === generation && preparationActive) pollPreparation(id, token);
    }, Math.min(15000, 1000 * 2 ** Math.min(preparationFailures, 4)));
  }
  function stopPolling(): void {
    stopPreparationPolling();
    if (pollTimer) clearTimeout(pollTimer);
    pollTimer = null;
  }
  function resetTask(): void {
    generation++;
    pollFailures = 0;
    stopPolling();
    currentTaskId.value = '';
    currentTask.value = null;
    taskStatus.value = '';
    taskProgress.value = 0;
    taskStage.value = '';
    preparationFailures = 0;
    taskError.value = '';
    overlayMsg.value = '';
    isSubmitting.value = false;
    batchRunning.value = false;
    batchMessage.value = '';
    images.value = [];
    pairs.value = [];
    pairIndex = 0;
    batchAOI = undefined;
  }
  function resetDetection(): void {
    resetTask();
    beforeImage.value = null;
    afterImage.value = null;
  }
  async function submitTask(
    getSelected: () => { algorithm: string; model_id?: string; config_revision?: number; params: Record<string, number | string | boolean> } | undefined,
    area: AreaBounds | null,
    selection?: { region_id: string; scene_ids: string[]; aoi?: AreaPolygon },
  ): Promise<void> {
    if (isSubmitting.value || inProgress.value) return;
    if (!area) {
      taskError.value = '请先在地图绘制或输入检测区域';
      return;
    }
    if (selection && (!selection.region_id || !selection.scene_ids.length)) {
      taskError.value = '请先拖入至少两个不同月份的影像';
      return;
    }
    const selected = getSelected();
    if (!selected) { taskError.value = '所选模型或配置尚未就绪，请检查检测服务'; return; }
    resetDetection();
    const token = generation;
    taskParams = { ...selected.params, provider: 'cuda' };
    isSubmitting.value = true;
    batchRunning.value = true;
    taskStatus.value = 'pending';
    taskStage.value = 'preparing';
    batchMessage.value = selection ? '正在裁切已加载影像并按月拼接…' : '正在自动获取该区域全部时相影像…';
    try {
      const progressId = crypto.randomUUID();
      preparationActive = true;
      pollPreparation(progressId, token);
      const frozenSelection = selection ? JSON.parse(JSON.stringify(selection)) : undefined;
      batchAOI = frozenSelection?.aoi;
      const response = await fetchLocalImagery({ progress_id: progressId, bounds: [area.west, area.south, area.east, area.north], ...frozenSelection, model_id:selected.model_id, config_revision:selected.config_revision, params:selected.params });
      if (token !== generation) return;
      stopPreparationPolling();
      if (batchAOI && response.items.length && response.items.some(image => {
        try {
          const meta = JSON.parse(image.meta_json || '{}');
          return meta.aoi?.type !== 'Polygon' || meta.region_id !== frozenSelection.region_id ||
            JSON.stringify(meta.aoi.coordinates) !== JSON.stringify(batchAOI!.coordinates);
        } catch { return true; }
      })) throw new Error('裁切服务未确认多边形范围，请重启 Go 和 Python 服务后重新检测');
      taskError.value = '';
      advanceProgress(20);
      batchId = response.batch_id;
      images.value = response.items;
      const timeline = buildTimeline(response.items);
      images.value = timeline.images;
      pairs.value = timeline.pairs;
      if (!pairs.value.length) throw new Error('至少需要两个有有效像元的不同月份影像');
      await startPair(token);
    } catch (e: unknown) {
      if (token === generation) failBatch(e instanceof Error ? e.message : '获取影像失败');
    } finally { if (token === generation) isSubmitting.value = false; }
  }

  function failBatch(message: string): void {
    stopPolling();
    taskError.value = message;
    taskStatus.value = 'failed';
    batchRunning.value = false;
    const pair = pairs.value[pairIndex];
    if (pair && pair.status !== 'succeeded') pair.status = 'failed';
    batchMessage.value = `检测已停止，已完成 ${pairs.value.filter(pair => pair.status === 'succeeded').length} / ${pairs.value.length} 组`;
  }

  async function startPair(token: number): Promise<void> {
    if (token !== generation) return;
    const pair = pairs.value[pairIndex];
    taskStage.value = 'queued';
    beforeImage.value = pair.before;
    afterImage.value = pair.after;
    batchMessage.value = `检测 ${pairIndex + 1} / ${pairs.value.length}：${phaseDate(pair.before)} → ${phaseDate(pair.after)}`;
    isSubmitting.value = true;
    try {
      const task = await createTask({ before_id: pair.before.id, after_id: pair.after.id,
        algorithm: 'hebei_onnx', batch_id: batchId, params: { ...taskParams } });
      if (token !== generation) return;
      pair.taskId = task.id;
      pair.status = task.status === 'running' ? 'running' : 'pending';
      currentTaskId.value = task.id;
      currentTask.value = task;
      taskStatus.value = 'running';
      updatePairProgress(task);
      schedulePoll(task.id, token, pair);
    } catch (e: unknown) {
      if (token === generation) failBatch(e instanceof Error ? e.message : '提交任务失败');
    } finally { if (token === generation) isSubmitting.value = false; }
  }

  function schedulePoll(taskId: string, token: number, pair: TimelinePair): void {
    pollTimer = setTimeout(async () => {
      pollTimer = null;
      try {
        const task = await getTask(taskId);
        if (token !== generation) return;
        pollFailures = 0;
        taskError.value = '';
        pair.status = task.status === 'running' ? 'running' : 'pending';
        updatePairProgress(task);
        currentTask.value = task;
        if (task.status === 'succeeded' || task.status === 'completed') {
          taskError.value = '';
          pair.status = 'succeeded';
          void loadResultOverlay(taskId, token, pairBounds(pair.before, pair.after), pair).then(async data => {
            if (!data || token !== generation) { if (token === generation) pair.warning = '结果预览加载失败，可在对比页重试'; return; }
            pair.stats = data.stats; pair.geojson = data.geojson; data.pair = { ...pair };
            try { if (resultListener) await resultListener(data); } catch { pair.warning = '预览显示失败，可重试'; }
          });
          pairIndex++;
          advanceProgress(20 + 80 * pairIndex / pairs.value.length);
          if (pairIndex < pairs.value.length) await startPair(token);
          else {
            taskStatus.value = 'succeeded';
            taskProgress.value = 100;
            taskStage.value = 'completed';
            batchRunning.value = false;
            batchMessage.value = `检测完成：${images.value.length} 张影像，${pairs.value.length} 组相邻时相`;
          }
        } else if (task.status === 'failed') {
          failBatch(task.error_msg ?? '任务失败');
        } else schedulePoll(taskId, token, pair);
      } catch (e: unknown) {
        if (token === generation) {
          pollFailures++;
          taskError.value = `任务状态暂不可达，正在重试（${pollFailures}）：${e instanceof Error ? e.message : String(e)}`;
          schedulePoll(taskId, token, pair);
        }
      }
    }, Math.min(15000, 1000 * 2 ** Math.min(pollFailures, 4)));
  }

  async function waitTaskResult(taskId: string, token: number) {
    let lastError: unknown;
    for (let i = 0; i < 120 && token === generation; i++) {
      try { return await getTaskResult(taskId); }
      catch (e: unknown) {
        lastError = e;
        const ax = axios.isAxiosError(e) ? e : null;
        const code = (ax?.response?.data as { code?: string } | undefined)?.code;
        const status = ax?.response?.status;
        if (code !== 'RESULT_NOT_READY' && status !== 404 && status !== 425 &&
            !/result not ready|RESULT_NOT_READY|not ready/i.test(e instanceof Error ? e.message : '')) throw e;
        await new Promise(resolve => setTimeout(resolve, 1000));
      }
    }
    if (token !== generation) return;
    throw lastError;
  }

  async function loadResultOverlay(taskId: string, token = generation,
    bounds?: ResultOverlayPayload['resultBounds'], pair?: TimelinePair): Promise<ResultOverlayPayload | undefined> {
    try {
      const result = await waitTaskResult(taskId, token);
      if (!result || token !== generation) return;
      const payload: ResultOverlayPayload = { resultBounds: bounds, aoi: batchAOI };
      const warnings: string[] = [];
      if (result.stats_json) {
        try {
          const raw = JSON.parse(result.stats_json);
          if (!raw || typeof raw !== 'object' || Array.isArray(raw)) throw new Error('invalid stats');
          payload.stats = parseDetectionStats(raw);
        } catch { warnings.push('统计数据格式无效'); }
      }
      if (result.mask_path) {
        if (pair) pair.maskPath = result.mask_path;
        try {
          const overlay = await getMaskOverlay(result.mask_path, 0.45, pair?.color);
          payload.maskDataUrl = 'data:image/png;base64,' + overlay.png_b64;
          payload.maskBounds = overlay.bounds;
        } catch { warnings.push('变化掩膜加载失败'); }
      }
      if (token !== generation) return;
      if (result.geojson) payload.geojson = result.geojson;
      else {
        // Compatibility with older servers that only return file paths.
        try {
          const blob = await downloadTaskResult(taskId, 'geojson');
          payload.geojson = JSON.parse(await blob.text()) as ChangePatchCollection;
        } catch { warnings.push('变化图斑数据加载失败'); }
      }
      if (token !== generation) return;
      if (pair) pair.warning = warnings.join('；');
      overlayMsg.value = pairs.value.map(pair => pair.warning).filter(Boolean).join('；') || warnings.join('；');
      return payload;
    } catch (e: unknown) {
      if (token === generation) overlayMsg.value = e instanceof Error ? e.message : '加载结果失败';
    }
  }

  function onResult(callback: ResultListener): void { resultListener = callback; }
  onUnmounted(() => { generation++; stopPolling(); resultListener = null; });
  return {
    currentTaskId, beforeImage, afterImage, isSubmitting, overlayMsg, taskStatus, taskProgress,
    taskError, taskStage, taskStageLabel, currentTask, images, pairs, batchMessage, inProgress, isSuccess, submitTask,
    loadResultOverlay, stopPolling, resetDetection, onResult,
  };
}
