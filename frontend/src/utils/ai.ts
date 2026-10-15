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
  aoi?: GeoJSON.Polygon;
  task_id?: string;
  // Friendly metadata helps the model and UI explain opaque IDs without making
  // these values part of the command contract.
  region_name?: string;
  phases?: Array<{ id: string; month: string; scene_ids: string[] }>;
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
): Promise<void> {
  const response = await fetch(
    `/api/ai/workflows/${encodeURIComponent(workflowId)}/events?after=${after}`,
    { headers: { Accept: 'text/event-stream' }, signal },
  );
  if (!response.ok) {
    const payload = await readResponseJson(response);
    throw new Error(errorMessage(payload, `工作流订阅失败（${response.status}）`));
  }
  if (!response.body) return;
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '',
    eventName = '',
    eventId: string | undefined,
    dataLines: string[] = [];
  const dispatch = () => {
    const event = eventFromParts(eventName, dataLines.join('\n'), eventId);
    if (event) onEvent(event);
    eventName = '';
    eventId = undefined;
    dataLines = [];
  };
  const consume = (chunk: string) => {
    buffer += chunk;
    const lines = buffer.split(/\r?\n/);
    buffer = lines.pop() || '';
    for (const line of lines) {
      if (!line) {
        dispatch();
        continue;
      }
      if (line.startsWith(':')) continue;
      const separator = line.indexOf(':');
      const field = separator < 0 ? line : line.slice(0, separator);
      const value = separator < 0 ? '' : line.slice(separator + 1).replace(/^ /, '');
      if (field === 'event') eventName = value;
      else if (field === 'id') eventId = value;
      else if (field === 'data') dataLines.push(value);
    }
  };
  try {
    while (true) {
      const result = await reader.read();
      if (result.done) break;
      consume(decoder.decode(result.value, { stream: true }));
    }
    consume(decoder.decode());
    if (dataLines.length) dispatch();
  } finally {
    reader.releaseLock();
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
    record && (record.data ?? record.payload ?? parsed) !== undefined
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
): Promise<void> {
  const response = await fetch(`/api/ai/sessions/${encodeURIComponent(sessionId)}/messages`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream, application/json' },
    body: JSON.stringify(body),
    signal,
  });
  if (!response.ok) {
    const payload = await readResponseJson(response);
    throw new Error(errorMessage(payload, `AI 请求失败（${response.status}）`));
  }
  if (!response.body) {
    const payload = await readResponseJson(response);
    const event = eventFromParts('', JSON.stringify(payload));
    if (event) onEvent(event);
    return;
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  let eventName = '';
  let eventId: string | undefined;
  let dataLines: string[] = [];
  const dispatch = () => {
    const event = eventFromParts(eventName, dataLines.join('\n'), eventId);
    if (event) onEvent(event);
    eventName = '';
    eventId = undefined;
    dataLines = [];
  };
  const consume = (chunk: string) => {
    buffer += chunk;
    const lines = buffer.split(/\r?\n/);
    buffer = lines.pop() || '';
    for (const line of lines) {
      if (!line) {
        dispatch();
        continue;
      }
      if (line.startsWith(':')) continue;
      const separator = line.indexOf(':');
      const field = separator < 0 ? line : line.slice(0, separator);
      const value = separator < 0 ? '' : line.slice(separator + 1).replace(/^ /, '');
      if (field === 'event') eventName = value;
      else if (field === 'id') eventId = value;
      else if (field === 'data') dataLines.push(value);
    }
  };
  try {
    while (true) {
      const result = await reader.read();
      if (result.done) break;
      consume(decoder.decode(result.value, { stream: true }));
    }
    consume(decoder.decode());
    if (dataLines.length) dispatch();
  } finally {
    reader.releaseLock();
  }
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
