<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue';
import {
  useAiAssistant,
  type AiTimelineEntry,
  type AiConversationTurn,
} from '../composables/useAiAssistant';
import { safeAiResourceReferences } from '../utils/ai';
import { renderAiMarkdown } from '../utils/aiMarkdown';
const props = defineProps<{ visible: boolean }>();

const assistant = useAiAssistant();
const inputRef = ref<HTMLTextAreaElement | null>(null);
const panelRef = ref<HTMLElement | null>(null);
const messageListRef = ref<HTMLElement | null>(null);
const visibleTurns = computed(() => assistant.turns.value.slice(-40));
const processViewports = new Map<string, HTMLElement>();
const processFollowing = new Map<string, boolean>();
let followMessages = true;

function focusInput(): void {
  void nextTick(() => inputRef.value?.focus());
}

function toggle(): void {
  assistant.toggle();
  if (!assistant.open.value) return;
  focusInput();
}

function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') {
    assistant.close();
    return;
  }
  if (event.target !== inputRef.value) return;
  if (event.key === 'Enter' && !event.shiftKey) {
    event.preventDefault();
    void assistant.send();
  }
}

function entryLabel(entry: AiTimelineEntry): string {
  switch (entry.kind) {
    case 'user':
      return '你';
    case 'plan':
      return '执行计划';
    case 'tool':
      return '工具';
    case 'progress':
      return '进度';
    case 'error':
      return '错误';
    case 'final':
      return '完成';
    case 'system':
      return '系统';
    default:
      return 'AI';
  }
}

function turnDownloads(turn: AiConversationTurn): string[] {
  return safeAiResourceReferences(turn.entries.map((entry) => entry.detail)).downloads;
}

function turnResources(turn: AiConversationTurn): string[] {
  const labels = {
    task_id: '任务',
    result_id: '结果',
    workflow_id: '工作流',
    operation_id: '准备操作',
  };
  return safeAiResourceReferences(turn.entries.map((entry) => entry.detail)).resources.map(
    (resource) => `${labels[resource.kind]}：${resource.id}`,
  );
}

function processEntries(turn: AiConversationTurn): AiTimelineEntry[] {
  return turn.entries.filter(
    (entry) => !['user', 'final', 'error'].includes(entry.kind) && entry.phase !== 'final',
  );
}

function turnErrors(turn: AiConversationTurn): string[] {
  return [
    ...new Set(
      [
        ...turn.entries.filter((entry) => entry.kind === 'error').map((entry) => entry.text),
        turn.error,
      ].filter(Boolean),
    ),
  ];
}

function processStatus(turn: AiConversationTurn): string {
  return {
    running: '执行中',
    succeeded: '已完成',
    failed: '执行失败',
    cancelled: '已取消',
    interrupted: '连接中断',
  }[turn.status];
}

function setProcessViewport(turnId: string, value: unknown): void {
  if (value instanceof HTMLElement) processViewports.set(turnId, value);
  else {
    processViewports.delete(turnId);
    processFollowing.delete(turnId);
  }
}

function onMessageScroll(): void {
  const list = messageListRef.value;
  if (list) followMessages = list.scrollHeight - list.scrollTop - list.clientHeight < 32;
}

function onProcessScroll(turnId: string): void {
  const viewport = processViewports.get(turnId);
  if (viewport)
    processFollowing.set(
      turnId,
      viewport.scrollHeight - viewport.scrollTop - viewport.clientHeight < 24,
    );
}

async function startNewConversation(): Promise<void> {
  await assistant.newConversation();
  if (!assistant.error.value) {
    followMessages = true;
    focusInput();
  }
}

function panelOrigin(element: Element): void {
  const panel = element as HTMLElement;
  const launcher = document.querySelector('.ai-launcher');
  if (!launcher) return;
  const source = launcher.getBoundingClientRect();
  const styles = getComputedStyle(panel);
  const height = panel.offsetHeight || Number.parseFloat(styles.height) || 0;
  const bottom = Number.parseFloat(styles.bottom) || 0;
  panel.style.setProperty(
    '--ai-origin-x',
    `${source.left + source.width / 2 - window.innerWidth / 2}px`,
  );
  panel.style.setProperty(
    '--ai-origin-y',
    `${source.top + source.height / 2 - (window.innerHeight - bottom - height / 2)}px`,
  );
}

