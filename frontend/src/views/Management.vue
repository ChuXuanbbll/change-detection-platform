<script setup lang="ts">
import { ref, onMounted, onUnmounted } from 'vue';
import {
  settingsState,
  models,
  refreshSettings,
  settingsHTTP,
  waitOperation,
  type Settings,
  type ManagedModel,
  type Operation,
} from '../utils/settings';
const tab = ref('模型');
const draft = ref<Settings>();
const expected = ref(0);
const busy = ref(false);
const message = ref('');
const error = ref('');
const newAIKey = ref('');
const modelName = ref('');
const modelPath = ref('');
const upload = ref<File>();
const controller = new AbortController();
const labels: Record<string, [string, string, number, number, number]> = {
  threshold: ['变化概率阈值', '0–1', 0, 1, 0.01],
  resolution: ['推理分辨率', '米', 0.01, 10000, 0.1],
  kernel_size: ['形态学核大小', '像元', 1, 99, 1],
  min_area_pixels: ['最小图斑', '像元', 0, 100000000, 1],
  gpu_device: ['GPU 设备', '编号', 0, 31, 1],
  batch_size: ['推理批大小', '块', 1, 64, 1],
  prefetch_batches: ['预取批数', '批', 0, 16, 1],
  timeout_seconds: ['推理超时', '秒', 30, 86400, 1],
  workers: ['检测并发', '任务', 1, 8, 1],
  queue_capacity: ['检测等待队列', '任务', 1, 1024, 1],
  concurrency: ['瓦片并发', '请求 / 进程', 1, 8, 1],
  max_pending: ['瓦片等待队列', '请求', 1, 64, 1],
  wait_ms: ['瓦片等待时间', '毫秒', 5, 1000, 1],
  cache_items: ['PNG 缓存条数', '张', 0, 8192, 1],
  cache_mb: ['缓存容量', 'MiB', 1, 1024, 1],
  handles: ['数据集句柄', '个 / 进程', 1, 32, 1],
  gdal_cache_mb: ['GDAL 缓存', 'MiB / 进程', 16, 512, 1],
  inactive_layers: ['非活动时相', '个', 0, 12, 1],
  idle_seconds: ['闲置释放时间', '秒', 10, 3600, 1],
};
async function reload() {
  await refreshSettings();
  if (settingsState.value) {
    const next = JSON.parse(JSON.stringify(settingsState.value.settings)) as Settings;
    next.ai ||= { api_base: '', api_key: '', model: '', timeout_ms: 45000 };
    draft.value = next;
    newAIKey.value = '';
    expected.value = settingsState.value.revision;
  }
}
async function run(action: () => Promise<unknown>, refresh = true) {
  if (busy.value) return;
  busy.value = true;
  error.value = '';
  message.value = '正在准备…';
  try {
    await action();
    if (refresh) await reload();
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e);
    message.value = '';
  } finally {
    busy.value = false;
  }
}
async function operation(path: string, data: unknown) {
  const response = await settingsHTTP.post<Operation>(path, data);
  return waitOperation(response.data, (value) => (message.value = value), controller.signal);
}
function save(validate = false) {
  return run(async () => {
    const payload = draft.value ? (JSON.parse(JSON.stringify(draft.value)) as Settings) : undefined;
    if (payload?.ai && newAIKey.value.trim()) payload.ai.api_key = newAIKey.value.trim();
    const result = await operation(
      `/settings/${validate ? 'validate' : 'apply'}`,
      validate ? payload : { expected_revision: expected.value, settings: payload },
    );
    if (validate && result) {
      const report = result as { scenes: number; available: number };
      message.value = `参数与目录校验通过：${report.scenes} 张影像，${report.available} 张可用；尚未应用`;
    }
    if (!validate) newAIKey.value = '';
  }, !validate);
}
function addSource() {
  draft.value?.sources.push({
    id: crypto.randomUUID(),
    name: '新影像来源',
    path: '',
    enabled: true,
    year: null,
  });
}
function move(index: number, offset: number) {
  const s = draft.value!.sources;
  [s[index], s[index + offset]] = [s[index + offset], s[index]];
}
function pickFile(event: Event) {
  upload.value = (event.target as HTMLInputElement).files?.[0];
}
function importFile() {
  return run(async () => {
    if (!upload.value) throw new Error('请选择单文件 ONNX');
    const r = await settingsHTTP.post(
      `/models/import?name=${encodeURIComponent(modelName.value || upload.value.name)}`,
      upload.value,
      {
        headers: { 'Content-Type': 'application/octet-stream' },
        onUploadProgress: (e) =>
          (message.value = `上传 ${Math.round((e.loaded / (e.total || upload.value!.size)) * 100)}%`),
      },
    );
    if (r.status === 202)
      await waitOperation(r.data, (v) => (message.value = v), controller.signal);
    else message.value = '相同模型已存在，已复用原制品';
  });
}
function toggle(model: ManagedModel) {
  return run(async () => {
    await settingsHTTP.put(`/models/${model.id}`, {
      name: model.name,
      description: model.description,
      enabled: !model.enabled,
    });
    message.value = model.enabled ? '已停用；历史批次仍可解析' : '已启用';
  });
}
onMounted(() => run(reload, false));
onUnmounted(() => controller.abort());
</script>

