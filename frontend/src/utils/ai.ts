export type JsonPrimitive = string | number | boolean | null;
export type JsonValue = JsonPrimitive | JsonObject | JsonValue[];
export interface JsonObject {
  [key: string]: JsonValue | undefined;
}

export interface AiWorkspaceContext {
  revision: string;
  page: string;
  region_id?: string;
  phase_ids: string[];
  bounds?: [number, number, number, number];
  aoi?: GeoJSON.Polygon | GeoJSON.MultiPolygon;
  admin_code?: string;
  admin_version?: string;
  task_id?: string;
  // Friendly metadata helps the model and UI explain opaque IDs without making
  // these values part of the command contract.
  region_name?: string;
  detection?: { model_id: string; config_revision: number; settings: Record<string, number> };
  phases?: Array<{ id: string; month: string; scene_ids: string[] }>;
  available_phases?: Array<{ id: string; month: string; scene_ids: string[] }>;
  catalog_status?: 'loading' | 'error' | 'ready';
  selection_error?: string;
  active_phase?: string;
  task?: { status: string; progress: number; message?: string };
}

export interface AiSession {
  id: string;
}

export interface AiToolError {
  code: string;
  message: string;
  retryable: boolean;
  field?: string;
  recoverable_actions?: string[];
}

export interface AiSessionSnapshot extends AiSession {
  closed_at?: string;
  messages?: unknown[];
  workspace?: AiWorkspaceContext;
  workflow_ids?: string[];
  actions?: unknown[] | Record<string, unknown>;
}

export interface AiWorkflowEvent {
  id?: number;
  type: string;
  data?: unknown;
  created_at?: string;
}

export interface AiWorkflowSnapshot {
  id: string;
  status?: string;
  request_id?: string;
  events?: AiWorkflowEvent[];
}

export interface AiTaskSnapshot {
  id: string;
  before_id: string;
  after_id: string;
  algorithm: string;
  status: string;
  progress?: number;
  stage?: string;
  error_msg?: string;
}

export interface AiActionAck {
  status: 'received' | 'applied' | 'rejected' | 'failed';
  context_revision?: string;
  workspace?: AiWorkspaceContext;
  error?: AiToolError;
  result?: { task_id: string; pair_count: number };
}

// Result exports are intentionally treated as opaque server references. The
// assistant may render only the same-origin routes served by the task API and
// the filenames that the backend's result handler actually accepts.
const AI_RESULT_FILES = new Set(['change.geojson', 'stats.csv', 'change.zip', 'mask.tif']);
const AI_OPAQUE_ID = '[A-Za-z0-9_-]+';

export function safeAiDownloadRef(value: unknown): string | undefined {
  const record = asRecord(value);
  const candidate =
    typeof value === 'string'
      ? value
      : record && typeof record.download_ref === 'string'
        ? record.download_ref
        : undefined;
  if (!candidate || candidate.includes('%') || candidate.includes('\\')) return undefined;
  const match = new RegExp(`^/api/tasks/(${AI_OPAQUE_ID})/result/([^/?#]+)$`).exec(candidate);
  if (!match || !AI_RESULT_FILES.has(match[2])) return undefined;
  return candidate;
}

export interface AiResourceReferences {
  resources: Array<{ kind: 'task_id' | 'result_id' | 'workflow_id' | 'operation_id'; id: string }>;
  downloads: string[];
}

/** Display only opaque, documented references, never provider paths or arbitrary JSON. */
export function safeAiResourceReferences(values: unknown[]): AiResourceReferences {
  const resources = new Map<string, AiResourceReferences['resources'][number]>();
  const downloads = new Set<string>();
  const opaqueId = /^[A-Za-z0-9_-]{1,128}$/;
  const collect = (value: unknown, depth = 0): void => {
    if (depth > 3) return;
    if (typeof value === 'string') {
      const download = safeAiDownloadRef(value);
      if (download) downloads.add(download);
      return;
    }
    if (Array.isArray(value)) {
      value.forEach((item) => collect(item, depth + 1));
      return;
    }
    const record = asRecord(value);
    if (!record) return;
    for (const kind of ['task_id', 'result_id', 'workflow_id', 'operation_id'] as const) {
      const id = record[kind];
      if (typeof id === 'string' && opaqueId.test(id)) resources.set(`${kind}:${id}`, { kind, id });
    }
    if (Array.isArray(record.task_ids)) {
      for (const id of record.task_ids) {
        if (typeof id === 'string' && opaqueId.test(id))
          resources.set(`task_id:${id}`, { kind: 'task_id', id });
      }
    }
    const download = safeAiDownloadRef(record);
    if (download) downloads.add(download);
    if (record.data) collect(record.data, depth + 1);
    if (record.refs) collect(record.refs, depth + 1);
  };
  values.forEach((value) => collect(value));
  return { resources: [...resources.values()], downloads: [...downloads] };
}

