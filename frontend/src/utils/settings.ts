import { ref } from 'vue';
import axios from 'axios';

export interface Source {
  id: string;
  name: string;
  path: string;
  year?: number | null;
  enabled: boolean;
}
export interface Settings {
  sources: Source[];
  default_model: string;
  detection: Record<string, number>;
  runtime: Record<string, number>;
  tiles: Record<string, number>;
  browser: Record<string, number>;
}
export interface ManagedModel {
  id: string;
  name: string;
  description: string;
  sha256: string;
  status: string;
  enabled: boolean;
  error: string;
  mapping: Record<string, string>;
  inputs: { name: string; shape: (string | number | null)[]; dtype: number }[] | null;
  outputs: { name: string; shape: (string | number | null)[]; dtype: number }[] | null;
}
export interface SettingsState {
  revision: number;
  settings: Settings;
  effective: boolean;
  error: string;
  history: { id: number; state: string; created_at: string }[];
  diagnostics: Record<string, unknown>;
}
export interface Operation {
  id: string;
  status: string;
  error: string;
  result?: unknown;
}
export const settingsState = ref<SettingsState>();
export const models = ref<ManagedModel[]>([]);
export const managementError = ref('');
export const settingsHTTP = axios.create({ baseURL: '/api', timeout: 30 * 60_000 });
settingsHTTP.interceptors.response.use(
  (r) => r,
  (e) => {
    e.message = e.response?.data?.error || e.message;
    return Promise.reject(e);
  },
);
export async function refreshSettings() {
  try {
    const [s, m] = await Promise.all([
      settingsHTTP.get<SettingsState>('/settings'),
      settingsHTTP.get<ManagedModel[]>('/models'),
    ]);
    models.value = m.data;
    settingsState.value = s.data;
    managementError.value = '';
  } catch (e) {
    managementError.value = e instanceof Error ? e.message : String(e);
    throw e;
  }
}
export async function waitOperation(
  op: Operation,
  update: (message: string) => void,
  signal?: AbortSignal,
) {
  while (op.status === 'running') {
    update(`正在处理 · ${op.id.slice(0, 8)}（扫描和 CUDA 验证可能需要数分钟）`);
    await new Promise((resolve) => setTimeout(resolve, 1200));
    if (signal?.aborted) return;
    op = (await settingsHTTP.get<Operation>(`/settings/operations/${op.id}`, { signal })).data;
  }
  if (op.status !== 'succeeded') throw new Error(op.error || '操作失败');
  update('操作完成');
  return op.result;
}