<template>
  <section class="management">
    <div class="management-title">
      <div>
        <h2>管理与配置</h2>
        <p>本机服务配置 · 变更只影响新批次，正在运行的批次保留原模型与参数。</p>
      </div>
      <span v-if="settingsState" class="config-state"
        >版本 {{ settingsState.revision }} ·
        {{ settingsState.effective ? '已生效' : '尚未同步' }}</span
      >
    </div>
    <div role="tablist" aria-label="配置分类" class="management-tabs">
      <button
        v-for="name in ['模型', '影像来源', '检测默认值', 'AI 配置', '性能与诊断']"
        :key="name"
        role="tab"
        :aria-selected="tab === name"
        :class="{ selected: tab === name }"
        @click="tab = name"
      >
        {{ name }}
      </button>
    </div>
    <p v-if="error" role="alert" class="config-error">{{ error }}</p>
    <p v-if="message" role="status">{{ message }}</p>
    <p v-if="settingsState && !settingsState.effective" class="config-error">
      配置未同步：{{ settingsState.error || 'Python 服务的生效版本未确认' }}
      <button :disabled="busy" @click="run(() => operation('/settings/sync', {}))">重新同步</button>
    </p>
    <fieldset :disabled="busy" class="config-content">
      <template v-if="tab === '模型'">
        <p>
          协议：RGB 前后时相、ImageNet 归一化、512 × 512 NCHW 输入，单通道变化 logits 或双通道第 1
          类为变化。结构和 CUDA 试运行通过不代表检测精度达标。
        </p>
        <div class="model-import panel">
          <label>模型名称<input v-model="modelName" class="input" placeholder="可选名称" /></label
          ><label>上传模型文件<input type="file" accept=".onnx" @change="pickFile" /></label
          ><button class="btn-primary" :disabled="!upload" @click="importFile">上传并验证</button
          ><label
            >读取服务端模型路径<input
              v-model="modelPath"
              class="input"
              placeholder="D:\models\model.onnx" /></label
          ><button
            class="btn-secondary"
            :disabled="!modelPath"
            @click="run(() => operation('/models/import', { path: modelPath, name: modelName }))"
          >
            从路径导入</button
          ><small>仅支持内嵌权重的单文件 ONNX。相同内容复用制品，不覆盖旧版本。</small>
        </div>
        <p v-if="!models.length">尚无模型，请导入 ONNX。</p>
        <article v-for="model in models" :key="model.id" class="panel model-card">
          <div class="model-heading">
            <strong>{{ model.name }}</strong
            ><span>{{ model.status }} · {{ model.enabled ? '启用' : '停用' }}</span>
          </div>
          <code>{{ model.sha256 || '待计算哈希' }}</code>
          <p v-if="model.error" class="config-error">{{ model.error }}</p>
          <details>
            <summary>信息与输入输出映射</summary>
            <label>名称<input v-model="model.name" class="input" /></label
            ><label>说明<input v-model="model.description" class="input" /></label
            ><button
              class="btn-secondary"
              @click="
                run(async () => {
                  await settingsHTTP.put(`/models/${model.id}`, model);
                  message = '信息已保存';
                })
              "
            >
              保存信息
            </button>
            <pre>{{
              JSON.stringify({ inputs: model.inputs, outputs: model.outputs }, null, 2)
            }}</pre>
            <label
              >前时相输入<select v-model="model.mapping.input_a" class="input">
                <option v-for="input in model.inputs" :key="input.name">{{ input.name }}</option>
              </select></label
            ><label v-if="(model.inputs?.length || 0) > 1"
              >后时相输入<select v-model="model.mapping.input_b" class="input">
                <option v-for="input in model.inputs" :key="input.name">{{ input.name }}</option>
              </select></label
            ><label
              >变化 logits 输出<select v-model="model.mapping.output" class="input">
                <option v-for="output in model.outputs" :key="output.name">
                  {{ output.name }}
                </option>
              </select></label
            >
          </details>
          <div class="config-actions">
            <button
              class="btn-secondary"
              @click="
                run(() => operation(`/models/${model.id}/validate`, { mapping: model.mapping }))
              "
            >
              验证 CUDA / 映射</button
            ><button
              class="btn-secondary"
              :disabled="model.status !== 'ready'"
              @click="toggle(model)"
            >
              {{ model.enabled ? '停用' : '启用' }}</button
            ><button
              v-if="draft"
              class="btn-secondary"
              :disabled="!model.enabled || model.status !== 'ready'"
              @click="draft.default_model = model.id"
            >
              {{ draft.default_model === model.id ? '默认模型（应用后生效）' : '设为默认' }}</button
            ><button
              class="btn-ghost"
              @click="
                run(async () => {
                  await settingsHTTP.delete(`/models/${model.id}`, {
                    headers: { 'Content-Type': 'application/json' },
                  });
                  message = '已移除未引用登记';
                })
              "
            >
              移除登记
            </button>
          </div>
        </article>
      </template>
      <template v-if="tab === '影像来源' && draft">
        <p>
          这里读取服务运行主机的本地或 UNC
          共享目录。同月跨来源合成，优先采用真实分辨率更高的有效像元；分辨率相同时按下列顺序，再按相对路径。月份必须来自月份目录。
        </p>
        <article
          v-for="(source, index) in draft.sources"
          :key="source.id"
          class="panel source-card"
        >
          <label>名称<input v-model="source.name" class="input" required /></label
          ><label
            >服务端目录<input
              v-model="source.path"
              class="input"
              required
              placeholder="D:\imagery\2025年 或 \\server\share" /></label
          ><label
            >年份（空值按目录）<input
              v-model.number="source.year"
              type="number"
              min="1900"
              max="2199"
              class="input"
              @change="source.year = source.year || null" /></label
          ><label><input v-model="source.enabled" type="checkbox" />启用</label>
          <div class="config-actions">
            <button :disabled="index === 0" @click="move(index, -1)">上移</button
            ><button :disabled="index === draft.sources.length - 1" @click="move(index, 1)">
              下移</button
            ><button @click="draft.sources.splice(index, 1)">移除来源</button>
          </div>
          <small>{{ source.id }} · 仅移除配置，不删除影像文件</small>
        </article>
        <button class="btn-secondary" @click="addSource">添加影像来源</button>
      </template>
      <template v-if="tab === '检测默认值' && draft"
        ><p>
          新批次自动填入这些值；概览右栏可以覆盖。模型归一化、四邻域过滤和八邻域统计属于固定算法协议。
        </p>
        <div class="config-grid">
          <label v-for="(_, key) in draft.detection" :key="key"
            >{{ labels[key]?.[0] || key
            }}<input
              v-model.number="draft.detection[key]"
              class="input"
              type="number"
              :min="labels[key]?.[2]"
              :max="labels[key]?.[3]"
              :step="labels[key]?.[4]"
              required
            /><small>{{ labels[key]?.[1] }} · 新批次生效</small></label
          >
        </div></template
      >
      <template v-if="tab === 'AI 配置' && draft">
        <p>
          AI 服务配置只在本机管理入口维护。API key
          不会回显完整内容，保存时留空或保留缩略值表示继续使用当前 key。
        </p>
        <div class="config-grid ai-config">
          <label
            >API 地址<input
              v-model="draft.ai.api_base"
              class="input"
              placeholder="https://api.openai.com/v1"
          /></label>
          <label
            >模型名称<input v-model="draft.ai.model" class="input" placeholder="gpt-4o-mini"
          /></label>
          <label
            >请求超时（毫秒）<input
              v-model.number="draft.ai.timeout_ms"
              class="input"
              type="number"
              min="1000"
              max="600000"
              step="1000"
          /></label>
          <label
            >API key<input
              v-model="newAIKey"
              class="input"
              type="password"
              autocomplete="new-password"
              placeholder="sk-..."
          /></label>
        </div>
        <small
          >当前值：{{ settingsState?.settings.ai?.api_key || '未配置' }}。完整 key
          只由后端保存并用于请求。</small
        >
      </template>
      <template v-if="tab === '性能与诊断' && draft"
        ><p>
          默认每台设备同时运行一个检测任务；增加并发可能耗尽显存。队列参数变更需要当前检测队列空闲。瓦片进程会排空并重建；浏览器缓存在当前批次结束后重建。
        </p>
        <details v-for="group in ['runtime', 'tiles', 'browser'] as const" :key="group">
          <summary>
            {{ { runtime: 'GPU 与任务', tiles: '后端瓦片', browser: '浏览器缓存' }[group] }} ·
            高级设置
          </summary>
          <div class="config-grid">
            <label v-for="(_, key) in draft[group]" :key="key"
              >{{ labels[key]?.[0] || key
              }}<input
                v-model.number="draft[group][key]"
                type="number"
                class="input"
                :min="group === 'browser' && key === 'cache_mb' ? 16 : labels[key]?.[2]"
                :max="group === 'browser' && key === 'concurrency' ? 16 : labels[key]?.[3]"
                :step="labels[key]?.[4]"
                required
              /><small>{{ labels[key]?.[1] }} · 应用后生效</small></label
            >
          </div>
        </details>
        <details open>
          <summary>运行诊断（只读）</summary>
          <p>
            解释器、数据目录和服务入口来自部署环境；CUDA 验证显示实际试运行结果，未验证时为 null。
          </p>
          <pre>{{ JSON.stringify(settingsState?.diagnostics, null, 2) }}</pre>
        </details>
        <details>
          <summary>配置历史与回退</summary>
          <div v-for="revision in settingsState?.history" :key="revision.id" class="config-actions">
            <span>v{{ revision.id }} · {{ revision.created_at }} · {{ revision.state }}</span
            ><button
              v-if="revision.id !== expected"
              class="btn-secondary"
              @click="
                run(() =>
                  operation('/settings/rollback', {
                    expected_revision: expected,
                    rollback_revision: revision.id,
                  }),
                )
              "
            >
              回退到此配置
            </button>
          </div>
        </details></template
      >
    </fieldset>
    <footer class="config-footer">
      <span>编辑后校验并应用；未应用的修改不会生效。</span
      ><button class="btn-secondary" :disabled="busy" @click="run(reload, false)">重新加载</button
      ><button class="btn-secondary" :disabled="busy || !draft" @click="save(true)">校验配置</button
      ><button class="btn-primary" :disabled="busy || !draft" @click="save()">
        {{ busy ? '处理中…' : '应用配置' }}
      </button>
    </footer>
  </section>
