import {
  computed,
  nextTick,
  onMounted,
  onUnmounted,
  ref,
  watch,
  type ComputedRef,
  type Ref,
} from 'vue';
import { useRoute, useRouter } from 'vue-router';
import { useWorkspace } from './useWorkspace';
import {
  acknowledgeAiAction,
  asEventRecord,
  createAiSession,
  getAiImage,
  getAiSession,
  getAiTask,
  getAiWorkflow,
  getAiWorkflowEvents,
  streamAiMessage,
  streamAiWorkflowEvents,
  type AiActionAck,
  type AiServerEvent,
  type AiSessionSnapshot,
  type AiWorkflowEvent,
  type AiUiCommand,
  type AiWorkspaceContext,
  isAiUiCommand,
  makeAiToolError,
} from '../utils/ai';
import { getAiMapAdapter } from './useWorkspace';
import { getRegionData, type RegionCollection } from '../utils/regions';
import { polygonBounds, polygonError } from '../utils/selectionGeometry';
import type { TimelinePair } from '../utils/timeline';
import { pairBounds, pairColor } from '../utils/timeline';

export type AiEntryKind =
  | 'user'
  | 'assistant'
  | 'plan'
  | 'tool'
  | 'progress'
  | 'error'
  | 'final'
  | 'system';

export interface AiTimelineEntry {
  id: string;
  kind: AiEntryKind;
  text: string;
  timestamp: number;
  detail?: unknown;
}

export interface AiAssistant {
  configured: Ref<boolean>;
  open: Ref<boolean>;
  input: Ref<string>;
  sending: Ref<boolean>;
  error: Ref<string>;
  messages: Ref<AiTimelineEntry[]>;
  workspace: ComputedRef<AiWorkspaceContext>;
  toggle: () => void;
  close: () => void;
  send: () => Promise<void>;
  handleUiCommand: (command: AiUiCommand) => Promise<void>;
}

interface StoredMessage {
  role?: unknown;
  type?: unknown;
  kind?: unknown;
  text?: unknown;
  content?: unknown;
  message?: unknown;
  detail?: unknown;
  timestamp?: unknown;
}

interface AiStatusPayload {
  configured?: unknown;
}

const SESSION_STORAGE_KEY = 'change-detection.ai.session';
const CONTEXT_REVISION_KEY = 'change-detection.ai.context-revision';

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function stringValue(value: unknown): string | undefined {
  return typeof value === 'string' && value.length > 0 ? value : undefined;
}

function actionText(value: unknown, fallback: string): string {
  if (typeof value === 'string') return value;
  if (isRecord(value)) {
    const message =
      stringValue(value.message) || stringValue(value.detail) || stringValue(value.name);
    if (message) return message;
  }
  return fallback;
}

function parseBounds(value: unknown): [number, number, number, number] | undefined {
  if (
    !Array.isArray(value) ||
    value.length !== 4 ||
    value.some((item) => typeof item !== 'number' || !Number.isFinite(item))
  )
    return undefined;
  return [value[0] as number, value[1] as number, value[2] as number, value[3] as number];
}

function parsePolygon(value: unknown): GeoJSON.Polygon | undefined {
  if (!isRecord(value) || value.type !== 'Polygon' || !Array.isArray(value.coordinates))
    return undefined;
  const coordinates = value.coordinates as unknown[];
  if (
    coordinates.length !== 1 ||
    coordinates.some((ring) => !Array.isArray(ring) || ring.length < 4)
  )
    return undefined;
  if (
    coordinates.some((ring) => {
      if (!Array.isArray(ring)) return true;
      const first = ring[0];
      const last = ring[ring.length - 1];
      return (
        !Array.isArray(first) ||
        !Array.isArray(last) ||
        first.length < 2 ||
        last.length < 2 ||
        first[0] !== last[0] ||
        first[1] !== last[1]
      );
    })
  )
    return undefined;
  return value as unknown as GeoJSON.Polygon;
}

function taskStatus(value: string): TimelinePair['status'] {
  if (value === 'succeeded' || value === 'completed') return 'succeeded';
  if (value === 'failed') return 'failed';
  if (value === 'running') return 'running';
  if (value === 'pending') return 'pending';
  return 'waiting';
}

