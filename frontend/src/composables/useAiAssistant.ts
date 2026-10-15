import {
  computed,
  onMounted,
  onUnmounted,
  ref,
  watch,
  type ComputedRef,
  type Ref,
} from 'vue';
import { useRoute, useRouter } from 'vue-router';
import { useWorkspace, type DetectionRequest } from './useWorkspace';
import {
  acknowledgeAiAction,
  AiStreamError,
  asEventRecord,
  closeAiSession,
  createAiSession,
  getAiImage,
  getAiTask,
  streamAiMessage,
  streamAiWorkflowEvents,
  type AiActionAck,
  type AiServerEvent,
  type AiUiCommand,
  type AiWorkspaceContext,
  isAiUiCommand,
  makeAiToolError,
  waitForAiRetry,
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
  turnId?: string;
  phase?: 'process' | 'final';
}

export type AiTurnStatus = 'running' | 'succeeded' | 'needs_clarification' | 'failed' | 'cancelled' | 'interrupted';

export interface AiConversationTurn {
  id: string;
  workflowId?: string;
  requestId?: string;
  status: AiTurnStatus;
  entries: AiTimelineEntry[];
  result: string;
  error: string;
  cursor?: string;
  canReconnect: boolean;
}

export interface AiAssistant {
  configured: Ref<boolean>;
  open: Ref<boolean>;
  input: Ref<string>;
  sending: ComputedRef<boolean>;
  closingSession: Ref<boolean>;
  error: Ref<string>;
  messages: Ref<AiTimelineEntry[]>;
  turns: Ref<AiConversationTurn[]>;
  workspace: ComputedRef<AiWorkspaceContext>;
  toggle: () => void;
  close: () => void;
  send: () => Promise<void>;
  newConversation: () => Promise<void>;
  reconnect: (turnId: string) => Promise<void>;
  handleUiCommand: (command: AiUiCommand) => Promise<void>;
}

interface SessionOperation {
  generation: number;
  signal: AbortSignal;
}