export function makeAiToolError(message: string, code = 'UI_ACTION_REJECTED'): AiToolError {
  return { code, message, retryable: false };
}

export interface AiUiCommand {
  action_id: string;
  action: string;
  args?: JsonObject;
  expected_revision?: string;
}

export interface AiServerEvent {
  type: string;
  data: unknown;
  id?: string;
}

function asRecord(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

function errorMessage(value: unknown, fallback: string): string {
  const record = asRecord(value);
  if (record) {
    for (const key of ['error', 'detail', 'message']) {
      const message = record[key];
      if (typeof message === 'string' && message.length) return message;
    }
  }
  return fallback;
}

async function readResponseJson(response: Response): Promise<unknown> {
  const text = await response.text();
  if (!text) return null;
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return text;
  }
}

export async function createAiSession(signal?: AbortSignal): Promise<AiSession> {
  const response = await fetch('/api/ai/sessions', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify({}),
    signal,
  });
  const payload = await readResponseJson(response);
  if (!response.ok) throw new Error(errorMessage(payload, `AI 会话创建失败（${response.status}）`));
  const record = asRecord(payload);
  const id = record && (typeof record.id === 'string' ? record.id : record.session_id);
  if (typeof id !== 'string' || !id) throw new Error('AI 会话响应缺少会话 ID');
  return { id };
}

export async function getAiSession(
  sessionId: string,
  signal?: AbortSignal,
): Promise<AiSessionSnapshot> {
  const response = await fetch(`/api/ai/sessions/${encodeURIComponent(sessionId)}`, {
    headers: { Accept: 'application/json' },
    signal,
  });
  const payload = await readResponseJson(response);
  if (!response.ok) throw new Error(errorMessage(payload, `AI 会话读取失败（${response.status}）`));
  const record = asRecord(payload);
  if (!record) throw new Error('AI 会话响应格式无效');
  const id = typeof record.id === 'string' ? record.id : sessionId;
  return {
    id,
    closed_at: typeof record.closed_at === 'string' ? record.closed_at : undefined,
    messages: Array.isArray(record.messages) ? record.messages : undefined,
    workspace: asRecord(record.workspace) as unknown as AiWorkspaceContext | undefined,
    workflow_ids: Array.isArray(record.workflow_ids)
      ? record.workflow_ids.filter((item): item is string => typeof item === 'string')
      : undefined,
    actions:
      Array.isArray(record.actions) || asRecord(record.actions)
        ? (record.actions as unknown[] | Record<string, unknown>)
        : undefined,
  };
}

export async function closeAiSession(sessionId: string, signal?: AbortSignal): Promise<void> {
  const response = await fetch(`/api/ai/sessions/${encodeURIComponent(sessionId)}/close`, {
    method: 'POST',
    headers: { Accept: 'application/json' },
    signal,
  });
  // A missing session has already ceased to own any resumable work.
  if (response.status === 404) return;
  if (!response.ok) {
    const payload = await readResponseJson(response);
    throw new Error(errorMessage(payload, `结束 AI 对话失败（${response.status}）`));
  }
}

async function getAiJson<T>(path: string, fallback: string, signal?: AbortSignal): Promise<T> {
  const response = await fetch(path, { headers: { Accept: 'application/json' }, signal });
  const payload = await readResponseJson(response);
  if (!response.ok) throw new Error(errorMessage(payload, `${fallback}（${response.status}）`));
  return payload as T;
}

export function getAiTask(taskId: string, signal?: AbortSignal): Promise<AiTaskSnapshot> {
  return getAiJson<AiTaskSnapshot>(
    `/api/tasks/${encodeURIComponent(taskId)}`,
    '任务读取失败',
    signal,
  );
}