function sessionIdFromStorage(): string | undefined {
  try {
    const value = sessionStorage.getItem(SESSION_STORAGE_KEY);
    return value && value.length ? value : undefined;
  } catch {
    return undefined;
  }
}

function storeSessionId(value: string): void {
  try {
    sessionStorage.setItem(SESSION_STORAGE_KEY, value);
  } catch {
    /* private browsing */
  }
}

function initialRevision(): string {
  try {
    return sessionStorage.getItem(CONTEXT_REVISION_KEY) || 'workspace-0';
  } catch {
    return 'workspace-0';
  }
}

function storeRevision(value: string): void {
  try {
    sessionStorage.setItem(CONTEXT_REVISION_KEY, value);
  } catch {
    /* private browsing */
  }
}

function parseStoredMessage(value: unknown, index: number): AiTimelineEntry | undefined {
  if (!isRecord(value)) return undefined;
  const source = value as StoredMessage;
  const text =
    stringValue(source.text) || stringValue(source.content) || stringValue(source.message);
  if (!text) return undefined;
  const role = stringValue(source.role);
  const kind = stringValue(source.kind) || stringValue(source.type);
  const entryKind: AiEntryKind =
    role === 'user'
      ? 'user'
      : kind === 'error'
        ? 'error'
        : role === 'assistant'
          ? 'assistant'
          : 'system';
  return {
    id: `restored-${index}`,
    kind: entryKind,
    text,
    detail: source.detail,
    timestamp: typeof source.timestamp === 'number' ? source.timestamp : Date.now() - index * 1000,
  };
}

async function aiConfigured(): Promise<boolean> {
  const response = await fetch('/api/ai/status', { headers: { Accept: 'application/json' } });
  if (!response.ok) throw new Error(`AI status request failed (${response.status})`);
  const payload = (await response.json()) as AiStatusPayload;
  return payload.configured === true;
}

const AI_STATUS_RECHECK_MS = 8_000;
const AI_ACK_RETRY_DELAYS_MS = [250, 500, 1_000, 2_000];

function newId(prefix: string): string {
  return `${prefix}-${crypto.randomUUID()}`;
}