type TurnConnection =
  | {
      kind: 'message';
      sessionId: string;
      body: { text: string; request_id: string; workspace: AiWorkspaceContext };
    }
  | { kind: 'workflow'; workflowId: string };

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
    const tool = stringValue(value.tool);
    if (isRecord(value.error) && stringValue(value.error.message))
      return `${tool ? `${tool}：` : ''}${value.error.message}`;
    if (tool)
      return `${tool} · ${value.status === 'pending' ? '已提交，处理中' : value.status === 'error' ? '调用失败' : fallback}`;
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
  const error = ref('');
  const messages = ref<AiTimelineEntry[]>([]);
  const turns = ref<AiConversationTurn[]>([]);
  const closingSession = ref(false);
  const sending = computed(
    () => turns.value.some((turn) => turn.status === 'running'),
  );
  const sessionId = ref<string>();
  // Reload starts a new workspace. Remove keys left by older versions, but keep
  // sessions and reconnect cursors in memory for SPA navigation only.
  try {
    sessionStorage.removeItem(SESSION_STORAGE_KEY);
    sessionStorage.removeItem(CONTEXT_REVISION_KEY);
  } catch {
    /* private browsing */
  }
  const contextRevision = ref(newId('workspace'));
  let generation = 0;
  let disposed = false;
  let sessionController = new AbortController();
  const connections = new Map<string, TurnConnection>();
  const connectedTurns = new Set<string>();
  const seenEvents = new Set<string>();
  const handledActionIds = new Set<string>();
  const acknowledgedActionIds = new Set<string>();
  const pendingAcks = new Map<
    string,
    { ack: AiActionAck; command: AiUiCommand; sessionId: string; turnId?: string }
  >();
  const inFlightCommands = new Map<string, { command: AiUiCommand; turnId?: string }>();
  const ackInFlight = new Set<string>();
  const ackRetryTimers = new Map<string, ReturnType<typeof setTimeout>>();
  let lastConfigCheck = 0;
  let configCheck: Promise<void> | null = null;

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
      available_phases: workspaceStore.catalogPhases.value
        .filter((phase) => phase.available)
        .map((phase) => ({
          id: phase.id,
          month: phase.month,
          scene_ids: phase.scenes.filter((scene) => scene.available).map((scene) => scene.id),
        })),
      catalog_status: workspaceStore.catalogLoading.value || workspaceStore.indexLoading.value
        ? 'loading'
        : workspaceStore.catalogError.value || workspaceStore.indexError.value ? 'error' : 'ready',
      selection_error: workspaceStore.selectionError.value || undefined,
      active_phase: workspaceStore.activeMonth.value || undefined,
      bounds: area ? [area.west, area.south, area.east, area.north] : undefined,
      aoi: workspaceStore.detectionAoi.value || undefined,
      task_id: workspaceStore.currentTaskId.value || undefined,
      detection: workspaceStore.getDetectionSelection() ? { model_id: workspaceStore.selectedModel.value, config_revision: workspaceStore.getDetectionSelection()!.config_revision, settings: { ...workspaceStore.detectionSettings } } : undefined,
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
    const next = newId('workspace');
    contextRevision.value = next;
    return next;
  }

  const workspaceFingerprint = computed(() =>
    JSON.stringify({
      page: workspace.value.page,
      region_id: workspace.value.region_id,
      phase_ids: workspace.value.phase_ids,
      available_phases: workspace.value.available_phases,
      catalog_status: workspace.value.catalog_status,
      bounds: workspace.value.bounds,
      aoi: workspace.value.aoi,
      task_id: workspace.value.task_id,
      active_phase: workspace.value.active_phase,
      detection: workspace.value.detection,
    }),
  );
  watch(workspaceFingerprint, (next, previous) => {
    if (previous !== undefined && next !== previous) bumpRevision();
  }, { flush: 'sync' });

  function operation(): SessionOperation {
    return { generation, signal: sessionController.signal };
  }

  function isCurrent(op: SessionOperation): boolean {
    return !disposed && op.generation === generation && !op.signal.aborted;
  }

  function requireCurrent(op: SessionOperation): void {
    if (!isCurrent(op)) throw new DOMException('Session changed', 'AbortError');
  }

  function createTurn(workflowId?: string, requestId?: string): AiConversationTurn {
    turns.value.push({
      id: newId('turn'),
      workflowId,
      requestId,
      status: 'running',
      entries: [],
      result: '',
      error: '',
      canReconnect: false,
    });
    return turns.value[turns.value.length - 1];
  }

  function append(
    kind: AiEntryKind,
    text: string,
    detail?: unknown,
    target?: AiConversationTurn,
  ): AiTimelineEntry {
    const turn = target || turns.value[turns.value.length - 1] || createTurn();
    const entry: AiTimelineEntry = {
      id: newId('ai'),
      kind,
      text,
      detail,
      timestamp: Date.now(),
      turnId: turn.id,
      phase: 'process',
    };
    messages.value.push(entry);
    turn.entries.push(entry);
    return turn.entries[turn.entries.length - 1];
  }

  async function refreshConfigured(force = false): Promise<void> {
    const now = Date.now();
    if (configCheck) return configCheck;
    if (!force && now - lastConfigCheck < AI_STATUS_RECHECK_MS) return;
    lastConfigCheck = now;
    configCheck = aiConfigured()
      .then((value) => {
        if (!disposed) configured.value = value;
      })
      .catch(() => {
        if (!disposed) configured.value = false;
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

  async function ensureSession(op: SessionOperation): Promise<string> {
    requireCurrent(op);
    if (sessionId.value) return sessionId.value;
    const created = await createAiSession(op.signal);
    requireCurrent(op);
    sessionId.value = created.id;
    return created.id;
  }

  async function acknowledgeWithRetry(
    command: AiUiCommand,
    acknowledgement: AiActionAck,
    op: SessionOperation,
    id: string,
    turn?: AiConversationTurn,
  ): Promise<void> {
    if (!isCurrent(op)) return;
    const actionId = command.action_id;
    if (acknowledgedActionIds.has(actionId) || ackInFlight.has(actionId)) return;
    ackInFlight.add(actionId);
    pendingAcks.set(actionId, { command, ack: acknowledgement, sessionId: id, turnId: turn?.id });
    try {
      let lastError: unknown;
      for (let attempt = 0; attempt <= AI_ACK_RETRY_DELAYS_MS.length; attempt += 1) {
        requireCurrent(op);
        if (attempt > 0) {
          await waitForAiRetry(AI_ACK_RETRY_DELAYS_MS[attempt - 1], op.signal);
        }
        try {
          await acknowledgeAiAction(id, actionId, acknowledgement, op.signal);
          requireCurrent(op);
          acknowledgedActionIds.add(actionId);
          pendingAcks.delete(actionId);
          return;
        } catch (cause: unknown) {
          if (!isCurrent(op)) return;
          lastError = cause;
        }
      }
      if (!ackRetryTimers.has(actionId)) {
        const timer = setTimeout(() => {
          ackRetryTimers.delete(actionId);
          if (isCurrent(op)) void acknowledgeWithRetry(command, acknowledgement, op, id, turn);
        }, 5_000);
        ackRetryTimers.set(actionId, timer);
      }
      append(
        'error',
        lastError instanceof Error ? lastError.message : '界面操作回执失败，正在重试',
        undefined,
        turn,
      );
    } catch (cause: unknown) {
      if (isCurrent(op))
        append(
          'error',
          cause instanceof Error ? cause.message : '界面操作回执失败',
          undefined,
          turn,
        );
    } finally {
      if (isCurrent(op)) ackInFlight.delete(actionId);
    }
  }

  function close(): void {
    open.value = false;
  }
  function toggle(): void {
    open.value = !open.value;
  }

  function finishTurn(turn: AiConversationTurn, text: string, status?: string): void {
    turn.result = text;
    turn.status = status === 'needs_clarification' ? 'needs_clarification' : 'succeeded';
    turn.error = '';
    turn.canReconnect = false;
    const lastAssistant = [...turn.entries].reverse().find((entry) => entry.kind === 'assistant');
    if (lastAssistant?.text === text) lastAssistant.phase = 'final';
    const existing = turn.entries.find((entry) => entry.kind === 'final');
    if (existing) existing.text = text;
    else append('final', text, undefined, turn).phase = 'final';
  }

  function onStreamEvent(
    event: AiServerEvent,
    turn: AiConversationTurn,
    op: SessionOperation,
  ): void {
    if (!isCurrent(op)) return;
    const payload = event.data;
    const record = asEventRecord(payload);
    if (event.type === 'workflow_started') {
      if (record?.session_id && record.session_id !== sessionId.value) return;
      if (record?.request_id && turn.requestId && record.request_id !== turn.requestId) return;
      if (typeof record?.workflow_id === 'string') turn.workflowId = record.workflow_id;
      if (typeof record?.request_id === 'string') turn.requestId = record.request_id;
    }
    if (event.id) {
      const key = `${turn.workflowId || turn.id}:${event.id}`;
      if (seenEvents.has(key)) return;
      seenEvents.add(key);
      if (!Number.isFinite(Number(event.id)) || Number(event.id) >= Number(turn.cursor || 0))
        turn.cursor = event.id;
    }
    switch (event.type) {
      case 'workflow_started':
      case 'message':
        return;
      case 'text_delta': {
        const text = record ? stringValue(record.text) : stringValue(payload);
        if (!text) return;
        const current = turn.entries[turn.entries.length - 1];
        if (current?.kind === 'assistant' && current.detail === 'streaming') current.text += text;
        else {
          const entry = append('assistant', text, undefined, turn);
          entry.detail = 'streaming';
        }
        return;
      }
      case 'plan':
        append('plan', actionText(payload, '已生成执行计划'), payload, turn);
        return;
      case 'intent_progress':
        append('progress', actionText(payload, '正在解析需求'), payload, turn);
        return;
      case 'tool_started':
        append('tool', actionText(payload, '正在调用工具…'), payload, turn);
        return;
      case 'tool_finished':
        append('tool', actionText(payload, '工具调用完成'), payload, turn);
        return;
      case 'tool_progress':
        append(
          'progress',
          typeof record?.attempt === 'number' && typeof record.max_attempts === 'number'
            ? `正在重试（${record.attempt}/${record.max_attempts}），${Number(record.delay_ms || 0) / 1000} 秒后继续`
            : actionText(payload, '正在处理…'),
          payload,
          turn,
        );
        return;
      case 'ui_command':
        if (isAiUiCommand(payload)) void handleUiCommand(payload, op, turn);
        else append('error', '收到格式无效的界面操作指令', undefined, turn);
        return;
      case 'final':
        finishTurn(
          turn,
          stringValue(record?.message) ||
            [...turn.entries].reverse().find((entry) => entry.kind === 'assistant')?.text ||
            turn.result ||
            '处理完成',
          stringValue(record?.status),
        );
        return;
      case 'cancelled':
        turn.status = 'cancelled';
        turn.result = actionText(payload, '任务已取消');
        turn.error = '';
        turn.canReconnect = false;
        return;
      case 'error': {
        const message = record?.message || record?.error || 'AI 请求失败';
        turn.error = typeof message === 'string' ? message : 'AI 请求失败';
        turn.status = 'failed';
        turn.canReconnect = false;
        append('error', turn.error, payload, turn);
        return;
      }
      default:
        if (typeof payload === 'string' && payload) append('system', payload, undefined, turn);
    }
  }

  async function connectTurn(turn: AiConversationTurn, op: SessionOperation): Promise<void> {
    if (!isCurrent(op) || connectedTurns.has(turn.id)) return;
    const connection = connections.get(turn.id);
    if (!connection) return;
    connectedTurns.add(turn.id);
    turn.status = 'running';
    turn.error = '';
    turn.canReconnect = false;
    const options = {
      after: turn.cursor,
      workflowId: turn.workflowId,
      onReconnect: (attempt: number, maxAttempts: number, delayMs: number) => {
        if (!isCurrent(op)) return;
        append(
          'progress',
          `连接中断，${delayMs / 1000} 秒后尝试恢复（${attempt}/${maxAttempts}）`,
          { stage: 'reconnect', attempt, max_attempts: maxAttempts, delay_ms: delayMs },
          turn,
        );
      },
    };
    try {
      const onEvent = (event: AiServerEvent) => onStreamEvent(event, turn, op);
      if (connection.kind === 'message')
        await streamAiMessage(connection.sessionId, connection.body, onEvent, op.signal, options);
      else
        await streamAiWorkflowEvents(
          connection.workflowId,
          Number(turn.cursor) || 0,
          onEvent,
          op.signal,
          options,
        );
    } catch (cause: unknown) {
      if (!isCurrent(op)) return;
      turn.status = cause instanceof AiStreamError && !cause.retryable ? 'failed' : 'interrupted';
      turn.error = cause instanceof Error ? cause.message : 'AI 连接中断';
      turn.canReconnect = turn.status === 'interrupted';
    } finally {
      if (isCurrent(op)) connectedTurns.delete(turn.id);
    }
  }

  async function send(): Promise<void> {
    const text = input.value.trim();
    if (
      !text ||
      sending.value ||
      closingSession.value ||
      turns.value.some((turn) => turn.canReconnect) ||
      !configured.value
    )
      return;
    const op = operation();
    input.value = '';
    error.value = '';
    const turn = createTurn(undefined, newId('request'));
    append('user', text, undefined, turn);
    try {
      const id = await ensureSession(op);
      requireCurrent(op);
      const body = {
        text,
        request_id: turn.requestId || newId('request'),
        workspace: JSON.parse(JSON.stringify(workspace.value)) as AiWorkspaceContext,
      };
      connections.set(turn.id, { kind: 'message', sessionId: id, body });
      await connectTurn(turn, op);
    } catch (cause: unknown) {
      if (!isCurrent(op)) return;
      turn.error = cause instanceof Error ? cause.message : 'AI 请求失败';
      turn.status = 'failed';
    }
  }

  async function reconnect(turnId: string): Promise<void> {
    if (closingSession.value) return;
    const turn = turns.value.find((item) => item.id === turnId);
    if (!turn?.canReconnect) return;
    error.value = '';
    const op = operation();
    for (const pending of pendingAcks.values()) {
      if (pending.sessionId === sessionId.value)
        void acknowledgeWithRetry(
          pending.command,
          pending.ack,
          op,
          pending.sessionId,
          turns.value.find((item) => item.id === pending.turnId),
        );
    }
    await connectTurn(turn, op);
  }

  function invalidateSession(): void {
    generation += 1;
    sessionController.abort();
    sessionController = new AbortController();
    connectedTurns.clear();
    ackRetryTimers.forEach((timer) => clearTimeout(timer));
    ackRetryTimers.clear();
    ackInFlight.clear();
    for (const pending of inFlightCommands.values()) {
      if (sessionId.value && !pendingAcks.has(pending.command.action_id)) {
        pendingAcks.set(pending.command.action_id, {
          ...pending,
          sessionId: sessionId.value,
          ack: {
            status: 'failed',
            error: makeAiToolError('对话切换时界面操作已停止', 'UI_ACTION_CANCELLED'),
          },
        });
      }
    }
    inFlightCommands.clear();
  }

  async function newConversation(): Promise<void> {
    if (closingSession.value) return;
    const previous = sessionId.value;
    invalidateSession();
    const op = operation();
    closingSession.value = true;
    error.value = '';
    for (const turn of turns.value) {
      if (turn.status === 'running') {
        turn.status = 'interrupted';
        turn.canReconnect = connections.has(turn.id);
      }
    }
    try {
      if (previous) await closeAiSession(previous, op.signal);
      requireCurrent(op);
      sessionId.value = undefined;
      try {
        sessionStorage.removeItem(SESSION_STORAGE_KEY);
      } catch {
        /* private browsing */
      }
      messages.value = [];
      turns.value = [];
      input.value = '';
      connections.clear();
      seenEvents.clear();
      handledActionIds.clear();
      acknowledgedActionIds.clear();
      pendingAcks.clear();
    } catch (cause: unknown) {
      if (!isCurrent(op)) return;
      error.value = `${cause instanceof Error ? cause.message : '结束旧对话失败'}。旧任务可能仍在运行，请重试新对话或恢复连接。`;
    } finally {
      if (isCurrent(op)) closingSession.value = false;
    }
  }

  async function applyUiCommand(command: AiUiCommand, op: SessionOperation): Promise<AiActionAck> {
    requireCurrent(op);
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
      ['set_existing_aoi', 'load_phase', 'load_phases', 'select_region', 'submit_detection'].includes(command.action)
    ) {
      return {
        status: 'rejected',
        context_revision: contextRevision.value,
        workspace: workspace.value,
        error: makeAiToolError('检测任务正在运行，当前工作区暂时不可修改', 'WORKSPACE_BUSY'),
      };
    }
    switch (command.action) {
      case 'submit_detection': {
        try {
          const result = await workspaceStore.submitDetection(args as unknown as DetectionRequest, () => isCurrent(op));
          return { status: 'applied', context_revision: contextRevision.value, workspace: workspace.value, result };
        } catch (cause: unknown) {
          return { status: 'failed', context_revision: contextRevision.value, workspace: workspace.value, error: makeAiToolError(cause instanceof Error ? cause.message : '检测提交失败', 'DETECTION_SUBMIT_FAILED') };
        }
      }
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
        return { status: 'applied', context_revision: contextRevision.value, workspace: workspace.value };
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
        return { status: 'applied', context_revision: contextRevision.value, workspace: workspace.value };
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
        const collection = await getRegionData<RegionCollection>('/py/local/regions', op.signal);
        requireCurrent(op);
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
        await workspaceStore.selectWorkspaceRegion(feature, () => isCurrent(op), op.signal);
        requireCurrent(op);
        getAiMapAdapter()?.selectRegion?.(regionId);
        return { status: 'applied', context_revision: contextRevision.value, workspace: workspace.value };
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
          const task = await getAiTask(taskId, op.signal);
          requireCurrent(op);
          const [before, after] = await Promise.all([
            getAiImage(task.before_id, op.signal),
            getAiImage(task.after_id, op.signal),
          ]);
          requireCurrent(op);
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
              ? await workspaceStore.loadResultOverlay(
                  taskId,
                  undefined,
                  bounds,
                  pair,
                  () => isCurrent(op),
                  op.signal,
                )
              : undefined;
            requireCurrent(op);
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
        requireCurrent(op);
        return { status: 'applied', context_revision: contextRevision.value, workspace: workspace.value };
      }
      case 'open_report':
        await router.push('/reports');
        requireCurrent(op);
        return { status: 'applied', context_revision: contextRevision.value, workspace: workspace.value };
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
        return { status: 'applied', context_revision: contextRevision.value, workspace: workspace.value };
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

  async function handleUiCommand(
    command: AiUiCommand,
    op = operation(),
    turn?: AiConversationTurn,
  ): Promise<void> {
    if (!isCurrent(op)) return;
    const id = sessionId.value;
    if (!id) return;
    if (handledActionIds.has(command.action_id)) {
      const pending = pendingAcks.get(command.action_id);
      if (pending)
        void acknowledgeWithRetry(pending.command, pending.ack, op, pending.sessionId, turn);
      return;
    }
    handledActionIds.add(command.action_id);
    inFlightCommands.set(command.action_id, { command, turnId: turn?.id });
    const acknowledgement = await applyUiCommand(command, op).catch(
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
    if (!isCurrent(op)) return;
    inFlightCommands.delete(command.action_id);
    if (acknowledgement.status === 'rejected' && acknowledgement.error)
      append('error', acknowledgement.error.message, undefined, turn);
    await acknowledgeWithRetry(command, acknowledgement, op, id, turn);
  }

  const recheckConfigured = () => {
    if (route.path === '/overview') void refreshConfigured(false);
  };
  onMounted(() => {
    void refreshConfigured(true);
    window.addEventListener('focus', recheckConfigured);
    document.addEventListener('visibilitychange', recheckConfigured);
  });
  watch(
    () => route.path,
    (path) => {
      if (path === '/overview') void refreshConfigured(false);
    },
  );
  onUnmounted(() => {
    disposed = true;
    window.removeEventListener('focus', recheckConfigured);
    document.removeEventListener('visibilitychange', recheckConfigured);
    invalidateSession();
  });

  return {
    configured,
    open,
    input,
    sending,
    closingSession,
    error,
    messages,
    turns,
    workspace,
    toggle,
    close,
    send,
    newConversation,
    reconnect,
    handleUiCommand,
  };
}