export function getAiImage(
  imageId: string,
  signal?: AbortSignal,
): Promise<import('./api').ImageInfo> {
  return getAiJson<import('./api').ImageInfo>(
    `/api/images/${encodeURIComponent(imageId)}`,
    '影像读取失败',
    signal,
  );
}

export async function getAiWorkflowEvents(
  workflowId: string,
  after = 0,
  signal?: AbortSignal,
): Promise<AiWorkflowEvent[]> {
  const payload = await getAiJson<{ events?: unknown[] }>(
    `/api/ai/workflows/${encodeURIComponent(workflowId)}/events?after=${after}`,
    '工作流事件读取失败',
    signal,
  );
  return Array.isArray(payload.events)
    ? payload.events.filter((event): event is AiWorkflowEvent => isWorkflowEvent(event))
    : [];
}

export function getAiWorkflow(
  workflowId: string,
  signal?: AbortSignal,
): Promise<AiWorkflowSnapshot> {
  return getAiJson<AiWorkflowSnapshot>(
    `/api/ai/workflows/${encodeURIComponent(workflowId)}`,
    '工作流读取失败',
    signal,
  );
}

export async function streamAiWorkflowEvents(
  workflowId: string,
  after: number,
  onEvent: (event: AiServerEvent) => void,
  signal?: AbortSignal,
  options: AiStreamOptions = {},
): Promise<void> {
  await streamWithReconnect(
    (cursor) =>
      fetch(`/api/ai/workflows/${encodeURIComponent(workflowId)}/events?after=${cursor || after}`, {
        headers: { Accept: 'text/event-stream', ...(cursor ? { 'Last-Event-ID': cursor } : {}) },
        signal,
      }),
    onEvent,
    signal,
    { ...options, after: options.after || String(after), workflowId },
  );
}

export interface AiStreamOptions {
  after?: string;
  workflowId?: string;
  onReconnect?: (attempt: number, maxAttempts: number, delayMs: number) => void;
}

export class AiStreamError extends Error {
  constructor(
    message: string,
    readonly retryable: boolean,
  ) {
    super(message);
    this.name = 'AiStreamError';
  }
}

/** Every delay belongs to the current session signal, including ACK backoff. */
export function waitForAiRetry(delayMs: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const abort = () => {
      clearTimeout(timer);
      signal?.removeEventListener('abort', abort);
      reject(new DOMException('Aborted', 'AbortError'));
    };
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', abort);
      resolve();
    }, delayMs);
    if (signal?.aborted) abort();
    else signal?.addEventListener('abort', abort, { once: true });
  });
}