watch(
  () => assistant.open.value,
  (open) => {
    if (open) focusInput();
  },
);
watch(
  () => props.visible,
  (visible) => {
    if (!visible) assistant.close();
  },
);
watch(
  () =>
    assistant.turns.value
      .map(
        (turn) =>
          `${turn.id}:${turn.status}:${turn.result}:${turn.error}:${turn.entries.length}:${turn.entries[turn.entries.length - 1]?.text || ''}`,
      )
      .join('\0'),
  () => {
    void nextTick(() => {
      const list = messageListRef.value;
      if (list?.isConnected && followMessages) list.scrollTop = list.scrollHeight;
      for (const [turnId, viewport] of processViewports) {
        if (viewport.isConnected && processFollowing.get(turnId) !== false)
          viewport.scrollTop = viewport.scrollHeight;
      }
    });
  },
);
</script>

<template>
  <div v-if="props.visible && assistant.configured.value" class="ai-assistant-root">
    <Teleport to="body">
      <button
        class="ai-launcher"
        type="button"
        :aria-expanded="assistant.open.value"
        aria-controls="ai-assistant-dialog"
        aria-label="打开 AI 助手"
        title="AI 助手"
        @click="toggle"
      >
        <span class="ai-launcher-ring" aria-hidden="true" />
        <span class="ai-launcher-mark" aria-hidden="true">AI</span>
        <span v-if="assistant.sending.value" class="ai-launcher-status" aria-hidden="true" />
      </button>
    </Teleport>

    <Teleport to="body">
      <Transition name="ai-panel" @enter="panelOrigin" @before-leave="panelOrigin">
        <section
          v-if="assistant.open.value"
          id="ai-assistant-dialog"
          ref="panelRef"
          class="ai-dialog"
          role="dialog"
          aria-modal="false"
          aria-labelledby="ai-dialog-title"
          @keydown="onKeydown"
        >
          <header class="ai-dialog-header">
            <div class="ai-dialog-heading">
              <span class="ai-dialog-kicker">AI WORKSPACE</span>
              <h2 id="ai-dialog-title">自然语言助手</h2>
            </div>
            <div class="ai-dialog-actions">
              <button
                class="ai-new-conversation"
                type="button"
                :disabled="assistant.closingSession.value"
                @click="startNewConversation"
              >
                {{ assistant.closingSession.value ? '正在结束旧对话…' : '新对话' }}
              </button>
              <span v-if="assistant.sending.value" class="ai-live-status" role="status"
                >处理中</span
              >
              <button
                class="ai-icon-button"
                type="button"
                aria-label="关闭 AI 助手"
                title="关闭"
                @click="assistant.close"
              >
                <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m6 6 12 12M18 6 6 18" /></svg>
              </button>
            </div>
          </header>

          <div ref="messageListRef" class="ai-message-list" @scroll="onMessageScroll">
            <div v-if="!visibleTurns.length" class="ai-empty-state">
              <span class="ai-empty-glyph" aria-hidden="true">✦</span>
              <p>描述要查询的区域、时相或检测结果</p>
              <span>例如：查询雄县 2025 年 1 月到 7 月的变化</span>
            </div>
            <section
              v-for="turn in visibleTurns"
              :key="turn.id"
              class="ai-turn"
              :data-turn-id="turn.id"
              :data-status="turn.status"
            >
              <article
                v-for="entry in turn.entries.filter((entry) => entry.kind === 'user')"
                :key="entry.id"
                class="ai-message is-user"
              >
                <div class="ai-message-meta">你</div>
                <p>{{ entry.text }}</p>
              </article>
              <details
                v-if="processEntries(turn).length || turn.status === 'running'"
                class="ai-turn-process"
                :open="turn.status === 'running'"
              >
                <summary>
                  <span role="status">{{ processStatus(turn) }}</span
                  ><span> · 执行过程（{{ processEntries(turn).length }}）</span>
                </summary>
                <div
                  :ref="(element) => setProcessViewport(turn.id, element)"
                  class="ai-process-viewport"
                  @scroll="onProcessScroll(turn.id)"
                >
                  <p v-if="!processEntries(turn).length" class="ai-process-placeholder">
                    正在理解请求…
                  </p>
                  <article
                    v-for="entry in processEntries(turn)"
                    :key="entry.id"
                    class="ai-message"
                    :class="`is-${entry.kind}`"
                  >
                    <div class="ai-message-meta">
                      <span>{{ entryLabel(entry) }}</span
                      ><span
                        v-if="
                          turn.status === 'running' &&
                          entry.kind === 'assistant' &&
                          entry.detail === 'streaming'
                        "
                        class="ai-streaming-dot"
                        aria-label="正在生成"
                      />
                    </div>
                    <p>{{ entry.text }}</p>
                    <details
                      v-if="entry.detail && entry.kind !== 'assistant'"
                      class="ai-message-detail"
                    >
                      <summary>查看事件数据</summary>
                      <pre>{{ JSON.stringify(entry.detail, null, 2) }}</pre>
                    </details>
                  </article>
                </div>
              </details>
              <article
                v-if="turn.status === 'succeeded' || turn.status === 'cancelled'"
                class="ai-message is-final"
                aria-live="polite"
              >
                <div class="ai-message-meta">
                  {{ turn.status === 'cancelled' ? '已取消' : '结果' }}
                </div>
                <div class="ai-markdown" v-html="renderAiMarkdown(turn.result || '处理完成')" />
              </article>
              <p
                v-for="message in turnErrors(turn)"
                :key="message"
                class="ai-message is-error"
                role="alert"
              >
                {{ message }}
              </p>
              <div v-if="turnResources(turn).length" class="ai-resource-refs">
                <p v-for="resource in turnResources(turn)" :key="resource">{{ resource }}</p>
              </div>
              <div v-if="turnDownloads(turn).length" class="ai-result-downloads">
                <a
                  v-for="link in turnDownloads(turn)"
                  :key="link"
                  class="ai-download-link"
                  :href="link"
                  download
                  >下载结果</a
                >
              </div>
              <button
                v-if="turn.canReconnect"
                class="ai-reconnect-button"
                type="button"
                :disabled="assistant.closingSession.value"
                @click="assistant.reconnect(turn.id)"
              >
                恢复连接
              </button>
            </section>
            <p v-if="assistant.error.value" class="ai-inline-error" role="alert">
              {{ assistant.error.value }}
            </p>
          </div>

          <form class="ai-composer" @submit.prevent="assistant.send">
            <label class="sr-only" for="ai-message-input">输入给 AI 助手的消息</label>
            <textarea
              id="ai-message-input"
              ref="inputRef"
              v-model="assistant.input.value"
              rows="2"
              maxlength="4000"
              :disabled="assistant.sending.value || assistant.closingSession.value"
              placeholder="输入自然语言指令…"
              @keydown="onKeydown"
            />
            <button
              class="ai-send-button"
              type="submit"
              :disabled="
                assistant.sending.value ||
                assistant.closingSession.value ||
                !assistant.input.value.trim() ||
                assistant.turns.value.some((turn) => turn.canReconnect)
              "
              aria-label="发送消息"
              title="发送"
            >
              <svg viewBox="0 0 24 24" aria-hidden="true">
                <path d="m4 4 16 8-16 8 3-8-3-8Zm3 8h13" />
              </svg>
            </button>
          </form>
        </section>
      </Transition>
    </Teleport>
  </div>