</template>

<style scoped>
.management {
  height: 100%;
  overflow: auto;
  padding: 24px;
  color: var(--text-primary, #e1e9ed);
}
.management-title,
.model-heading,
.config-actions,
.config-footer {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}
.management-title {
  justify-content: space-between;
}
.management h2 {
  font-size: 24px;
}
.management p,
.management small {
  color: #9aacb8;
  line-height: 1.7;
}
.management-tabs {
  display: flex;
  gap: 8px;
  margin: 20px 0;
}
.management-tabs button {
  padding: 10px 16px;
  border-bottom: 2px solid transparent;
}
.management-tabs .selected {
  color: #67e8f9;
  border-color: #22d3ee;
}
.config-content {
  border: 0;
  min-width: 0;
  padding: 0 0 28px;
}
.config-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: 18px;
  margin: 20px 0;
}
.management label {
  display: flex;
  flex-direction: column;
  gap: 8px;
  min-width: 0;
  font-size: 13px;
}
.model-import,
.source-card {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
  gap: 16px;
  padding: 20px;
  margin: 16px 0;
  align-items: end;
}
.model-card {
  padding: 20px;
  margin: 12px 0;
}
.model-card code {
  display: block;
  overflow-wrap: anywhere;
  color: #92b5c5;
  font-size: 12px;
  margin: 10px 0;
}
.management details {
  margin: 18px 0;
  padding: 12px;
  border: 1px solid #29404c;
  border-radius: 8px;
}
.management summary {
  cursor: pointer;
}
.management pre {
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  font-size: 12px;
  color: #a4bbc7;
  margin: 10px 0;
}
.config-actions {
  margin-top: 12px;
}
.config-error {
  color: #fda4af !important;
  overflow-wrap: anywhere;
  white-space: pre-wrap;
}
.config-footer {
  position: sticky;
  bottom: -24px;
  background: #101e2cf5;
  padding: 16px;
  border-top: 1px solid #29404c;
  z-index: 2;
}
.config-footer > span {
  margin-right: auto;
  font-size: 12px;
  color: #9aacb8;
}
.config-state {
  color: #67e8f9;
  font-size: 13px;
}
.management button:disabled {
  opacity: 0.45;
  cursor: wait;
}
@media (max-width: 600px) {
  .management {
    padding: 12px;
  }
  .management-tabs {
    overflow: auto;
  }
  .management-tabs button {
    white-space: nowrap;
    padding: 8px;
  }
  .config-footer {
    bottom: -12px;
  }
}
</style>