async function consumeAiEvents(
  response: Response,
  onEvent: (event: AiServerEvent) => void,
): Promise<void> {
  if (!response.body) throw new AiStreamError('AI 连接没有返回事件流', true);
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  let eventName = '';
  let eventId: string | undefined;
  let dataLines: string[] = [];
  let terminal = false;
  const dispatch = () => {
    const event = eventFromParts(eventName, dataLines.join('\n'), eventId);
    if (event && !terminal) {
      onEvent(event);
      terminal = ['final', 'error', 'cancelled'].includes(event.type);
    }
    eventName = '';
    eventId = undefined;
    dataLines = [];
  };
  const consume = (chunk: string) => {
    buffer += chunk;
    const lines = buffer.split(/\r?\n/);
    buffer = lines.pop() || '';
    for (const line of lines) {
      if (!line) dispatch();
      else if (!line.startsWith(':')) {
        const separator = line.indexOf(':');
        const field = separator < 0 ? line : line.slice(0, separator);
        const value = separator < 0 ? '' : line.slice(separator + 1).replace(/^ /, '');
        if (field === 'event') eventName = value;
        else if (field === 'id') eventId = value;
        else if (field === 'data') dataLines.push(value);
      }
    }
  };
  try {
    while (!terminal) {
      const result = await reader.read();
      if (result.done) break;
      consume(decoder.decode(result.value, { stream: true }));
    }
    consume(decoder.decode() + '\n');
    if (dataLines.length) dispatch();
  } finally {
    if (terminal) await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

async function streamWithReconnect(
  connect: (cursor: string | undefined) => Promise<Response>,
  onEvent: (event: AiServerEvent) => void,
  signal: AbortSignal | undefined,
  options: AiStreamOptions,
): Promise<void> {
  const delays = [500, 1_000, 2_000];
  let cursor = options.after;
  let workflowId = options.workflowId || '';
  const seen = new Set<string>();
  for (let attempt = 0; ; attempt += 1) {
    let terminal = false;
    try {
      if (signal?.aborted) throw new DOMException('Aborted', 'AbortError');
      const response = await connect(cursor);
      if (!response.ok) {
        const payload = await readResponseJson(response);
        throw new AiStreamError(
          errorMessage(payload, `AI 连接失败（${response.status}）`),
          [408, 429].includes(response.status) || response.status >= 500,
        );
      }
      await consumeAiEvents(response, (event) => {
        if (signal?.aborted) return;
        if (event.type === 'workflow_started') {
          const data = asRecord(event.data);
          if (typeof data?.workflow_id === 'string') workflowId = data.workflow_id;
        }
        if (['final', 'error', 'cancelled'].includes(event.type)) terminal = true;
        if (event.id) {
          const currentId = Number(cursor || 0);
          const nextId = Number(event.id);
          if (!Number.isFinite(currentId) || !Number.isFinite(nextId) || nextId >= currentId)
            cursor = event.id;
          const key = `${workflowId}:${event.id}`;
          if (seen.has(key)) return;
          seen.add(key);
        }
        onEvent(event);
      });
      if (terminal) return;
      throw new AiStreamError('AI 连接已中断，尚未收到任务终态', true);
    } catch (cause: unknown) {
      if (signal?.aborted) throw cause;
      if (terminal) return;
      const retryable = cause instanceof AiStreamError ? cause.retryable : true;
      if (!retryable || attempt >= delays.length) {
        if (cause instanceof AiStreamError) throw cause;
        throw new AiStreamError(cause instanceof Error ? cause.message : 'AI 连接中断', retryable);
      }
      options.onReconnect?.(attempt + 1, delays.length, delays[attempt]);
      await waitForAiRetry(delays[attempt], signal);
    }
  }
}

function isWorkflowEvent(value: unknown): value is AiWorkflowEvent {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return false;
  const record = value as Record<string, unknown>;
  return typeof record.type === 'string';
}

function eventFromParts(eventName: string, data: string, id?: string): AiServerEvent | null {
  if (!data) return null;
  let parsed: unknown = data;
  try {
    parsed = JSON.parse(data) as unknown;
  } catch {
    /* text_delta may be plain text */
  }
  const record = asRecord(parsed);
  const typeValue = record && (record.type ?? record.event);
  const type = typeof typeValue === 'string' && typeValue ? typeValue : eventName || 'message';
  const eventData =
    record &&
    typeof typeValue === 'string' &&
    (record.data ?? record.payload ?? parsed) !== undefined
      ? (record.data ?? record.payload ?? parsed)
      : parsed;
  return { type, data: eventData, id };
}

/** Streams a server-sent event response and normalizes both named and typed events. */
export async function streamAiMessage(
  sessionId: string,
  body: { text: string; request_id: string; workspace: AiWorkspaceContext; skill_id?: string },
  onEvent: (event: AiServerEvent) => void,
  signal?: AbortSignal,
  options: AiStreamOptions = {},
): Promise<void> {
  // Freeze the original body: reconnecting must not silently use a newer workspace.
  const encoded = JSON.stringify(body);
  await streamWithReconnect(
    (cursor) =>
      fetch(`/api/ai/sessions/${encodeURIComponent(sessionId)}/messages`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          Accept: 'text/event-stream, application/json',
          ...(cursor ? { 'Last-Event-ID': cursor } : {}),
        },
        body: encoded,
        signal,
      }),
    onEvent,
    signal,
    options,
  );
}

export async function acknowledgeAiAction(
  sessionId: string,
  actionId: string,
  ack: AiActionAck,
  signal?: AbortSignal,
): Promise<void> {
  const response = await fetch(
    `/api/ai/sessions/${encodeURIComponent(sessionId)}/actions/${encodeURIComponent(actionId)}`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify(ack),
      signal,
    },
  );
  if (!response.ok) {
    const payload = await readResponseJson(response);
    throw new Error(errorMessage(payload, `AI 操作回执失败（${response.status}）`));
  }
}

export function isAiUiCommand(value: unknown): value is AiUiCommand {
  const record = asRecord(value);
  return !!record && typeof record.action_id === 'string' && typeof record.action === 'string';
}

export function asEventRecord(value: unknown): Record<string, unknown> | null {
  return asRecord(value);
}