</template>

<style>
.ai-assistant-root {
  position: relative;
  z-index: 1100;
}
.ai-launcher {
  position: fixed;
  z-index: 1100;
  left: 55px;
  bottom: 18px;
  width: 50px;
  height: 50px;
  padding: 0;
  display: grid;
  place-items: center;
  border: 1px solid rgba(116, 224, 244, 0.72);
  border-radius: 50%;
  color: #dffaff;
  background: radial-gradient(
    circle at 35% 30%,
    rgba(92, 227, 248, 0.34),
    rgba(7, 35, 58, 0.97) 68%
  );
  box-shadow:
    0 0 0 4px rgba(79, 216, 239, 0.08),
    0 0 24px rgba(79, 216, 239, 0.32),
    inset 0 0 16px rgba(79, 216, 239, 0.2);
  cursor: pointer;
  transition:
    transform 180ms ease,
    box-shadow 180ms ease,
    border-color 180ms ease;
}
.ai-launcher:hover {
  transform: translateY(-2px);
  border-color: #c8f9ff;
  box-shadow:
    0 0 0 5px rgba(79, 216, 239, 0.1),
    0 0 30px rgba(79, 216, 239, 0.46),
    inset 0 0 18px rgba(79, 216, 239, 0.24);
}
.ai-launcher:active {
  transform: scale(0.96);
}
.ai-launcher-ring {
  position: absolute;
  inset: 6px;
  border: 1px solid rgba(141, 236, 250, 0.4);
  border-radius: 50%;
}
.ai-launcher-mark {
  font:
    700 13px/1 'Rajdhani',
    sans-serif;
  letter-spacing: 1px;
  text-shadow: 0 0 12px rgba(143, 244, 255, 0.85);
}
.ai-launcher-status {
  position: absolute;
  top: 3px;
  right: 3px;
  width: 9px;
  height: 9px;
  border: 2px solid #082138;
  border-radius: 50%;
  background: #72f0ca;
  box-shadow: 0 0 9px #72f0ca;
  animation: ai-pulse 1.2s ease-in-out infinite;
}
@keyframes ai-pulse {
  50% {
    opacity: 0.38;
    transform: scale(0.76);
  }
}
.ai-dialog {
  position: fixed;
  z-index: 2000;
  left: 50%;
  bottom: max(24px, env(safe-area-inset-bottom));
  width: min(680px, calc(100vw - 32px));
  height: min(590px, calc(100dvh - 100px));
  min-height: 390px;
  display: flex;
  flex-direction: column;
  overflow: hidden;
  color: #e2eefa;
  background: linear-gradient(180deg, rgba(11, 33, 55, 0.985), rgba(5, 16, 29, 0.99));
  border: 1px solid rgba(88, 179, 211, 0.58);
  border-radius: 10px;
  box-shadow:
    0 20px 80px rgba(0, 0, 0, 0.54),
    0 0 36px rgba(79, 216, 239, 0.16),
    inset 0 1px 0 rgba(156, 233, 248, 0.1);
  transform: translateX(-50%);
  transform-origin: center center;
}
.ai-panel-enter-active,
.ai-panel-leave-active {
  transition:
    opacity 220ms ease,
    transform 220ms cubic-bezier(0.2, 0.8, 0.2, 1);
}
.ai-panel-enter-from,
.ai-panel-leave-to {
  opacity: 0;
  transform: translate(calc(-50% + var(--ai-origin-x, 0px)), var(--ai-origin-y, 22px)) scale(0.08);
}
.ai-download-link {
  display: inline-block;
  margin-top: 7px;
  color: #9eeaf5;
  font-size: 11px;
  text-decoration: underline;
  text-underline-offset: 2px;
}
.ai-download-link:hover {
  color: #dffaff;
}
.ai-dialog-header {
  min-height: 66px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  padding: 12px 16px;
  border-bottom: 1px solid rgba(74, 132, 176, 0.3);
  background: rgba(10, 31, 51, 0.82);
}
.ai-dialog-heading {
  min-width: 0;
}
.ai-dialog-kicker {
  display: block;
  color: #4fd8ef;
  font:
    600 10px/1.3 'Rajdhani',
    sans-serif;
  letter-spacing: 2px;
}
.ai-dialog h2 {
  margin: 4px 0 0;
  font-size: 17px;
  font-weight: 600;
  letter-spacing: 0.5px;
}
.ai-dialog-actions {
  display: flex;
  align-items: center;
  gap: 12px;
}
.ai-new-conversation,
.ai-reconnect-button {
  padding: 6px 9px;
  border: 1px solid rgba(79, 216, 239, 0.45);
  border-radius: 5px;
  color: #c9f8ff;
  background: rgba(23, 92, 111, 0.35);
  font: inherit;
  font-size: 11px;
  cursor: pointer;
}
.ai-new-conversation:disabled,
.ai-reconnect-button:disabled {
  opacity: 0.5;
  cursor: wait;
}
.ai-reconnect-button {
  align-self: flex-start;
}
.ai-live-status {
  color: #72f0ca;
  font-size: 11px;
}
.ai-icon-button {
  width: 32px;
  height: 32px;
  display: grid;
  place-items: center;
  padding: 0;
  border: 1px solid rgba(74, 132, 176, 0.42);
  border-radius: 6px;
  color: #9abbd3;
  background: rgba(30, 66, 102, 0.45);
  cursor: pointer;
}
.ai-icon-button:hover {
  color: #dffaff;
  border-color: #4fd8ef;
}
.ai-icon-button svg,
.ai-send-button svg {
  width: 17px;
  height: 17px;
  fill: none;
  stroke: currentColor;
  stroke-width: 1.8;
  stroke-linecap: round;
  stroke-linejoin: round;
}
.ai-message-list {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 16px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.ai-empty-state {
  flex: 1;
  min-height: 180px;
  display: grid;
  place-items: center;
  align-content: center;
  gap: 8px;
  text-align: center;
  color: #90a9c3;
  font-size: 12px;
}
.ai-empty-state p {
  margin: 0;
  color: #d6e7f5;
  font-size: 14px;
}
.ai-empty-state span:last-child {
  color: #7593ad;
  font-size: 11px;
}
.ai-empty-glyph {
  color: #4fd8ef;
  font-size: 27px;
  text-shadow: 0 0 16px rgba(79, 216, 239, 0.72);
}
.ai-message {
  max-width: 92%;
  padding: 9px 11px;
  border: 1px solid rgba(74, 132, 176, 0.26);
  border-radius: 7px;
  background: rgba(18, 44, 70, 0.64);
  font-size: 12px;
  line-height: 1.55;
  overflow-wrap: anywhere;
}
.ai-turn {
  display: flex;
  flex-direction: column;
  flex: 0 0 auto;
  min-width: 0;
  gap: 9px;
}
.ai-turn-process {
  min-width: 0;
  border: 1px solid rgba(74, 132, 176, 0.3);
  border-radius: 6px;
  background: rgba(10, 28, 46, 0.6);
}
.ai-turn-process > summary {
  padding: 8px 10px;
  color: #91b4cd;
  font-size: 11px;
  cursor: pointer;
}
.ai-process-viewport {
  box-sizing: border-box;
  height: 160px;
  overflow: auto;
  overscroll-behavior: contain;
  padding: 0 10px 10px;
  display: flex;
  flex-direction: column;
  gap: 7px;
}
.ai-process-viewport .ai-message {
  max-width: 100%;
  flex: 0 0 auto;
  padding: 7px 9px;
  font-size: 11px;
}
.ai-process-placeholder {
  margin: 8px 0;
  color: #91b4cd;
  font-size: 12px;
}
.ai-result-downloads {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
}
.ai-resource-refs {
  padding: 7px 10px;
  color: #a8c8dd;
  border-left: 2px solid rgba(79, 216, 239, 0.5);
  font-size: 11px;
  overflow-wrap: anywhere;
}
.ai-resource-refs p {
  margin: 3px 0;
}
.ai-markdown {
  min-width: 0;
  overflow-wrap: anywhere;
}
.ai-markdown > :first-child {
  margin-top: 0;
}
.ai-markdown > :last-child {
  margin-bottom: 0;
}
.ai-markdown p {
  margin: 0.5em 0;
  white-space: normal;
}
.ai-markdown h1,
.ai-markdown h2,
.ai-markdown h3,
.ai-markdown h4 {
  margin: 0.8em 0 0.4em;
  font-size: 1.15em;
  line-height: 1.4;
  letter-spacing: normal;
}
.ai-markdown ul,
.ai-markdown ol {
  margin: 0.5em 0;
  padding-left: 1.5em;
}
.ai-markdown blockquote {
  margin: 0.6em 0;
  padding-left: 10px;
  border-left: 2px solid #4fd8ef;
  color: #a8c8dd;
}
.ai-markdown pre {
  max-width: 100%;
  overflow-x: auto;
  padding: 9px;
  border-radius: 4px;
  background: rgba(0, 0, 0, 0.3);
  white-space: pre;
}
.ai-markdown code {
  font:
    11px/1.5 ui-monospace,
    Consolas,
    monospace;
}
.ai-markdown table {
  display: block;
  max-width: 100%;
  overflow-x: auto;
  border-collapse: collapse;
}
.ai-markdown th,
.ai-markdown td {
  padding: 5px 8px;
  border: 1px solid rgba(74, 132, 176, 0.4);
  white-space: nowrap;
}
.ai-markdown a {
  color: #9eeaf5;
  text-underline-offset: 2px;
}
.ai-message p {
  margin: 0;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
.ai-message.is-user {
  align-self: flex-end;
  color: #dffaff;
  border-color: rgba(79, 216, 239, 0.4);
  background: rgba(29, 94, 112, 0.42);
}
.ai-message.is-plan,
.ai-message.is-tool,
.ai-message.is-progress {
  border-left: 2px solid #4fd8ef;
}
.ai-message.is-error {
  color: #ffb0b0;
  border-color: rgba(255, 143, 143, 0.44);
  background: rgba(102, 28, 39, 0.3);
}
.ai-message.is-final {
  color: #dfffea;
  border-color: rgba(114, 240, 202, 0.36);
}
.ai-message-meta {
  display: flex;
  align-items: center;
  gap: 7px;
  margin-bottom: 4px;
  color: #7fa9c4;
  font:
    600 10px/1.2 'Rajdhani',
    sans-serif;
  letter-spacing: 1px;
  text-transform: uppercase;
}
.is-user .ai-message-meta {
  color: #9eeaf5;
}
.ai-streaming-dot {
  width: 5px;
  height: 5px;
  border-radius: 50%;
  background: #4fd8ef;
  box-shadow: 0 0 8px #4fd8ef;
  animation: ai-pulse 1s infinite;
}
.ai-message-detail {
  margin-top: 7px;
  color: #8eaac0;
  font-size: 10px;
}
.ai-message-detail summary {
  cursor: pointer;
}
.ai-message-detail pre {
  max-height: 120px;
  margin: 5px 0 0;
  overflow: auto;
  color: #a8c8dd;
  font:
    10px/1.45 ui-monospace,
    SFMono-Regular,
    Consolas,
    monospace;
  white-space: pre-wrap;
}
.ai-inline-error {
  flex: 0 0 auto;
  margin: 0;
  color: #ff9f9f;
  font-size: 11px;
}
.ai-composer {
  display: flex;
  align-items: flex-end;
  gap: 9px;
  padding: 12px 14px 10px;
  border-top: 1px solid rgba(74, 132, 176, 0.3);
  background: rgba(7, 21, 36, 0.74);
}
.ai-composer textarea {
  flex: 1;
  min-width: 0;
  resize: none;
  min-height: 42px;
  max-height: 110px;
  padding: 10px 11px;
  border: 1px solid rgba(74, 132, 176, 0.45);
  border-radius: 6px;
  color: #e2eefa;
  background: rgba(3, 12, 22, 0.7);
  font: inherit;
  font-size: 12px;
  line-height: 1.4;
}
.ai-composer textarea::placeholder {
  color: #6e8aa2;
}
.ai-composer textarea:focus {
  border-color: #4fd8ef;
  outline: 2px solid rgba(79, 216, 239, 0.15);
}
.ai-send-button {
  width: 40px;
  height: 40px;
  flex: 0 0 auto;
  display: grid;
  place-items: center;
  padding: 0;
  border: 1px solid rgba(79, 216, 239, 0.55);
  border-radius: 6px;
  color: #c9f8ff;
  background: rgba(23, 92, 111, 0.62);
  cursor: pointer;
}
.ai-send-button:hover:not(:disabled) {
  background: rgba(35, 128, 145, 0.78);
  box-shadow: 0 0 16px rgba(79, 216, 239, 0.22);
}
.ai-send-button:disabled {
  opacity: 0.42;
  cursor: not-allowed;
}
.ai-dialog-footer {
  flex: 0 0 auto;
  padding: 0 14px 10px;
  color: #67849c;
  font-size: 10px;
}
.sr-only {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip: rect(0, 0, 0, 0);
  white-space: nowrap;
  border: 0;
}
@media (max-width: 767px) {
  .ai-launcher {
    left: 14px;
    bottom: max(14px, env(safe-area-inset-bottom));
    width: 46px;
    height: 46px;
  }
  .ai-dialog {
    width: calc(100vw - 20px);
    height: min(600px, calc(100dvh - 84px));
    min-height: 350px;
    bottom: max(10px, env(safe-area-inset-bottom));
  }
  .ai-message-list {
    padding: 12px;
  }
  .ai-process-viewport {
    height: 120px;
  }
  .ai-dialog-actions {
    gap: 7px;
  }
}
@media (max-width: 1599px) and (min-width: 1101px) {
  .ai-launcher {
    left: 43px;
  }
}
@media (max-width: 1100px) and (min-width: 768px) {
  .ai-launcher {
    left: 11px;
  }
}
@media (prefers-reduced-motion: reduce) {
  .ai-launcher,
  .ai-panel-enter-active,
  .ai-panel-leave-active {
    transition: none;
  }
  .ai-launcher-status,
  .ai-streaming-dot {
    animation: none;
  }
  .ai-panel-enter-from,
  .ai-panel-leave-to {
    opacity: 1;
    transform: translateX(-50%);
  }
}
</style>