export function useAiAssistant(): AiAssistant {
  const route = useRoute();
  const router = useRouter();
  const workspaceStore = useWorkspace();
  const configured = ref(false);
  const open = ref(false);
  const input = ref('');
  const sending = ref(false);
  const error = ref('');
  const messages = ref<AiTimelineEntry[]>([]);
  const sessionId = ref<string>();
  const contextRevision = ref(initialRevision());
  let activeStream: AbortController | null = null;
  const workflowStreams = new Set<AbortController>();
  const handledActionIds = new Set<string>();
  const acknowledgedActionIds = new Set<string>();
  const pendingAcks = new Map<string, { ack: AiActionAck; command: AiUiCommand }>();
  const ackInFlight = new Set<string>();
  const ackRetryTimers = new Map<string, ReturnType<typeof setTimeout>>();
  let restoringWorkspace = false;
  let lastConfigCheck = 0;
  let configCheck: Promise<void> | null = null;
  let restored = false;

  const workspace = computed<AiWorkspaceContext>(() => {
    const selected = workspaceStore.selectedRegion.value;
    const area = workspaceStore.selectedArea.value;
    const task = workspaceStore.currentTask.value;
    return {
      revision: contextRevision.value,
      page: route.path,
      region_id: selected?.id,
      region_name: selected?.properties.name,
      phase_ids: workspaceStore.loadedPhaseIds.value.slice(),
      phases: workspaceStore.imageryPhases.value.map((phase) => ({
        id: phase.id,
        month: phase.month,
        scene_ids: phase.scenes.map((scene) => scene.id),
      })),
      active_phase: workspaceStore.activeMonth.value || undefined,
      bounds: area ? [area.west, area.south, area.east, area.north] : undefined,
      aoi: workspaceStore.selectedGeometry.value || undefined,
      task_id: workspaceStore.currentTaskId.value || undefined,
      task: task
        ? {
            status: task.status,
            progress: task.progress || 0,
            message: workspaceStore.batchMessage.value || undefined,
          }
        : undefined,
    };
  });

  function bumpRevision(): string {
    const next = `${contextRevision.value.split('-')[0]}-${Date.now().toString(36)}`;
    contextRevision.value = next;
    storeRevision(next);
    return next;
  }

  const workspaceFingerprint = computed(() =>
    JSON.stringify({
      page: workspace.value.page,
      region_id: workspace.value.region_id,
      phase_ids: workspace.value.phase_ids,
      bounds: workspace.value.bounds,
      aoi: workspace.value.aoi,
      task_id: workspace.value.task_id,
      active_phase: workspace.value.active_phase,
    }),
  );
  watch(workspaceFingerprint, (next, previous) => {
    if (!restoringWorkspace && previous !== undefined && next !== previous) bumpRevision();
  });

  function append(kind: AiEntryKind, text: string, detail?: unknown): AiTimelineEntry {
    const entry: AiTimelineEntry = { id: newId('ai'), kind, text, detail, timestamp: Date.now() };
    messages.value.push(entry);
    return entry;
  }

  function updateRevisionFromSnapshot(snapshot: AiSessionSnapshot): void {
    const revision = snapshot.workspace?.revision;
    if (typeof revision === 'string' && revision) {
      contextRevision.value = revision;
      storeRevision(revision);
    }
  }

  function workspaceHasContext(): boolean {
    return Boolean(
      workspaceStore.selectedRegion.value ||
        workspaceStore.selectedArea.value ||
        workspaceStore.selectedGeometry.value ||
        workspaceStore.loadedPhaseIds.value.length ||
        workspaceStore.currentTaskId.value,
    );
  }

  function snapshotPhaseIds(snapshot: AiWorkspaceContext): string[] {
    if (Array.isArray(snapshot.phase_ids) && snapshot.phase_ids.length)
      return snapshot.phase_ids.filter((id) => typeof id === 'string');
    return Array.isArray(snapshot.phases)
      ? snapshot.phases
          .filter((phase) => phase && typeof phase.id === 'string')
          .map((phase) => phase.id)
      : [];
  }

  async function refreshConfigured(force = false): Promise<void> {
    const now = Date.now();
    if (configCheck) return configCheck;
    if (!force && now - lastConfigCheck < AI_STATUS_RECHECK_MS) return;
    lastConfigCheck = now;
    configCheck = aiConfigured()
      .then((value) => {
        configured.value = value;
      })
      .catch(() => {
        configured.value = false;
        // A transient status failure should be recoverable on the next window
        // focus without forcing a full page reload. A successful `configured: false`
        // response remains throttled by the normal interval above.
        lastConfigCheck = 0;
      })
      .finally(() => {
        configCheck = null;
      });
    await configCheck;
  }

  async function restoreStoredSession(): Promise<void> {
    if (!configured.value || restored) return;
    restored = true;
    const previous = sessionIdFromStorage();
    if (!previous) return;
    try {
      await restoreSession(previous);
    } catch {
      try {
        sessionStorage.removeItem(SESSION_STORAGE_KEY);
      } catch {
        /* private browsing */
      }
    }
  }

  async function restoreWorkspace(snapshot: AiSessionSnapshot): Promise<void> {
    const target = snapshot.workspace;
    if (!target || workspaceHasContext()) return;
    restoringWorkspace = true;
    const initialFingerprint = workspaceFingerprint.value;
    try {
      if (target.region_id) {
        const collection = await getRegionData<RegionCollection>('/py/local/regions');
        // A user can create an AOI or select a region while the directory request
        // is in flight. Preserve that newer context instead of applying the old
        // session snapshot over it.
        if (workspaceFingerprint.value !== initialFingerprint || workspaceHasContext()) return;
        const feature = collection.features.find(
          (item) => item.id === target.region_id || item.properties.region_id === target.region_id,
        );
        if (!feature) {
          append('error', '会话中的行政区已不在当前目录中');
          return;
        }
        await workspaceStore.selectWorkspaceRegion(feature);
        if (workspaceStore.selectedRegion.value?.id !== feature.id) return;
        // The catalog request yields to user input. An AOI created in the
        // meantime belongs to the current workspace and must not be replaced.
        if (workspaceStore.selectedArea.value || workspaceStore.selectedGeometry.value) return;
        const catalogPhaseIds = workspaceStore.imageryPhases.value.map((phase) => phase.id);
        if (
          workspaceStore.loadedPhaseIds.value.some((id) => !catalogPhaseIds.includes(id)) ||
          workspaceStore.loadedPhaseIds.value.length !== catalogPhaseIds.length ||
          (catalogPhaseIds.length > 0 &&
            workspaceStore.activeMonth.value !== workspaceStore.imageryPhases.value[0]?.month)
        )
          return;
      }

      const phaseIds = snapshotPhaseIds(target);
      for (const id of workspaceStore.loadedPhaseIds.value.slice()) {
        if (!phaseIds.includes(id)) workspaceStore.removePhase(id);
      }
      for (const id of phaseIds) {
        if (workspaceStore.catalogPhases.value.some((phase) => phase.id === id && phase.available))
          workspaceStore.addPhase(id);
      }

      const bounds = parseBounds(target.bounds);
      const aoi = parsePolygon(target.aoi);
      if (bounds && aoi) {
        const regionGeometry = workspaceStore.selectedRegion.value?.geometry;
        if (
          !regionGeometry ||
          !polygonError(aoi.coordinates[0].slice(0, -1) as [number, number][], regionGeometry)
        )
          workspaceStore.setSelectedArea(
            { west: bounds[0], south: bounds[1], east: bounds[2], north: bounds[3] },
            aoi,
          );
      }
      if (
        typeof target.active_phase === 'string' &&
        workspaceStore.imageryPhases.value.some((phase) => phase.month === target.active_phase)
      ) {
        workspaceStore.activeMonth.value = target.active_phase;
      }
      // Keep the snapshot revision stable while the watcher observes the restored refs.
      await nextTick();
    } finally {
      await nextTick();
      restoringWorkspace = false;
    }
  }

  async function restoreSession(id: string): Promise<void> {
    const snapshot = await getAiSession(id);
    sessionId.value = snapshot.id;
    storeSessionId(snapshot.id);
    updateRevisionFromSnapshot(snapshot);
    await restoreWorkspace(snapshot);
    if (Array.isArray(snapshot.messages)) {
      messages.value = snapshot.messages
        .map(parseStoredMessage)
        .filter((item): item is AiTimelineEntry => !!item);
    }
    const actionStates = new Map<string, { status: string; command?: AiUiCommand }>();
    const storedActions = Array.isArray(snapshot.actions)
      ? snapshot.actions
      : isRecord(snapshot.actions)
        ? Object.entries(snapshot.actions).map(([id, action]) => ({ id, action }))
        : [];
    for (const stored of storedActions) {
      const wrapped = isRecord(stored) && isRecord(stored.action) ? stored : undefined;
      const action = wrapped ? wrapped.action : stored;
      if (!isRecord(action)) continue;
      const actionId = stringValue(action.id) || (wrapped ? stringValue(wrapped.id) : undefined);
      const status = stringValue(action.status)?.toLowerCase();
      if (!actionId || !status) continue;
      actionStates.set(actionId, {
        status,
        command: isAiUiCommand(action.command) ? action.command : undefined,
      });
      // Seed durable terminal states before subscribing: the matching event
      // might be missing from the fetched history but arrive on the live stream.
      if (status !== 'pending') {
        handledActionIds.add(actionId);
        const name = isAiUiCommand(action.command) ? action.command.action : actionId;
        if (status === 'applied') append('system', `已恢复界面操作：${name}`);
        else if (status === 'rejected') append('error', `恢复的界面操作已被拒绝：${name}`);
        else append('system', `已恢复界面操作终态（${status}），未重复执行：${name}`);
      }
    }
    for (const workflowId of snapshot.workflow_ids || []) {
      try {
        const workflow = await getAiWorkflow(workflowId);
        const events = await getAiWorkflowEvents(workflowId);
        await replayWorkflowEvents(events, actionStates);
        if (workflow.status === 'running' || workflow.status === 'queued') {
          const controller = new AbortController();
          workflowStreams.add(controller);
          const after = events.reduce((max, event) => Math.max(max, event.id || 0), 0);
          void streamAiWorkflowEvents(
            workflowId,
            after,
            (event) => {
              onStreamEvent(event);
            },
            controller.signal,
          )
            .catch((cause: unknown) => {
              if (!controller.signal.aborted)
                append('error', cause instanceof Error ? cause.message : '工作流订阅失败');
            })
            .finally(() => workflowStreams.delete(controller));
        }
      } catch (cause: unknown) {
        append('error', cause instanceof Error ? cause.message : '工作流恢复失败');
      }
    }
    // A pending action may have been persisted before its workflow event. Recover
    // it from the durable session snapshot after replaying all known events.
    for (const [actionId, action] of actionStates) {
      if (action.status !== 'pending' || !action.command || handledActionIds.has(actionId))
        continue;
      await handleUiCommand(action.command);
    }
  }

  async function replayWorkflowEvents(
    events: AiWorkflowEvent[],
    actionStates: Map<string, { status: string; command?: AiUiCommand }>,
  ): Promise<void> {
    for (const event of events) {
      if (event.type === 'message' || event.type === 'text_delta') continue;
      const payload = event.data;
      if (event.type === 'ui_command') {
        const command = isAiUiCommand(payload) ? payload : undefined;
        if (!command) {
          append('error', '恢复的界面操作格式无效', payload);
          continue;
        }
        const state = actionStates.get(command.action_id);
        if (handledActionIds.has(command.action_id)) continue;
        if (state?.status === 'applied') {
          handledActionIds.add(command.action_id);
          append('system', `已恢复界面操作：${command.action}`);
        } else if (state?.status === 'rejected') {
          handledActionIds.add(command.action_id);
          append('error', `恢复的界面操作已被拒绝：${command.action}`);
        } else if (state?.status === 'pending') {
          await handleUiCommand(state.command || command);
        } else if (
          state?.status === 'failed' ||
          state?.status === 'timeout' ||
          state?.status === 'cancelled'
        ) {
          handledActionIds.add(command.action_id);
          append('system', `已恢复界面操作终态（${state.status}），未重复执行：${command.action}`);
        } else {
          handledActionIds.add(command.action_id);
          append('progress', '界面操作状态未知，未自动重复执行', payload);
        }
        continue;
      }
      onStreamEvent({
        type: event.type,
        data: payload,
        id: event.id == null ? undefined : String(event.id),
      });
    }
  }

  async function ensureSession(): Promise<string> {
    if (sessionId.value) return sessionId.value;
    const created = await createAiSession();
    sessionId.value = created.id;
    storeSessionId(created.id);
    return created.id;
  }

  async function acknowledgeWithRetry(
    command: AiUiCommand,
    acknowledgement: AiActionAck,
  ): Promise<void> {
    const actionId = command.action_id;
    if (acknowledgedActionIds.has(actionId) || ackInFlight.has(actionId)) return;
    ackInFlight.add(actionId);
    pendingAcks.set(actionId, { command, ack: acknowledgement });
    try {
      const id = sessionId.value;
      if (!id) return;
      let lastError: unknown;
      for (let attempt = 0; attempt <= AI_ACK_RETRY_DELAYS_MS.length; attempt += 1) {
        if (attempt > 0) {
          await new Promise((resolve) => setTimeout(resolve, AI_ACK_RETRY_DELAYS_MS[attempt - 1]));
        }
        try {
          await acknowledgeAiAction(id, actionId, acknowledgement);
          acknowledgedActionIds.add(actionId);
          pendingAcks.delete(actionId);
          return;
        } catch (cause: unknown) {
          lastError = cause;
        }
      }
      if (!ackRetryTimers.has(actionId)) {
        const timer = setTimeout(() => {
          ackRetryTimers.delete(actionId);
          void acknowledgeWithRetry(command, acknowledgement);
        }, 5_000);
        ackRetryTimers.set(actionId, timer);
      }
      append(
        'error',
        lastError instanceof Error ? lastError.message : '界面操作回执失败，正在重试',
      );
    } finally {
      ackInFlight.delete(actionId);
    }
  }

  function close(): void {
    open.value = false;
  }
  function toggle(): void {
    open.value = !open.value;
  }

  function eventData(event: AiServerEvent): unknown {
    const record = asEventRecord(event.data);
    return record && record.data !== undefined ? record.data : event.data;
  }

  function onStreamEvent(event: AiServerEvent): void {
    const payload = eventData(event);
    const record = asEventRecord(payload);
    switch (event.type) {
      case 'text_delta': {
        const text = record ? stringValue(record.text) : stringValue(payload);
        if (!text) return;
        const current = messages.value[messages.value.length - 1];
        if (current?.kind === 'assistant' && current.detail === 'streaming') current.text += text;
        else {
          const entry = append('assistant', text);
          entry.detail = 'streaming';
        }
        return;
      }
      case 'plan':
        append('plan', actionText(payload, '已生成执行计划'), payload);
        return;
      case 'tool_started':
        append('tool', actionText(payload, '正在调用工具…'), payload);
        return;
      case 'tool_finished':
        append('tool', actionText(payload, '工具调用完成'), payload);
        return;
      case 'tool_progress':
        append('progress', actionText(payload, '正在处理…'), payload);
        return;
      case 'ui_command':
        if (isAiUiCommand(payload)) void handleUiCommand(payload);
        else append('error', '收到格式无效的界面操作指令');
        return;
      case 'final':
        append('final', actionText(payload, '处理完成'), payload);
        sending.value = false;
        return;
      case 'error': {
        const message = record?.message || record?.error || 'AI 请求失败';
        error.value = typeof message === 'string' ? message : 'AI 请求失败';
        append('error', error.value, payload);
        sending.value = false;
        return;
      }
      default:
        if (typeof payload === 'string' && payload) append('system', payload);
    }
  }

  async function send(): Promise<void> {
    const text = input.value.trim();
    if (!text || sending.value || !configured.value) return;
    input.value = '';
    error.value = '';
    append('user', text);
    sending.value = true;
    activeStream?.abort();
    activeStream = new AbortController();
    try {
      const id = await ensureSession();
      await streamAiMessage(
        id,
        { text, request_id: newId('request'), workspace: workspace.value },
        onStreamEvent,
        activeStream.signal,
      );
      sending.value = false;
    } catch (cause: unknown) {
      if (activeStream.signal.aborted) return;
      const message = cause instanceof Error ? cause.message : 'AI 请求失败';
      error.value = message;
      append('error', message);
      sending.value = false;
    }
  }

  async function applyUiCommand(command: AiUiCommand): Promise<AiActionAck> {
    const args = command.args || {};
    if (command.expected_revision && command.expected_revision !== contextRevision.value) {
      return {
        status: 'rejected',
        context_revision: contextRevision.value,
        workspace: workspace.value,
        error: makeAiToolError('工作区已变化，请重新读取当前状态', 'UI_CONTEXT_STALE'),
      };
    }
    if (
      workspaceStore.inProgress.value &&
      ['set_existing_aoi', 'load_phase', 'load_phases', 'select_region'].includes(command.action)
    ) {
      return {
        status: 'rejected',
        context_revision: contextRevision.value,
        workspace: workspace.value,
        error: makeAiToolError('检测任务正在运行，当前工作区暂时不可修改', 'WORKSPACE_BUSY'),
      };
    }
    switch (command.action) {
      case 'set_existing_aoi': {
        const bounds = parseBounds(args.bounds);
        const aoi = parsePolygon(args.aoi);
        const source = stringValue(args.source);
        if (source && !['admin_boundary', 'user_drawn', 'workspace'].includes(source))
          return {
            status: 'rejected',
            context_revision: contextRevision.value,
            workspace: workspace.value,
            error: makeAiToolError('选区来源无效', 'AOI_SOURCE_INVALID'),
          };
        if (!bounds || !aoi)
          return {
            status: 'rejected',
            context_revision: contextRevision.value,
            workspace: workspace.value,
            error: makeAiToolError('选区必须是合法 Polygon 和 [west,south,east,north] 范围'),
          };
        const calculatedBounds = polygonBounds(aoi);
        const boundTolerance = 1e-8;
        if (
          ['west', 'south', 'east', 'north'].some(
            (key, index) =>
              Math.abs(bounds[index] - calculatedBounds[key as keyof typeof calculatedBounds]) >
              boundTolerance,
          )
        ) {
          return {
            status: 'rejected',
            context_revision: contextRevision.value,
            workspace: workspace.value,
            error: makeAiToolError('选区 bounds 与 Polygon 几何不一致', 'AOI_BOUNDS_MISMATCH'),
          };
        }
        const regionGeometry = workspaceStore.selectedRegion.value?.geometry;
        if (
          regionGeometry &&
          polygonError(aoi.coordinates[0].slice(0, -1) as [number, number][], regionGeometry)
        ) {
          return {
            status: 'rejected',
            context_revision: contextRevision.value,
            workspace: workspace.value,
            error: makeAiToolError('选区不符合当前行政区的合法范围', 'AOI_INVALID'),
          };
        }
        workspaceStore.setSelectedArea(
          { west: bounds[0], south: bounds[1], east: bounds[2], north: bounds[3] },
          aoi,
        );
        return { status: 'applied', context_revision: bumpRevision(), workspace: workspace.value };
      }
      case 'load_phase':
      case 'load_phases': {
        const phaseIds = Array.isArray(args.phase_ids)
          ? args.phase_ids.filter((item): item is string => typeof item === 'string')
          : typeof args.phase_id === 'string'
            ? [args.phase_id]
            : [];
        if (
          !phaseIds.length ||
          phaseIds.some(
            (id) =>
              !workspaceStore.catalogPhases.value.some(
                (phase) => phase.id === id && phase.available,
              ),
          )
        ) {
          return {
            status: 'rejected',
            context_revision: contextRevision.value,
            workspace: workspace.value,
            error: makeAiToolError('请求的时相不在当前区域目录中'),
          };
        }
        if (Array.isArray(args.scene_ids)) {
          const sceneIds = args.scene_ids.filter(
            (item): item is string => typeof item === 'string',
          );
          const availableSceneIds = new Set(
            phaseIds.flatMap(
              (id) =>
                workspaceStore.catalogPhases.value
                  .find((phase) => phase.id === id)
                  ?.scenes.map((scene) => scene.id) || [],
            ),
          );
          if (sceneIds.some((id) => !availableSceneIds.has(id)))
            return {
              status: 'rejected',
              context_revision: contextRevision.value,
              workspace: workspace.value,
              error: makeAiToolError('请求的场景不属于当前时相目录', 'SCENE_NOT_FOUND'),
            };
        }
        phaseIds.forEach((id) => workspaceStore.addPhase(id));
        return { status: 'applied', context_revision: bumpRevision(), workspace: workspace.value };
      }
      case 'select_region': {
        const regionId = stringValue(args.region_id);
        if (!regionId)
          return {
            status: 'rejected',
            context_revision: contextRevision.value,
            workspace: workspace.value,
            error: makeAiToolError('缺少 region_id', 'UI_ACTION_INVALID'),
          };
        const collection = await getRegionData<RegionCollection>('/py/local/regions');
        const feature = collection.features.find(
          (item) => item.id === regionId || item.properties.region_id === regionId,
        );
        if (!feature)
          return {
            status: 'rejected',
            context_revision: contextRevision.value,
            workspace: workspace.value,
            error: makeAiToolError('找不到请求的行政区', 'REGION_NOT_FOUND'),
          };
        await workspaceStore.selectWorkspaceRegion(feature);
        getAiMapAdapter()?.selectRegion?.(regionId);
        return { status: 'applied', context_revision: bumpRevision(), workspace: workspace.value };
      }
      case 'show_task':
      case 'show_result': {
        const taskId = stringValue(args.task_id) || stringValue(args.result_id);
        if (!taskId)
          return {
            status: 'rejected',
            context_revision: contextRevision.value,
            workspace: workspace.value,
            error: makeAiToolError('缺少 task_id', 'UI_ACTION_INVALID'),
          };
        let pair = workspaceStore.pairs.value.find((item) => item.taskId === taskId);
        if (!pair) {
          const task = await getAiTask(taskId);
          const [before, after] = await Promise.all([
            getAiImage(task.before_id),
            getAiImage(task.after_id),
          ]);
          pair = {
            key: `${before.id}:${after.id}`,
            before: { ...before, phase: 'before' },
            after: { ...after, phase: 'after' },
            color: pairColor(workspaceStore.pairs.value.length),
            status: taskStatus(task.status),
            taskId,
          };
          workspaceStore.images.value = [before, after];
          workspaceStore.pairs.value = [pair];
          if (pair.status === 'succeeded') {
            const bounds = pairBounds(before, after);
            const payload = bounds
              ? await workspaceStore.loadResultOverlay(taskId, undefined, bounds, pair)
              : undefined;
            if (payload) {
              pair.stats = payload.stats;
              pair.geojson = payload.geojson;
              payload.pair = { ...pair };
              workspaceStore.results.value = [payload];
            }
          }
        }
        workspaceStore.selectedPairKey.value = pair.key;
        await router.push(command.action === 'show_task' ? '/comparison' : '/reports');
        return { status: 'applied', context_revision: bumpRevision(), workspace: workspace.value };
      }
      case 'open_report':
        await router.push('/reports');
        return { status: 'applied', context_revision: bumpRevision(), workspace: workspace.value };
      case 'fly_to_bounds': {
        const bounds = parseBounds(args.bounds);
        const adapter = getAiMapAdapter();
        if (!bounds || !adapter)
          return {
            status: 'rejected',
            context_revision: contextRevision.value,
            workspace: workspace.value,
            error: makeAiToolError('当前界面没有可用的地图定位适配器', 'MAP_ADAPTER_UNAVAILABLE'),
          };
        // AI bounds remain in the WGS84 wire order through the typed adapter.
        adapter.flyToBounds(bounds);
        return { status: 'applied', context_revision: bumpRevision(), workspace: workspace.value };
      }
      default:
        return {
          status: 'rejected',
          context_revision: contextRevision.value,
          workspace: workspace.value,
          error: makeAiToolError(`不支持的界面操作：${command.action}`, 'UI_ACTION_UNSUPPORTED'),
        };
    }
  }

  async function handleUiCommand(command: AiUiCommand): Promise<void> {
    if (handledActionIds.has(command.action_id)) {
      const pending = pendingAcks.get(command.action_id);
      if (pending) void acknowledgeWithRetry(pending.command, pending.ack);
      return;
    }
    handledActionIds.add(command.action_id);
    const acknowledgement = await applyUiCommand(command).catch(
      (cause: unknown): AiActionAck => ({
        status: 'rejected',
        context_revision: contextRevision.value,
        workspace: workspace.value,
        error: makeAiToolError(
          cause instanceof Error ? cause.message : '界面操作失败',
          'UI_ACTION_FAILED',
        ),
      }),
    );
    if (acknowledgement.status === 'rejected' && acknowledgement.error)
      append('error', acknowledgement.error.message);
    await acknowledgeWithRetry(command, acknowledgement);
  }

  const recheckConfigured = () => {
    if (route.path === '/overview') void refreshConfigured(false).then(restoreStoredSession);
  };
  onMounted(() => {
    void refreshConfigured(true).then(restoreStoredSession);
    window.addEventListener('focus', recheckConfigured);
    document.addEventListener('visibilitychange', recheckConfigured);
  });
  watch(
    () => route.path,
    (path) => {
      if (path === '/overview') void refreshConfigured(false).then(restoreStoredSession);
    },
  );
  onUnmounted(() => {
    window.removeEventListener('focus', recheckConfigured);
    document.removeEventListener('visibilitychange', recheckConfigured);
    activeStream?.abort();
    workflowStreams.forEach((controller) => controller.abort());
    ackRetryTimers.forEach((timer) => clearTimeout(timer));
  });

  return {
    configured,
    open,
    input,
    sending,
    error,
    messages,
    workspace,
    toggle,
    close,
    send,
    handleUiCommand,
  };
}
